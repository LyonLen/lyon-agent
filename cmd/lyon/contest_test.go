package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"lyon-agent/internal/mockapi"
	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/eval"
	"lyon-agent/pkg/routing"
	"lyon-agent/pkg/tools"
)

// pollUntil spins until cond is true or the deadline passes.
func pollUntil(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// TestSlashPopupCarriesContestAndAgents: #53's commands are reachable
// from the categorized popup — both filed under the evolve category.
func TestSlashPopupCarriesContestAndAgents(t *testing.T) {
	lookup := func(cmd string) (desc, cat string) {
		for _, c := range slashCommands {
			if c.cmd == cmd {
				return c.desc, c.cat
			}
		}
		return "", ""
	}
	for _, cmd := range []string{"/contest", "/agents"} {
		desc, cat := lookup(cmd)
		if desc == "" {
			t.Fatalf("%s missing from the slash popup", cmd)
		}
		if cat != "evolve" {
			t.Fatalf("%s filed under %q, want evolve", cmd, cat)
		}
	}
}

// TestSendPromptConsumesRoutingTable: the TUI's design §5 consumption —
// a confident routing entry steers the prompt to its model at submit,
// the transcript announces the swap with the ⇄ line, and the badge
// follows the model that actually runs (#53).
func TestSendPromptConsumesRoutingTable(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))
	t.Setenv("LYON_SCORE", "off")
	mock := mockapi.New(mockapi.Config{Script: []mockapi.Turn{{Text: []string{"mock answer"}}}})
	t.Cleanup(mock.Close)
	t.Setenv("ANTHROPIC_BASE_URL", mock.URL())
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "routing-tui-test-token")

	// Three confident samples for the research class, model different
	// from the session's — routingSwap's gate (n≥3, conf≥0.35) is met.
	rt := routing.Open()
	for i := 0; i < 3; i++ {
		rt.Record("research", "winner-a", "route-target-z", 0.8)
	}

	cfg := agent.DefaultConfig()
	cfg.Model = "main-fixed"
	eng := agent.NewEngine(cfg, nil)
	sess := eng.NewSession(nil)
	t.Cleanup(sess.Close)
	// Nobody pumps tea events here — drain the stream so drive never
	// blocks on a full channel.
	go func() {
		for range sess.Events() {
		}
	}()
	m := newTUIModel(cfg, sess, tools.NewMetrics())
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	m.sendPrompt("research the history of tea ceremonies in detail")

	if m.runModel != "route-target-z" {
		t.Fatalf("runModel = %q, want route-target-z (routing not consumed)", m.runModel)
	}
	if m.curAgent != "research" {
		t.Fatalf("badge class = %q, want research", m.curAgent)
	}
	var swapLine string
	for _, l := range m.lines {
		if strings.Contains(l.raw, "⇄ routing") {
			swapLine = l.raw
			break
		}
	}
	if swapLine == "" {
		t.Fatal("transcript missing the ⇄ routing line")
	}
	if !strings.Contains(swapLine, "route-target-z") || !strings.Contains(swapLine, "research") {
		t.Fatalf("⇄ line does not name the model and class: %q", swapLine)
	}

	// The wire must carry the routed model — the swap is real, not a badge.
	pollUntil(t, "the routed API call", 10*time.Second, func() bool {
		return mock.Calls() >= 1
	})
	var body struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(mock.LastBody(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Model != "route-target-z" {
		t.Fatalf("billed model = %q, want route-target-z", body.Model)
	}
}

// TestStepsDispatchAttribution: concurrent subtasks of a run_subagents
// call each keep their own node — inner executions nest under THEIR
// SubID's task, ends close the right rows, and a failed subtask goes red
// while its sibling finishes clean (#53).
func TestStepsDispatchAttribution(t *testing.T) {
	m := newTestModel(t)
	m.startPromptStep("survey two topics at once")
	m.applyEvent(agent.Event{
		Type: agent.EventToolUseStart, ToolID: "tu1", ToolName: "run_subagents",
		Input: json.RawMessage(`{"tasks":[{"task":"alpha"},{"task":"beta"}]}`),
	})
	m.applyEvent(agent.Event{
		Type: agent.EventSubagentStart, Sub: true, SubID: "d1.1",
		ToolName: "model-a", Detail: "alpha research",
	})
	m.applyEvent(agent.Event{
		Type: agent.EventSubagentStart, Sub: true, SubID: "d1.2",
		ToolName: "model-b", Detail: "beta survey",
	})

	// Lookup helper: steps append as events fold in (and the slice may
	// reallocate), so always re-find by token — never hold pointers
	// across an applyEvent.
	findBySub := func(sub string) *execStep {
		for i := range m.steps {
			if m.steps[i].sub == sub {
				return &m.steps[i]
			}
		}
		return nil
	}
	alpha, beta := findBySub("d1.1"), findBySub("d1.2")
	if alpha == nil || beta == nil {
		t.Fatalf("task nodes not created: sub=%q/%q", "d1.1", "d1.2")
	}
	if alpha.agent != "model-a" || beta.agent != "model-b" {
		t.Fatalf("task badges = %q/%q, want model-a/model-b", alpha.agent, beta.agent)
	}
	if alpha.detail != "alpha research" {
		t.Fatalf("alpha detail = %q", alpha.detail)
	}

	// Two inner executions — each must hang under ITS task's node.
	m.applyEvent(agent.Event{
		Type: agent.EventToolExecStart, ExecID: 101, ToolName: "read_file",
		Sub: true, SubID: "d1.1", Input: json.RawMessage(`{"path":"a.md"}`),
	})
	m.applyEvent(agent.Event{
		Type: agent.EventToolExecStart, ExecID: 202, ToolName: "web_search",
		Sub: true, SubID: "d1.2", Input: json.RawMessage(`{"query":"coffee"}`),
	})
	var execs []*execStep
	for i := range m.steps {
		if m.steps[i].execID != 0 {
			execs = append(execs, &m.steps[i])
		}
	}
	if len(execs) != 2 {
		t.Fatalf("exec steps = %d, want 2", len(execs))
	}
	for _, e := range execs {
		parentSub := "d1.1"
		if e.name == "web_search" {
			parentSub = "d1.2"
		}
		parent := findBySub(parentSub)
		if parent == nil || e.under != parent.n {
			t.Fatalf("exec %q under=%d, want task %d (%s)", e.name, e.under, parent.n, parent.detail)
		}
	}

	// alpha ends clean; beta ends failed — the right rows change state.
	m.applyEvent(agent.Event{
		Type: agent.EventSubagentEnd, Sub: true, SubID: "d1.1",
		ToolName: "model-a", Detail: "3 API calls", DurationMS: 1200,
	})
	m.applyEvent(agent.Event{
		Type: agent.EventSubagentEnd, Sub: true, SubID: "d1.2",
		ToolName: "model-b", IsError: true, Err: "gateway timeout", DurationMS: 50,
	})
	alpha, beta = findBySub("d1.1"), findBySub("d1.2")
	if alpha == nil || alpha.status != stepDone || !strings.Contains(alpha.detail, "3 API calls") {
		t.Fatalf("alpha after end: %+v", alpha)
	}
	if beta == nil || beta.status != stepError || beta.err != "gateway timeout" {
		t.Fatalf("beta after failed end: %+v", beta)
	}
	// The parent run_subagents row is untouched by either end.
	parent := m.findToolStep("run_subagents")
	if parent == nil || parent.status != stepRunning {
		t.Fatalf("parent run_subagents row lost: %+v", parent)
	}
}

// TestAgentsCommandLifecycle: /agents curates the field — add writes a
// profile, bare lists entrants and seats, rm removes, and the built-in
// "self" incumbent rejects deletion.
func TestAgentsCommandLifecycle(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))
	m := newTestModel(t)

	m.cmdOut = nil
	m.agentsCommand("")
	if got := m.cmdOutText(); !strings.Contains(got, "self") ||
		!strings.Contains(got, "candidates") {
		t.Fatalf("bare /agents listing:\n%s", got)
	}

	m.cmdOut = nil
	m.agentsCommand("add speedy model-s high")
	cands := eval.LoadCandidates()
	if len(cands) != 1 || cands[0].ID != "speedy" || cands[0].Model != "model-s" ||
		cands[0].Effort != "high" {
		t.Fatalf("after add: %+v", cands)
	}

	m.cmdOut = nil
	m.agentsCommand("rm speedy")
	if n := len(eval.LoadCandidates()); n != 0 {
		t.Fatalf("after rm: %d candidates, want 0", n)
	}

	m.cmdOut = nil
	m.agentsCommand("rm self")
	if got := m.cmdOutText(); !strings.Contains(got, "running model") &&
		!strings.Contains(got, "not a file-backed") {
		t.Fatalf("rm self must refuse with the builtin's reason:\n%s", got)
	}

	m.cmdOut = nil
	m.agentsCommand("frobnicate")
	if got := m.cmdOutText(); !strings.Contains(got, "usage: /agents") {
		t.Fatalf("unknown subcommand must show usage:\n%s", got)
	}
}

// TestRunContestElectsWinnerAndRoutesTraffic: the whole #53 loop against
// a mock gateway — the field (filed challenger + self incumbent) answers
// the held-out bank concurrently, every card files, the election picks
// the higher mean (tie → lexicographic), and routing starts steering the
// class to the winner's model.
func TestRunContestElectsWinnerAndRoutesTraffic(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))

	// Seed the bank and build the canned answer: a reply containing every
	// expected substring passes every question.
	ex, err := eval.LoadOrSeedExam("research")
	if err != nil {
		t.Fatal(err)
	}
	var exps []string
	for _, q := range ex.Questions {
		exps = append(exps, q.Expected)
	}
	canned := strings.Join(exps, " | ")

	mock := mockapi.New(mockapi.Config{Script: []mockapi.Turn{{Text: []string{canned}}}})
	t.Cleanup(mock.Close)
	t.Setenv("ANTHROPIC_BASE_URL", mock.URL())
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "contest-test-token")

	eng := agent.NewEngine(agent.Config{
		Model: "main-x", SubagentModel: "sub-x", EventBuffer: 64,
	}, nil)
	sess := eng.NewSession(nil)
	t.Cleanup(sess.Close)
	go func() {
		for range sess.Events() {
		}
	}()

	// A filed challenger joins the incumbent.
	if err := eval.SaveCandidate(eval.Candidate{ID: "alpha", Model: "model-a"}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rows, winner, err := runContest(ctx, sess, "self-model-x", "research")
	if err != nil {
		t.Fatalf("runContest: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (alpha + self)", len(rows))
	}
	for _, r := range rows {
		if r.Pct != 1.0 || !r.Passed {
			t.Fatalf("candidate %s graded %v (%.2f) — canned answer must pass all", r.ID, r.Passed, r.Pct)
		}
	}
	// Both at 1.0 → deterministic tie-break on the id.
	if winner != "alpha" {
		t.Fatalf("winner = %q, want alpha (tie-break)", winner)
	}

	if got := eval.OpenLeaderboard().OnDuty("research"); got != "alpha" {
		t.Fatalf("on-duty = %q, want alpha", got)
	}
	e, ok := routing.Open().Lookup("research")
	if !ok || e.Agent != "alpha" || e.Model != "model-a" {
		t.Fatalf("routing entry = %+v ok=%v, want alpha/model-a", e, ok)
	}
}
