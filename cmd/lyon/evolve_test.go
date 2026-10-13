package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"lyon-agent/internal/mockapi"
	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/eval"
	"lyon-agent/pkg/flow"
	"lyon-agent/pkg/routing"
)

// mockSession builds a session against a scriptable mock API — every
// Ask/Submit consumes the next scripted turn (the last repeats).
func mockSession(t *testing.T, script []mockapi.Turn) *agent.Session {
	t.Helper()
	mock := mockapi.New(mockapi.Config{Script: script, Latency: 2 * time.Millisecond})
	t.Cleanup(mock.Close)
	t.Setenv("ANTHROPIC_BASE_URL", mock.URL())
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "evolve-test-token")
	eng := agent.NewEngine(agent.DefaultConfig(), nil)
	sess := eng.NewSession(nil)
	t.Cleanup(sess.Close)
	return sess
}

// mockTUI is newTestModel wired to the mock API (engine must be built
// after the env swap).
func mockTUI(t *testing.T, script []mockapi.Turn) *tuiModel {
	t.Helper()
	mock := mockapi.New(mockapi.Config{Script: script, Latency: 2 * time.Millisecond})
	t.Cleanup(mock.Close)
	t.Setenv("ANTHROPIC_BASE_URL", mock.URL())
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "evolve-test-token")
	return newTestModel(t)
}

// TestShowRoute: bare renders the evidence table shell (empty home = the
// "no scores yet" hint); with text it classifies and reaches the
// workflow matcher.
func TestShowRoute(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))
	m := newTestModel(t)

	m.showRoute("")
	out := m.cmdOutText()
	if !strings.Contains(out, "routing") || !strings.Contains(out, "no scores yet") {
		t.Fatalf("bare /route output:\n%s", out)
	}
	if !strings.Contains(out, "model:") {
		t.Fatalf("bare /route lacks the model profile line:\n%s", out)
	}

	m.cmdOut = nil
	m.showRoute("implement a new search feature")
	out = m.cmdOutText()
	if !strings.Contains(out, "class: ") {
		t.Fatalf("classified output lacks class line:\n%s", out)
	}
	if !strings.Contains(out, "workflow: feature-dev") {
		t.Fatalf("expected feature-dev match:\n%s", out)
	}
}

// TestFlowCommands: list/show/match/export answer offline from the
// shipped set; save without prompts refuses before any model call.
func TestFlowCommands(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))
	m := newTestModel(t)

	m.cmdOut = nil
	m.flowCommand("list")
	out := m.cmdOutText()
	if !strings.Contains(out, "feature-dev") || !strings.Contains(out, "builtin") {
		t.Fatalf("flow list output:\n%s", out)
	}

	m.cmdOut = nil
	m.flowCommand("show feature-dev")
	out = m.cmdOutText()
	if !strings.Contains(out, "exit:") || !strings.Contains(out, "clarify") {
		t.Fatalf("flow show output:\n%s", out)
	}

	m.cmdOut = nil
	m.flowCommand("match add a dark mode feature")
	out = stripANSI(m.cmdOutText())
	if !strings.Contains(out, "match → feature-dev") {
		t.Fatalf("flow match output:\n%s", out)
	}

	m.cmdOut = nil
	m.flowCommand("match purple elephant xylophone quartet")
	out = stripANSI(m.cmdOutText())
	if !strings.Contains(out, "no workflow matches") {
		t.Fatalf("empty match output:\n%s", out)
	}

	m.cmdOut = nil
	m.flowCommand("show ghost")
	if got := m.cmdOutText(); !strings.Contains(got, "no workflow ghost") {
		t.Fatalf("missing-slug output:\n%s", got)
	}

	m.cmdOut = nil
	m.flowCommand("save")
	if got := m.cmdOutText(); !strings.Contains(got, "usage") {
		t.Fatalf("bare save must show usage:\n%s", got)
	}

	// No prompt rows yet — refuses before any network leg.
	m.cmdOut = nil
	m.flowCommand("save myflow")
	if got := m.cmdOutText(); !strings.Contains(got, "no prompts in this session") {
		t.Fatalf("save without prompts:\n%s", got)
	}
}

// TestExamDoneHandler: a landed exam files the score, echoes the
// summary and misses, and two consecutive failures by the sitting agent
// demote them (seat open).
func TestExamDoneHandler(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))
	m := newTestModel(t)

	step := m.pushAsyncStep(stepEval, "exam.coding", "round 10/10 — exam due…")
	pass := eval.ExamResult{
		Class: "coding", Pct: 0.75, Passed: true,
		Answers: []eval.ExamAnswer{
			{Q: "q1", OK: true}, {Q: "q2", OK: true},
			{Q: "q3", OK: true}, {Q: "q4", OK: false},
		},
	}
	m.Update(examDone{class: "coding", why: "round 10/10 — exam due",
		res: pass, step: step})
	if got := m.stepBy(step); got == nil || got.status != stepDone ||
		!strings.Contains(got.detail, "PASS") {
		t.Fatalf("exam row after pass: %+v", got)
	}
	out := m.cmdOutText()
	if !strings.Contains(out, "· exam") || !strings.Contains(out, "✗ q4") {
		t.Fatalf("exam report output:\n%s", out)
	}
	// The score is on file: leaderboard exam card + routing evidence.
	lb := eval.OpenLeaderboard()
	if onDuty, _, _ := lb.Status("coding"); onDuty != m.cfg.Model {
		t.Fatalf("on-duty after pass = %q, want %q", onDuty, m.cfg.Model)
	}
	if e, ok := routing.Open().Lookup("coding"); !ok || e.Samples == 0 {
		t.Fatalf("routing entry not recorded: %+v ok=%v", e, ok)
	}

	// Error leg closes the row as failed.
	step = m.pushAsyncStep(stepEval, "exam.coding", "manual…")
	m.Update(examDone{class: "coding", why: "manual", err: errTest("model down"), step: step})
	if got := m.stepBy(step); got == nil || got.status != stepError ||
		!strings.Contains(got.err, "model down") {
		t.Fatalf("exam row after error: %+v", got)
	}

	// Two failures in a row demote the sitting agent.
	fail := eval.ExamResult{Class: "coding", Pct: 0.25, Passed: false,
		Answers: []eval.ExamAnswer{{Q: "q1", OK: false}}}
	m.cmdOut = nil
	m.Update(examDone{class: "coding", why: "manual", res: fail,
		step: m.pushAsyncStep(stepEval, "exam.coding", "manual…")})
	if strings.Contains(m.cmdOutText(), "demoted") {
		t.Fatalf("first failure must not demote:\n%s", m.cmdOutText())
	}
	m.cmdOut = nil
	m.Update(examDone{class: "coding", why: "manual", res: fail,
		step: m.pushAsyncStep(stepEval, "exam.coding", "manual…")})
	if !strings.Contains(m.cmdOutText(), "demoted") {
		t.Fatalf("second failure must demote:\n%s", m.cmdOutText())
	}
	if onDuty, _, _ := eval.OpenLeaderboard().Status("coding"); onDuty != "" {
		t.Fatalf("seat after demotion = %q, want open", onDuty)
	}
}

// TestCountRoundClock: every successful prompt books a round; at ten the
// exam kicks (panel row opens) — LYON_EXAM=off still books the round
// but never fires.
func TestCountRoundClock(t *testing.T) {
	dir := tempDir(t)
	t.Setenv("LYON_HOME", dir)
	m := mockTUI(t, nil) // empty script: if the exam goroutine runs, it gets "ok"
	m.hist = append(m.hist, "fix the rollback bug")

	// First round: books, no exam (due needs 10 + a sitting agent).
	m.countRound()
	if row := m.stepByKind(stepEval); row != nil {
		t.Fatalf("round 1 must not open an exam row: %+v", row)
	}

	// Seat an agent and advance the clock to 9.
	lb := eval.OpenLeaderboard()
	lb.Record("fix", eval.Scorecard{Agent: m.cfg.Model, Total: 0.9, Passed: true})
	for i := 0; i < 8; i++ {
		lb.CountRound("fix")
	}

	// Off switch: books round 10, no trigger.
	t.Setenv("LYON_EXAM", "off")
	m.countRound()
	if row := m.stepByKind(stepEval); row != nil {
		t.Fatalf("LYON_EXAM=off must not open a row: %+v", row)
	}
	if _, rounds, _ := eval.OpenLeaderboard().Status("fix"); rounds != 10 {
		t.Fatalf("rounds = %d, want 10", rounds)
	}

	// Armed: round 11 isn't the interval — arm to 9 again and fire.
	lb = eval.OpenLeaderboard()
	// CountRound increments past 10; rewind by reopening at 19 → next is 20.
	for i := 0; i < 9; i++ {
		lb.CountRound("fix") // 11..19
	}
	t.Setenv("LYON_EXAM", "")
	m.countRound() // 20 → due → kick
	row := m.stepByKind(stepEval)
	if row == nil || !strings.HasPrefix(row.name, "exam.") {
		t.Fatalf("due round must open an exam row: %+v", row)
	}
	// The goroutine reads LYON_HOME while this test owns the env — wait
	// for its seed write, which happens after the last env read.
	deadline := time.Now().Add(5 * time.Second)
	seedPath := filepath.Join(dir, "exams", "fix.json")
	for {
		if _, err := os.Stat(seedPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("exam goroutine never seeded %s", seedPath)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// stepByN finds a row by its step number — stepByKind returns the first
// of a kind, which collides once two score rows coexist.
func stepByN(m *tuiModel, n int) *execStep {
	for i := range m.steps {
		if m.steps[i].n == n {
			return &m.steps[i]
		}
	}
	return nil
}

// TestScoreDoneClosesStep: the async grade lands as a closed stepEval
// row plus a score line in the answer area; a failed grade says why
// instead of pretending a number exists.
func TestScoreDoneClosesStep(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))
	m := newTestModel(t)

	step := m.pushAsyncStep(stepEval, "score", "blind judge grading this run…")
	m.Update(scoreDone{step: step, line: "score 0.93 (judge 85%) → leaderboard [coding]"})
	if row := stepByN(m, step); row == nil || row.status != stepDone ||
		!strings.Contains(row.detail, "leaderboard [coding]") {
		t.Fatalf("grade must close its row with the line: %+v", row)
	}
	out := m.cmdOutText()
	if !strings.Contains(out, "· score") || !strings.Contains(out, "0.93") {
		t.Fatalf("score line missing from the answer area:\n%s", out)
	}

	// Failure leg: honest error on the row, in the answer area.
	m.cmdOut = nil
	step2 := m.pushAsyncStep(stepEval, "score", "blind judge grading this run…")
	m.Update(scoreDone{step: step2, err: errors.New("grade: no number in judge reply")})
	if row := stepByN(m, step2); row == nil || row.status != stepError ||
		!strings.Contains(row.err, "no number") {
		t.Fatalf("failed grade must mark its row: %+v", row)
	}
	if out := m.cmdOutText(); !strings.Contains(out, "no number") {
		t.Fatalf("failed grade must explain itself:\n%s", out)
	}
	// The success row is untouched by its sibling's failure.
	if row := stepByN(m, step); row == nil || row.status != stepDone {
		t.Fatalf("earlier row mutated: %+v", row)
	}
}

// TestKickScoreFilesCard: with the guard open, kickScore grades off the
// event stream — the card reaches the leaderboard and the routing table
// even though no program is around to receive scoreDone (the filing
// lives inside the goroutine's bookPlainRun).
func TestKickScoreFilesCard(t *testing.T) {
	dir := tempDir(t)
	t.Setenv("LYON_HOME", dir)
	m := mockTUI(t, []mockapi.Turn{{Text: []string{"0.85"}}})
	t.Setenv("LYON_SCORE", "on") // newTestModel parks tests at off
	m.hist = append(m.hist, "implement a login form")
	m.kickScore(3)

	class := string(routing.Classify("implement a login form"))
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, ok := eval.OpenLeaderboard().Classes[class]; ok &&
			len(b.Agents[m.cfg.Model]) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("judge goroutine never filed a card for %q", class)
		}
		time.Sleep(25 * time.Millisecond)
	}
	b := eval.OpenLeaderboard().Classes[class]
	hist := b.Agents[m.cfg.Model]
	sc := hist[len(hist)-1] // appended chronologically: newest last
	if sc.Source != "production" {
		t.Errorf("source = %q, want production", sc.Source)
	}
	if sc.Dims.Correctness != 0.85 {
		t.Errorf("correctness = %v, want 0.85 (the judge's number)", sc.Dims.Correctness)
	}
	// No baseline/budget → the other three dims pass at 1.0:
	// total = 0.5·0.85 + 0.2 + 0.15 + 0.15 = 0.925.
	if sc.Total != 0.925 {
		t.Errorf("total = %v, want 0.925", sc.Total)
	}
	if !sc.Passed {
		t.Errorf("0.85 judge must clear the 0.80 threshold: %+v", sc)
	}
	if e, ok := routing.Open().Lookup(class); !ok || e.MeanScore != sc.Total {
		t.Errorf("routing evidence missing: ok=%v entry=%+v", ok, e)
	}
}

// TestScorePlainCLIClock: the CLI leg grades the same way and always
// ticks the exam clock — even with the judge switched off, the round
// still counts (the LYON_EXAM split mirrored).
func TestScorePlainCLIClock(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))
	t.Setenv("LYON_SCORE", "off")
	sess := mockSession(t, nil)
	const prompt = "write a changelog entry for the release"
	class := string(routing.Classify(prompt))

	scorePlainCLI(sess, "claude-opus-5-5", prompt)
	if _, rounds, _ := eval.OpenLeaderboard().Status(class); rounds != 1 {
		t.Fatalf("rounds = %d, want 1 (the clock ticks with the judge off)", rounds)
	}
	// Judge off → no card filed.
	if b, ok := eval.OpenLeaderboard().Classes[class]; ok && len(b.Agents) > 0 {
		t.Fatalf("LYON_SCORE=off must not file cards: %+v", b.Agents)
	}
}

// TestLastAssistantText: the judge sees the newest assistant prose,
// skipping tool-only turns and empty blocks; no prose anywhere is "".
func TestLastAssistantText(t *testing.T) {
	text := func(role anthropic.BetaMessageParamRole, parts ...string) anthropic.BetaMessageParam {
		m := anthropic.BetaMessageParam{Role: role}
		for _, p := range parts {
			m.Content = append(m.Content, anthropic.BetaContentBlockParamUnion{
				OfText: &anthropic.BetaTextBlockParam{Text: p},
			})
		}
		return m
	}
	msgs := []anthropic.BetaMessageParam{
		text(anthropic.BetaMessageParamRoleUser, "question"),
		text(anthropic.BetaMessageParamRoleAssistant), // tool-only: no blocks
		text(anthropic.BetaMessageParamRoleAssistant, "first answer"),
		text(anthropic.BetaMessageParamRoleAssistant, "line one", "line two"),
	}
	if got := lastAssistantText(msgs); got != "line one\nline two" {
		t.Fatalf("got %q, want the newest assistant's blocks joined", got)
	}
	// Empty/whitespace text must not shadow real prose underneath.
	msgs = []anthropic.BetaMessageParam{
		text(anthropic.BetaMessageParamRoleAssistant, "the real answer"),
		text(anthropic.BetaMessageParamRoleAssistant, "   "),
	}
	if got := lastAssistantText(msgs); got != "the real answer" {
		t.Fatalf("got %q, want the older real answer", got)
	}

	// User-only history yields nothing to judge.
	msgs = []anthropic.BetaMessageParam{
		text(anthropic.BetaMessageParamRoleUser, "q"),
	}
	if got := lastAssistantText(msgs); got != "" {
		t.Fatalf("user-only history = %q, want empty", got)
	}
}

// TestExamCommandUsage: bare with no history points at usage; with
// history it classifies and opens the row; an explicit class wins.
func TestExamCommandUsage(t *testing.T) {
	m := mockTUI(t, nil)
	m.examCommand("")
	if got := m.cmdOutText(); !strings.Contains(got, "usage: /exam") {
		t.Fatalf("bare /exam without history:\n%s", got)
	}

	m.examCommand("coding")
	row := m.stepByKind(stepEval)
	if row == nil || row.name != "exam.coding" {
		t.Fatalf("explicit class row: %+v", row)
	}
	m.finishStep(row.n, stepDone, "settled", "")

	m.hist = append(m.hist, "write the release notes changelog")
	m.examCommand("")
	row = m.stepByKind(stepEval)
	if row == nil || !strings.HasPrefix(row.name, "exam.") {
		t.Fatalf("classified row: %+v", row)
	}
	m.finishStep(row.n, stepDone, "settled", "")
}

// TestRunExamCLI: a passing answer set scores 0 and files the card;
// all-miss answers score 1; an unknown class is a setup error. No
// leaderboard leakage — temp home, all synchronous.
func TestRunExamCLI(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))

	// Correct answers for the four shipped coding questions, in order.
	passSess := mockSession(t, []mockapi.Turn{
		{Text: []string{"use go test ./... to run every package"}},
		{Text: []string{"deferred calls run just before the surrounding function returns"}},
		{Text: []string{"the commit is git's atomic unit"}},
		{Text: []string{"gofmt formats source files in place"}},
	})
	if code := runExamCLI(passSess, "claude-opus-5-5", "coding"); code != 0 {
		t.Fatalf("passing exam code = %d, want 0", code)
	}
	if onDuty, _, _ := eval.OpenLeaderboard().Status("coding"); onDuty != "claude-opus-5-5" {
		t.Fatalf("on-duty after CLI exam = %q", onDuty)
	}
	if e, ok := routing.Open().Lookup("coding"); !ok || e.Samples == 0 {
		t.Fatalf("routing evidence missing after CLI exam")
	}

	// Misses: the repeated "ok" answer fails every item.
	failSess := mockSession(t, []mockapi.Turn{{Text: []string{"ok"}}})
	if code := runExamCLI(failSess, "claude-opus-5-5", "coding"); code != 1 {
		t.Fatalf("failing exam code = %d, want 1", code)
	}

	if code := runExamCLI(passSess, "m", "no-such-class"); code != 2 {
		t.Fatalf("unknown class code = %d, want 2", code)
	}
}

// TestGatePromptCLI: a blind miss injects the gap lines into the prompt;
// a hit passes untouched; a broken checklist degrades to a note.
func TestGatePromptCLI(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))

	missSess := mockSession(t, []mockapi.Turn{
		{Text: []string{`[{"q":"What does X return on expiry?","expected":"42"}]`}},
		{Text: []string{`{"What does X return on expiry?":"unknown"}`}},
	})
	got := gatePromptCLI(missSess, "fix login")
	if !strings.Contains(got, "Knowledge gaps") ||
		!strings.Contains(got, "What does X return on expiry?") {
		t.Fatalf("missed gate must inject, got:\n%s", got)
	}

	hitSess := mockSession(t, []mockapi.Turn{
		{Text: []string{`[{"q":"What does X return on expiry?","expected":"42"}]`}},
		{Text: []string{`{"What does X return on expiry?":"42"}`}},
	})
	if got := gatePromptCLI(hitSess, "fix login"); got != "fix login" {
		t.Fatalf("passing gate must not touch the prompt:\n%s", got)
	}

	brokenSess := mockSession(t, []mockapi.Turn{
		{Text: []string{"this is not a checklist"}},
	})
	if got := gatePromptCLI(brokenSess, "fix login"); got != "fix login" {
		t.Fatalf("broken gate must send the prompt untouched:\n%s", got)
	}
}

// TestRunFlowCLI: a user workflow end to end — gate quiz, node fills
// through the real session loop, a shell command exit, then the score
// lands in the leaderboard + routing table. The failing leg scores a
// partial run and exits non-zero.
func TestRunFlowCLI(t *testing.T) {
	dir := tempDir(t)
	t.Setenv("LYON_HOME", dir)
	t.Setenv("LYON_FLOW_GATE", "on")

	wfDir := filepath.Join(dir, "workflows")
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	wfPath := filepath.Join(wfDir, "shipit.json")
	wf := flow.Workflow{
		Slug:  "shipit",
		Title: "ship it",
		Nodes: []flow.Node{
			{Name: "build", Prompt: "build the thing: {{input}}",
				Exit: flow.Exit{Kind: flow.ExitContains, Contains: "BUILT:"}},
			{Name: "verify", Prompt: "verify: {{input}}",
				Exit: flow.Exit{Kind: flow.ExitCommand, Command: "echo probe-ok"}},
		},
	}
	data, _ := json.Marshal(wf)
	if err := os.WriteFile(wfPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	sess := mockSession(t, []mockapi.Turn{
		// Gate: checklist, then blind answers (hit → no injection).
		{Text: []string{`[{"q":"What is built first?","expected":"the shell"}]`}},
		{Text: []string{`{"What is built first?":"the shell"}`}},
		// Node fill: carries the exit marker.
		{Text: []string{"BUILT: the artifact is ready"}},
	})
	cfg := agent.DefaultConfig()
	cfg.Workspace = tempDir(t)
	if code := runFlowCLI(sess, cfg, "shipit", "the new widget"); code != 0 {
		t.Fatalf("green flow code = %d, want 0", code)
	}
	// Scored: a routing entry exists with samples on file (the exact
	// class comes from Classify — any class carrying evidence counts).
	entries := routing.Open().All()
	found := false
	for _, e := range entries {
		if e.Samples > 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("routing samples not recorded: %+v", entries)
	}

	// Failing leg: gate off (script starts at the node), marker missing,
	// retries exhaust → non-zero exit, partial score on file.
	t.Setenv("LYON_FLOW_GATE", "off")
	wf2 := wf
	wf2.Slug = "shipit-miss"
	wf2.Nodes = []flow.Node{
		{Name: "build", Prompt: "build: {{input}}",
			Exit: flow.Exit{Kind: flow.ExitContains, Contains: "NEVER-MARKER:"}},
	}
	data2, _ := json.Marshal(wf2)
	if err := os.WriteFile(filepath.Join(wfDir, "shipit-miss.json"), data2, 0o644); err != nil {
		t.Fatal(err)
	}
	missSess := mockSession(t, []mockapi.Turn{
		{Text: []string{"half done, no marker"}},
	})
	if code := runFlowCLI(missSess, cfg, "shipit-miss", "the new widget"); code != 1 {
		t.Fatalf("red flow code = %d, want 1", code)
	}

	// Unknown slug is a setup error.
	if code := runFlowCLI(sess, cfg, "ghost", "x"); code != 2 {
		t.Fatalf("ghost slug code = %d, want 2", code)
	}
}

// TestRoutingSwap: the consumption side of design §5 — an unpinned run
// adopts the routing table's confident on-duty model for its class, and
// refuses to be steered by an unproven seat.
func TestRoutingSwap(t *testing.T) {
	home := tempDir(t)
	t.Setenv("LYON_HOME", home)
	write := func(entries string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(home, "routing.json"),
			[]byte(`{"entries":`+entries+`}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	prompt := "修复 calc.Sum 少加最后一个元素的 bug"

	// No table at all: run on the current model.
	if m, _, ok := routingSwap("mimo-v2.6-flash", prompt); ok || m != "mimo-v2.6-flash" {
		t.Fatalf("empty table: swap = %v %q, want no swap", ok, m)
	}

	// A confident fix seat takes the run.
	write(`{"fix":{"class":"fix","agent":"mimo-v2.6-pro","model":"mimo-v2.6-pro",` +
		`"confidence":0.62,"samples":5,"mean_score":0.83}}`)
	m, e, ok := routingSwap("mimo-v2.6-flash", prompt)
	if !ok || m != "mimo-v2.6-pro" || e.Class != "fix" {
		t.Fatalf("confident seat: swap = %v %q (%+v), want mimo-v2.6-pro [fix]", ok, m, e)
	}

	// Already on the recommended model: nothing to do.
	if _, _, ok := routingSwap("mimo-v2.6-pro", prompt); ok {
		t.Fatal("current model already on duty — no swap expected")
	}

	// An unproven seat (too few samples / low confidence) never steers.
	write(`{"fix":{"class":"fix","agent":"mimo-v2.5-pro","model":"mimo-v2.5-pro",` +
		`"confidence":0.34,"samples":2,"mean_score":0.85}}`)
	if m, _, ok := routingSwap("mimo-v2.6-flash", prompt); ok || m != "mimo-v2.6-flash" {
		t.Fatalf("weak seat: swap = %v %q, want no swap", ok, m)
	}

	// A class with no seat falls back to the current model.
	if m, _, ok := routingSwap("mimo-v2.6-flash", "写一篇关于路由的文章"); ok || m != "mimo-v2.6-flash" {
		t.Fatalf("seatless class: swap = %v %q, want no swap", ok, m)
	}
}
