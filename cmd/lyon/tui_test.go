package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	tea "github.com/charmbracelet/bubbletea"

	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/tools"
)

// tempDir is t.TempDir with a deletion retry: these tests churn atomic
// tmp+rename writes (snapshot store), and on Windows the cleanup
// occasionally races a transient handle (defender/indexer) in %TEMP% —
// RemoveAll fails with "directory not empty" right after emptying it.
// The retry runs before testing's own cleanup (LIFO), turning the
// package-level flake green.
func tempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() {
		for i := 0; i < 20; i++ {
			if os.RemoveAll(dir) == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	})
	return dir
}

// newTestModel builds a TUI model around a session that never prompts:
// events are injected directly, so no API call is involved.
func newTestModel(t *testing.T) *tuiModel {
	t.Helper()
	// Injection tests reach prompt_end without ever prompting; scoring
	// must not take that as license to judge — tests that want it
	// enable it deliberately.
	t.Setenv("LYON_SCORE", "off")
	cfg := agent.DefaultConfig()
	eng := agent.NewEngine(cfg, nil)
	sess := eng.NewSession(nil)
	t.Cleanup(sess.Close)
	m := newTUIModel(cfg, sess, tools.NewMetrics())
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return m
}

// cmdOutText joins the temporary command-output area — help text, tables
// and action feedback render there now (pushCmd), deliberately outside
// the chat transcript.
func (m *tuiModel) cmdOutText() string {
	return strings.Join(m.cmdOut, "\n")
}

// TestPromptEndBudgetNotice: a clean prompt_end that still needed history
// repair is MaxIterations cutting the loop before the last turn's tools
// ran — the transcript says how to continue, and the pending step is
// labeled "over budget" instead of a vague "unfinished".
func TestPromptEndBudgetNotice(t *testing.T) {
	t.Setenv("LYON_HOME", t.TempDir())
	m := newTestModel(t)
	m.startPromptStep("long refactor across the repo")
	m.applyEvent(agent.Event{
		Type: agent.EventToolUseStart, ToolID: "t1", ToolName: "run_shell",
		Input: json.RawMessage(`{"command":"go test ./..."}`),
	})
	m.applyEvent(agent.Event{Type: agent.EventPromptEnd,
		StopReason: "max_iterations", Iterations: 50})

	if out := stripANSI(m.renderTranscript()); !strings.Contains(out, "budget reached") ||
		!strings.Contains(out, "LYON_MAX_ITER") {
		t.Fatalf("budget notice missing:\n%s", out)
	}
	row := m.stepByKind(stepTool)
	if row == nil || row.status != stepError || row.err != "over budget" {
		t.Fatalf("pending step after budget cut: %+v", row)
	}

	// A normal end still reads as a plain done.
	m.applyEvent(agent.Event{Type: agent.EventPromptEnd, Iterations: 3})
	if out := stripANSI(m.renderTranscript()); !strings.Contains(out, "✓ done") {
		t.Fatalf("normal end missing done line:\n%s", out)
	}
	if rows := strings.Count(stripANSI(m.renderTranscript()), "budget reached"); rows != 1 {
		t.Fatalf("normal end must not repeat the budget notice (%d)", rows)
	}
}

// TestResumeRestoresStepsAndTurns: the audit panel and the [turn N]
// accounting lines ride into the snapshot — /resume brings both back
// (durations included), and a row that was still running when the save
// interrupted the prompt closes as interrupted, not as a phantom spinner.
func TestResumeRestoresStepsAndTurns(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t)) // keep the store out of ~/.lyon
	cfg := agent.DefaultConfig()
	cfg.Workspace = tempDir(t)
	eng := agent.NewEngine(cfg, nil)
	sess := eng.NewSession(nil)
	t.Cleanup(sess.Close)
	m := newTUIModel(cfg, sess, tools.NewMetrics())
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})

	now := time.Now()
	snap := agent.Snapshot{
		Version:   1,
		Workspace: cfg.Workspace,
		Model:     cfg.Model,
		SavedAt:   time.Date(2026, 10, 12, 9, 15, 0, 0, time.Local),
		Messages: []anthropic.BetaMessageParam{
			{Role: anthropic.BetaMessageParamRoleUser, Content: []anthropic.BetaContentBlockParamUnion{
				{OfText: &anthropic.BetaTextBlockParam{Text: "fix the widget"}},
			}},
			{Role: anthropic.BetaMessageParamRoleAssistant, Content: []anthropic.BetaContentBlockParamUnion{
				{OfText: &anthropic.BetaTextBlockParam{Text: "widget fixed"}},
			}},
		},
		Turns: []agent.TurnStat{{
			Turn: 1, StopReason: "end_turn",
			Usage:    agent.Usage{Input: 42, Output: 7, CacheRead: 900},
			Duration: 1500 * time.Millisecond,
		}},
		Steps: stepsState([]execStep{
			{n: 1, kind: stepPrompt, detail: "fix the widget", status: stepDone, ts: now},
			{n: 2, kind: stepTool, name: "run_shell", detail: "go test ./...", status: stepDone, durMS: 1234, ts: now},
			{n: 3, kind: stepTool, name: "edit_file", status: stepRunning, ts: now},
		}),
	}
	if err := agent.SaveSnapshot(snap); err != nil {
		t.Fatal(err)
	}
	m.slash("/resume")

	// Transcript: the turn marker replays with its usage accounting.
	got := m.renderTranscript()
	for _, want := range []string{"[turn 1]", "in=42 out=7", "cache_read=900", "stop=end_turn", "1500ms"} {
		if !strings.Contains(got, want) {
			t.Fatalf("replay missing turn marker part %q:\n%s", want, got)
		}
	}

	// Panel: all three rows restored with durations; the running row was
	// captured as interrupted; numbering resumes for new steps.
	if len(m.steps) != 3 {
		t.Fatalf("restored steps = %d, want 3", len(m.steps))
	}
	if s := m.steps[0]; s.kind != stepPrompt || s.detail != "fix the widget" {
		t.Fatalf("prompt row: %+v", s)
	}
	if s := m.steps[1]; s.name != "run_shell" || s.status != stepDone || s.durMS != 1234 {
		t.Fatalf("tool row: %+v", s)
	}
	if s := m.steps[2]; s.status != stepError || s.err != "interrupted" {
		t.Fatalf("running row at save must restore as interrupted: %+v", s)
	}
	if m.stepSeq != 3 {
		t.Fatalf("stepSeq = %d, want 3 (resume past restored rows)", m.stepSeq)
	}
	m.addStep(stepTool, "", "run_shell", "echo next")
	if got := m.steps[len(m.steps)-1].n; got != 4 {
		t.Fatalf("new step n = %d, want 4", got)
	}
}

// TestStepsStateRoundTrip: capture → load preserves every field, running
// rows close at capture time without mutating the live panel, and broken
// or empty blobs are rejected without clobbering the current panel.
func TestStepsStateRoundTrip(t *testing.T) {
	live := []execStep{
		{n: 5, kind: stepEval, name: "exam.coding", detail: "PASS 3/4", status: stepDone, durMS: 9},
		{n: 6, kind: stepTool, name: "run_shell", detail: "sleep 1", status: stepRunning},
	}
	raw := stepsState(live)
	if len(raw) == 0 {
		t.Fatal("stepsState returned nothing for a non-empty panel")
	}
	if live[1].status != stepRunning {
		t.Fatal("stepsState must not mutate the live panel")
	}

	m := newTestModel(t)
	if !m.applySteps(raw) {
		t.Fatal("applySteps rejected a valid capture")
	}
	if len(m.steps) != 2 || m.stepSeq != 6 {
		t.Fatalf("round trip: %d rows, stepSeq=%d", len(m.steps), m.stepSeq)
	}
	if s := m.steps[0]; s.kind != stepEval || s.name != "exam.coding" ||
		s.detail != "PASS 3/4" || s.durMS != 9 {
		t.Fatalf("field loss: %+v", s)
	}
	if s := m.steps[1]; s.status != stepError || s.err != "interrupted" {
		t.Fatalf("running row not closed at capture: %+v", s)
	}

	keep := m.steps
	if m.applySteps(json.RawMessage("{broken")) {
		t.Fatal("broken blob accepted")
	}
	if m.applySteps(nil) {
		t.Fatal("empty blob accepted")
	}
	if len(m.steps) != len(keep) {
		t.Fatal("rejected blobs must leave the panel untouched")
	}
}

// TestTUIStreamRendersTranscript: streaming deltas split on newlines, the
// partial line shows while in flight, and non-text events flush it.
func TestTUIStreamRendersTranscript(t *testing.T) {
	m := newTestModel(t)

	m.applyEvent(agent.Event{Type: agent.EventTextDelta, Text: "Hello\nwor"})
	if got := m.renderTranscript(); !strings.Contains(got, "Hello") {
		t.Fatalf("committed line missing from transcript:\n%s", got)
	}
	if got := m.renderTranscript(); !strings.Contains(got, "wor") {
		t.Fatalf("partial line missing from transcript:\n%s", got)
	}
	m.applyEvent(agent.Event{Type: agent.EventTextDelta, Text: "ld"})
	m.applyEvent(agent.Event{
		Type: agent.EventToolExecStart, ExecID: 7, ToolName: "run_shell",
		Input: json.RawMessage(`{"command":"echo hi"}`),
	})
	got := m.renderTranscript()
	if !strings.Contains(got, "world") {
		t.Fatalf("line not completed by flush:\n%s", got)
	}
	if !strings.Contains(got, "run_shell") {
		t.Fatalf("tool line missing:\n%s", got)
	}
}

// TestTUIPromptEndControlsState: prompt_end flips busy off, interrupted
// ends are labelled as such, and the follow-up prompt line renders.
func TestTUIPromptEndControlsState(t *testing.T) {
	m := newTestModel(t)
	m.busy = true
	m.applyEvent(agent.Event{Type: agent.EventPromptEnd, Err: agent.ErrInterrupted.Error()})
	if m.busy {
		t.Fatal("busy not cleared by prompt_end")
	}
	if got := m.renderTranscript(); !strings.Contains(got, "interrupted") {
		t.Fatalf("interrupt label missing:\n%s", got)
	}

	m.lines = append(m.lines, tline{kind: linePrompt, raw: "hello there"})
	got := m.renderTranscript()
	if !strings.Contains(got, "❯") || !strings.Contains(got, "hello there") {
		t.Fatalf("prompt line missing:\n%s", got)
	}
}

// TestTUIReplayThinkingFolds: thinking restored by /resume joins foldable
// blocks — folded by default with a one-line preview, ctrl+t expands and
// refolds. Replayed lines used to carry blk=0 ("ungrouped"), which
// bypasses the fold path entirely, so ctrl+t changed the status line but
// never the content.
func TestTUIReplayThinkingFolds(t *testing.T) {
	m := newTestModel(t)
	snap := &agent.Snapshot{
		Version: 1,
		SavedAt: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC),
		Messages: []anthropic.BetaMessageParam{{
			Role: anthropic.BetaMessageParamRoleAssistant,
			Content: []anthropic.BetaContentBlockParamUnion{
				{OfThinking: &anthropic.BetaThinkingBlockParam{Thinking: "replay plan alpha\nreplay plan beta"}},
				{OfText: &anthropic.BetaTextBlockParam{Text: "the answer"}},
			},
		}},
	}
	m.replay(snap)
	got := m.renderTranscript()
	if !strings.Contains(got, "▸ thinking") {
		t.Fatalf("fold header missing after replay:\n%s", got)
	}
	if !strings.Contains(got, "replay plan alpha") {
		t.Fatalf("fold preview missing after replay:\n%s", got)
	}
	if strings.Contains(got, "replay plan beta") {
		t.Fatalf("replayed thinking not folded:\n%s", got)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	if got = m.renderTranscript(); !strings.Contains(got, "replay plan beta") {
		t.Fatalf("ctrl+t did not expand replayed thinking:\n%s", got)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	if got = m.renderTranscript(); strings.Contains(got, "replay plan beta") {
		t.Fatalf("ctrl+t did not refold replayed thinking:\n%s", got)
	}
}

// TestTUISlashCommands: /help lists commands, /clear empties the view,
// unknown commands are reported without touching the session.
func TestTUISlashCommands(t *testing.T) {
	m := newTestModel(t)
	m.slash("/help")
	if got := m.cmdOutText(); !strings.Contains(got, "/stats") {
		t.Fatalf("help text missing from output area:\n%s", got)
	}
	if strings.Contains(m.renderTranscript(), "/stats   usage table") {
		t.Fatal("help text leaked into the chat transcript")
	}
	m.slash("/clear")
	if len(m.lines) != 0 {
		t.Fatalf("/clear left %d lines", len(m.lines))
	}
	if m.cmdOut != nil {
		t.Fatalf("/clear left %d output rows", len(m.cmdOut))
	}
	m.slash("/frobnicate")
	if got := m.cmdOutText(); !strings.Contains(got, "unknown command") {
		t.Fatalf("unknown-command feedback missing:\n%s", got)
	}
}

// TestTUIApprovalOverlayDeny: an approval request replaces the input line
// and an "n" keystroke answers false — fail closed.
func TestTUIApprovalOverlayDeny(t *testing.T) {
	m := newTestModel(t)
	req := approveReq{
		ev: tools.ExecEvent{Name: "run_shell", Input: json.RawMessage(`{"command":"rm -rf /"}`)},
		ch: make(chan bool, 1),
	}
	m.Update(req)
	if m.appr == nil {
		t.Fatal("approval not shown")
	}
	if !strings.Contains(m.statusLine(), "approve?") {
		t.Fatalf("status line missing approval prompt: %q", m.statusLine())
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if m.appr != nil {
		t.Fatal("approval overlay not dismissed")
	}
	select {
	case v := <-req.ch:
		if v {
			t.Fatal("deny keystroke answered true")
		}
	default:
		t.Fatal("denial never reached the waiting hook")
	}
}

// TestTUIThinkingFolds: a streaming thinking block shows its body, folds to
// a one-line header once the next non-thinking line closes it, and the
// global toggle (ctrl+t / /think) re-expands it.
func TestTUIThinkingFolds(t *testing.T) {
	m := newTestModel(t)

	m.applyEvent(agent.Event{Type: agent.EventThinkingDelta, Thinking: "first line\nsecond line"})
	got := m.renderTranscript()
	if !strings.Contains(got, "▾ thinking") {
		t.Fatalf("open fold header missing while streaming:\n%s", got)
	}
	if !strings.Contains(got, "second line") {
		t.Fatalf("streaming thinking body missing:\n%s", got)
	}

	// Text closes the block: the body folds away behind a header + preview.
	m.applyEvent(agent.Event{Type: agent.EventTextDelta, Text: "the answer"})
	got = m.renderTranscript()
	if !strings.Contains(got, "▸ thinking") {
		t.Fatalf("folded thinking header missing:\n%s", got)
	}
	if !strings.Contains(got, "first line") {
		t.Fatalf("fold preview missing:\n%s", got)
	}
	if strings.Contains(got, "second line") {
		t.Fatalf("completed thinking not folded:\n%s", got)
	}
	if !strings.Contains(got, "the answer") {
		t.Fatalf("answer missing:\n%s", got)
	}

	// ctrl+t expands, a second press folds back, /think mirrors the key.
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	if got = m.renderTranscript(); !strings.Contains(got, "second line") {
		t.Fatalf("ctrl+t did not expand thinking:\n%s", got)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	if got = m.renderTranscript(); strings.Contains(got, "second line") {
		t.Fatalf("second ctrl+t did not fold thinking:\n%s", got)
	}
	m.slash("/think")
	if got = m.renderTranscript(); !strings.Contains(got, "second line") {
		t.Fatalf("/think did not expand thinking:\n%s", got)
	}
}

// TestTUISlashPopup: typing "/" pops the command list — filter, tab
// completion, ↑/↓ selection, esc close, ambiguous prefixes asking for
// completion instead of dying as "unknown command".
func TestTUISlashPopup(t *testing.T) {
	m := newTestModel(t)

	// Bare slash lists every command plus the key hint, and the viewport
	// gives up exactly the rows the popup will draw.
	m.ti.SetValue("/")
	m.updateSuggestions()
	if got := len(m.sugs); got != len(slashCommands) {
		t.Fatalf("bare slash matched %d commands, want %d", got, len(slashCommands))
	}
	rows := m.suggestionRows()
	if len(rows) != len(slashCommands)+1 {
		t.Fatalf("popup rows = %d, want %d (commands + hint)", len(rows), len(slashCommands)+1)
	}
	if !strings.Contains(strings.Join(rows, "\n"), "/stats") {
		t.Fatalf("popup missing /stats:\n%s", strings.Join(rows, "\n"))
	}
	if want := 30 - 3 - len(rows); m.vp.Height != want {
		t.Fatalf("vp.Height = %d, want %d (terminal − chrome − popup)", m.vp.Height, want)
	}

	// Filtering to a unique command, then tab completion. (/st alone is
	// no longer unique — /steps shares the prefix — so pin /sta.)
	m.ti.SetValue("/sta")
	m.updateSuggestions()
	if len(m.sugs) != 1 || m.sugs[0].cmd != "/stats" {
		t.Fatalf("filter /sta → %#v", m.sugs)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if m.ti.Value() != "/stats" {
		t.Fatalf("tab completed to %q, want /stats", m.ti.Value())
	}
	if cmd := m.submit(); cmd != nil {
		t.Fatal("running a slash command must not return a tea.Cmd")
	}
	if !strings.Contains(m.cmdOutText(), "no turns yet") {
		t.Fatalf("unique match did not execute /stats:\n%s", m.cmdOutText())
	}

	// Ambiguous prefix + enter runs the highlighted entry — no tab-then-
	// enter detour. Walk to /clear so the effect is observable.
	m.ti.SetValue("/")
	m.updateSuggestions()
	m.Update(tea.KeyMsg{Type: tea.KeyDown}) // sugIdx 0 → 1 (/clear)
	if len(m.sugs) < 2 || m.sugs[m.sugIdx].cmd != "/clear" {
		t.Fatalf("selection walk: idx=%d %#v", m.sugIdx, m.sugs)
	}
	if cmd := m.submit(); cmd != nil {
		t.Fatal("ambiguous enter must not return a tea.Cmd")
	}
	if v := m.ti.Value(); v != "" {
		t.Fatalf("enter left input %q, want empty", v)
	}
	if len(m.sugs) != 0 {
		t.Fatalf("popup still open after enter: %#v", m.sugs)
	}
	if len(m.cmdOut) != 0 || len(m.lines) != 0 {
		t.Fatal("selected command did not run — /clear left content behind")
	}

	// ↑/↓ walk the list (wrapping), esc closes it, typing re-arms it.
	m.ti.SetValue("/")
	m.updateSuggestions()
	m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if m.sugIdx != len(slashCommands)-1 {
		t.Fatalf("after up: sugIdx = %d, want %d", m.sugIdx, len(slashCommands)-1)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if m.sugIdx != 0 {
		t.Fatalf("after down: sugIdx = %d, want 0", m.sugIdx)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if len(m.sugs) != 0 || m.suggestionRows() != nil {
		t.Fatalf("esc did not close the popup: %#v", m.sugs)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")}) // "/t"
	if len(m.sugs) != 1 || m.sugs[0].cmd != "/think" {
		t.Fatalf("typing did not re-arm the popup: %#v", m.sugs)
	}

	// Plain text never pops anything.
	m.ti.SetValue("hello there")
	m.updateSuggestions()
	if m.suggestionRows() != nil {
		t.Fatalf("popup shown for a normal message: %#v", m.sugs)
	}

	// The documented ":q" alias quits instead of reaching the model.
	m.ti.SetValue(":q")
	cmd := m.submit()
	if cmd == nil {
		t.Fatal(":q must quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatalf(":q returned %T, want tea.QuitMsg", cmd())
	}
}

// TestTUIHistoryRecall: up/down walk the input history, typing resets.
func TestTUIHistoryRecall(t *testing.T) {
	m := newTestModel(t)
	m.hist = []string{"first", "second"}
	m.histIdx = len(m.hist)

	m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if got := m.ti.Value(); got != "second" {
		t.Fatalf("after one up: %q, want second", got)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if got := m.ti.Value(); got != "first" {
		t.Fatalf("after two up: %q, want first", got)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if got := m.ti.Value(); got != "second" {
		t.Fatalf("after down: %q, want second", got)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if got := m.ti.Value(); got != "" {
		t.Fatalf("past the end: %q, want empty", got)
	}
}

// TestTUIReplaySeedsTranscript: `chat -c` opens with the restored
// conversation rendered as the live line kinds — prompt, thinking, tool
// call, result — plus one-line notices for memory and compaction blocks.
func TestTUIReplaySeedsTranscript(t *testing.T) {
	m := newTestModel(t)
	res := anthropic.NewBetaToolResultTextBlockParam("toolu_1", "shell output", false)
	snap := &agent.Snapshot{
		Version: 1,
		SavedAt: time.Date(2026, 10, 10, 12, 30, 0, 0, time.UTC),
		Messages: []anthropic.BetaMessageParam{
			anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(
				"[persistent-memory]\n## codename\nproject lychee")),
			anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("fix the widget")),
			{
				Role: anthropic.BetaMessageParamRoleAssistant,
				Content: []anthropic.BetaContentBlockParamUnion{
					{OfThinking: &anthropic.BetaThinkingBlockParam{Thinking: "hmm, tricky"}},
					{OfToolUse: &anthropic.BetaToolUseBlockParam{
						ID: "toolu_1", Name: "run_shell",
						Input: json.RawMessage(`{"command":"ls"}`)}},
				},
			},
			anthropic.NewBetaUserMessage(
				anthropic.BetaContentBlockParamUnion{OfToolResult: &res}),
			anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(
				"[conversation compacted — this summary replaces the full history]\n\nclever summary")),
		},
	}
	m.replay(snap)
	got := m.renderTranscript()
	for _, want := range []string{
		"restored 5 messages",
		"12:30",
		"memory restored",
		"fix the widget",
		"hmm, tricky",
		"run_shell",
		"shell output",
		"📦 compaction",
		"— this summary replaces the full history",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("replay missing %q in transcript:\n%s", want, got)
		}
	}
	// Replay is display-only: no live state flips.
	if m.busy || m.appr != nil {
		t.Fatalf("replay disturbed model state: busy=%v appr=%v", m.busy, m.appr)
	}

	// A nil snapshot (fresh session) renders nothing extra.
	m2 := newTestModel(t)
	base := m2.renderTranscript()
	m2.replay(nil)
	if m2.renderTranscript() != base {
		t.Fatal("nil replay must not change the transcript")
	}
}

// TestTUIResumeSlash: /resume loads the workspace's saved snapshot, rebuilds
// the transcript from it, reports a missing snapshot clearly, and — when a
// prompt is in flight — defers the whole thing until prompt_end instead of
// racing the drive.
func TestTUIResumeSlash(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t)) // keep the store out of ~/.lyon
	cfg := agent.DefaultConfig()
	cfg.Workspace = tempDir(t)
	eng := agent.NewEngine(cfg, nil)
	sess := eng.NewSession(nil)
	t.Cleanup(sess.Close)
	m := newTUIModel(cfg, sess, tools.NewMetrics())
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	// No snapshot yet → a clear notice, transcript otherwise untouched.
	before := m.renderTranscript()
	m.slash("/resume")
	got := m.renderTranscript()
	if !strings.Contains(got, "no saved session") {
		t.Fatalf("missing no-session notice:\n%s", got)
	}
	if strings.Contains(strings.TrimPrefix(got, before), "restored") {
		t.Fatalf("resume claimed to restore without a snapshot:\n%s", got)
	}

	// Save a snapshot → /resume swaps the transcript for the replay.
	snap := agent.Snapshot{
		Version:   1,
		Workspace: cfg.Workspace,
		Model:     cfg.Model,
		SavedAt:   time.Date(2026, 10, 10, 14, 30, 0, 0, time.Local),
		Messages: []anthropic.BetaMessageParam{
			{Role: anthropic.BetaMessageParamRoleUser, Content: []anthropic.BetaContentBlockParamUnion{
				{OfText: &anthropic.BetaTextBlockParam{Text: "remember the widget fix"}},
			}},
			{Role: anthropic.BetaMessageParamRoleAssistant, Content: []anthropic.BetaContentBlockParamUnion{
				{OfText: &anthropic.BetaTextBlockParam{Text: "done — widget fixed"}},
			}},
		},
	}
	if err := agent.SaveSnapshot(snap); err != nil {
		t.Fatal(err)
	}
	m.pushStyled("OLD-SESSION-CONTENT must vanish")
	m.slash("/resume")
	got = m.renderTranscript()
	for _, want := range []string{
		"restored 2 messages", "14:30", "remember the widget fix", "done — widget fixed",
		"commands: /stats", // banner re-seeded under the replay
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("resume replay missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "OLD-SESSION-CONTENT") {
		t.Fatalf("old transcript survived the restore:\n%s", got)
	}

	// Busy: the restore waits for prompt_end instead of running now.
	m.lines = nil
	m.busy = true
	m.slash("/resume")
	if m.pending != "/resume" {
		t.Fatalf("resume not deferred while a prompt is in flight (pending=%q)", m.pending)
	}
	if got := m.renderTranscript(); strings.Contains(got, "restored 2 messages") {
		t.Fatalf("restore ran while busy:\n%s", got)
	}
	m.applyEvent(agent.Event{Type: agent.EventPromptEnd, Err: agent.ErrInterrupted.Error()})
	if m.pending != "" {
		t.Fatalf("deferred resume did not fire on prompt_end (pending=%q)", m.pending)
	}
	if m.busy {
		t.Fatal("busy not cleared")
	}
	got = m.renderTranscript()
	if !strings.Contains(got, "restored 2 messages") || !strings.Contains(got, "remember the widget fix") {
		t.Fatalf("deferred restore missing replay:\n%s", got)
	}
}

// TestTUIResumeSlashNew: /new wipes the engine conversation and transcript
// back to the banner, keeps the engine usable, and — like /resume — defers
// to prompt_end while a prompt is in flight.
func TestTUIResumeSlashNew(t *testing.T) {
	cfg := agent.DefaultConfig()
	cfg.Workspace = tempDir(t)
	eng := agent.NewEngine(cfg, nil)
	sess := eng.NewSession(nil)
	t.Cleanup(sess.Close)
	m := newTUIModel(cfg, sess, tools.NewMetrics())
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	m.pushStyled("OLD-CONVERSATION-LINE")
	m.slash("/new")
	got := m.renderTranscript()
	if !strings.Contains(got, "new session") {
		t.Fatalf("missing new-session notice:\n%s", got)
	}
	if !strings.Contains(got, "commands: /stats") {
		t.Fatalf("banner not re-seeded by /new:\n%s", got)
	}
	if strings.Contains(got, "OLD-CONVERSATION-LINE") {
		t.Fatalf("old transcript survived /new:\n%s", got)
	}
	if n := len(m.sess.Stats()); n != 0 {
		t.Fatalf("/new left %d turns in the engine", n)
	}

	// Busy: the reset waits for prompt_end instead of racing the drive.
	m.lines = nil
	m.busy = true
	m.slash("/new")
	if m.pending != "/new" {
		t.Fatalf("new not deferred while a prompt is in flight (pending=%q)", m.pending)
	}
	m.applyEvent(agent.Event{Type: agent.EventPromptEnd, Err: agent.ErrInterrupted.Error()})
	if m.pending != "" {
		t.Fatalf("deferred /new did not fire on prompt_end (pending=%q)", m.pending)
	}
	if got := m.renderTranscript(); !strings.Contains(got, "new session") {
		t.Fatalf("deferred /new did not reset the transcript:\n%s", got)
	}
}

// TestTUISessionsAndIndexedResume: /sessions lists the whole store
// newest-first, /resume N restores a specific slot (not just the newest),
// out-of-range slots say so, and parseResumeIndex validates the argument.
func TestTUISessionsAndIndexedResume(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))
	cfg := agent.DefaultConfig()
	cfg.Workspace = tempDir(t)
	eng := agent.NewEngine(cfg, nil)
	sess := eng.NewSession(nil)
	t.Cleanup(sess.Close)
	m := newTUIModel(cfg, sess, tools.NewMetrics())
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	m.slash("/sessions")
	if got := m.cmdOutText(); !strings.Contains(got, "no saved sessions") {
		t.Fatalf("empty store notice missing:\n%s", got)
	}

	mk := func(hour int, text string) agent.Snapshot {
		return agent.Snapshot{
			Version: 1, Workspace: cfg.Workspace, Model: cfg.Model,
			SavedAt: time.Date(2026, 10, 10, hour, 0, 0, 0, time.Local),
			Messages: []anthropic.BetaMessageParam{{Role: anthropic.BetaMessageParamRoleUser,
				Content: []anthropic.BetaContentBlockParamUnion{{OfText: &anthropic.BetaTextBlockParam{Text: text}}}}},
		}
	}
	if err := agent.SaveSnapshot(mk(14, "SESSION-ALPHA content")); err != nil {
		t.Fatal(err)
	}
	if err := agent.SaveSnapshot(mk(15, "SESSION-BETA content")); err != nil {
		t.Fatal(err)
	}

	m.lines = nil
	m.slash("/sessions")
	got := m.cmdOutText()
	for _, want := range []string{"saved sessions (2", "2026-10-10 15:00", "2026-10-10 14:00", "/resume 2"} {
		if !strings.Contains(got, want) {
			t.Fatalf("/sessions missing %q:\n%s", want, got)
		}
	}

	// /resume 2 reaches the OLDER session — the original complaint.
	m.slash("/resume 2")
	got = m.renderTranscript()
	if !strings.Contains(got, "SESSION-ALPHA content") {
		t.Fatalf("/resume 2 did not restore the older session:\n%s", got)
	}
	if strings.Contains(got, "SESSION-BETA content") {
		t.Fatalf("/resume 2 restored the wrong slot:\n%s", got)
	}

	m.slash("/resume 9")
	if got := m.renderTranscript(); !strings.Contains(got, "no session #9") {
		t.Fatalf("out-of-range slot not reported:\n%s", got)
	}
	m.slash("/resume nonsense")
	if got := m.cmdOutText(); !strings.Contains(got, "usage: /resume") {
		t.Fatalf("bad argument not reported:\n%s", got)
	}
}

// TestTUIResumeOpensPicker: a bare /resume over several saved sessions opens
// an interactive picker — ↑/↓ wrap, esc cancels without restoring, enter
// restores exactly the highlighted slot — while a single saved session
// restores directly and an empty store just reports.
func TestTUIResumeOpensPicker(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))
	cfg := agent.DefaultConfig()
	cfg.Workspace = tempDir(t)
	eng := agent.NewEngine(cfg, nil)
	sess := eng.NewSession(nil)
	t.Cleanup(sess.Close)
	m := newTUIModel(cfg, sess, tools.NewMetrics())
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	// Empty store: a notice, no picker.
	m.slash("/resume")
	if len(m.pick) != 0 {
		t.Fatal("picker opened with no sessions")
	}
	if got := m.renderTranscript(); !strings.Contains(got, "no saved session") {
		t.Fatalf("empty-store notice missing:\n%s", got)
	}

	mk := func(hour int, text string) agent.Snapshot {
		return agent.Snapshot{
			Version: 1, Workspace: cfg.Workspace, Model: cfg.Model,
			SavedAt: time.Date(2026, 10, 10, hour, 0, 0, 0, time.Local),
			Messages: []anthropic.BetaMessageParam{{Role: anthropic.BetaMessageParamRoleUser,
				Content: []anthropic.BetaContentBlockParamUnion{{OfText: &anthropic.BetaTextBlockParam{Text: text}}}}},
		}
	}
	if err := agent.SaveSnapshot(mk(14, "PICKER-ALPHA")); err != nil {
		t.Fatal(err)
	}
	if err := agent.SaveSnapshot(mk(15, "PICKER-BETA")); err != nil {
		t.Fatal(err)
	}

	// Two sessions: bare /resume opens the picker instead of restoring.
	m.lines = nil
	m.slash("/resume")
	if len(m.pick) != 2 || m.pickIdx != 0 {
		t.Fatalf("picker = %d rows idx %d, want 2 rows at 0", len(m.pick), m.pickIdx)
	}
	if got := m.renderTranscript(); strings.Contains(got, "restored") {
		t.Fatalf("picker restored before a choice:\n%s", got)
	}
	rows := strings.Join(m.suggestionRows(), "\n")
	for _, want := range []string{
		"resume — saved sessions (2", "2026-10-10 15:00", "2026-10-10 14:00", "enter restore",
	} {
		if !strings.Contains(rows, want) {
			t.Fatalf("picker rows missing %q:\n%s", want, rows)
		}
	}

	// ↑/↓ walk with wraparound.
	m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if m.pickIdx != 1 {
		t.Fatalf("up: pickIdx = %d, want 1", m.pickIdx)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if m.pickIdx != 0 {
		t.Fatalf("down: pickIdx = %d, want 0", m.pickIdx)
	}

	// esc cancels — popup gone, nothing restored.
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if len(m.pick) != 0 {
		t.Fatal("esc left the picker open")
	}
	if got := m.renderTranscript(); strings.Contains(got, "restored") {
		t.Fatalf("esc restored a session:\n%s", got)
	}

	// Reopen, highlight the OLDER slot, enter restores exactly it.
	m.slash("/resume")
	if len(m.pick) != 2 {
		t.Fatalf("reopen failed: %d rows", len(m.pick))
	}
	m.Update(tea.KeyMsg{Type: tea.KeyDown}) // pick #2 (ALPHA, 14:00)
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.pick) != 0 {
		t.Fatal("enter left the picker open")
	}
	got := m.renderTranscript()
	if !strings.Contains(got, "PICKER-ALPHA") || !strings.Contains(got, "restored 1 messages") {
		t.Fatalf("enter did not restore the highlighted slot:\n%s", got)
	}
	if strings.Contains(got, "PICKER-BETA") {
		t.Fatalf("picker restored the wrong session:\n%s", got)
	}

	// Exactly one saved session: bare /resume restores directly, no picker.
	t.Setenv("LYON_HOME", tempDir(t))
	cfg2 := agent.DefaultConfig()
	cfg2.Workspace = tempDir(t)
	eng2 := agent.NewEngine(cfg2, nil)
	sess2 := eng2.NewSession(nil)
	t.Cleanup(sess2.Close)
	m2 := newTUIModel(cfg2, sess2, tools.NewMetrics())
	m2.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	if err := agent.SaveSnapshot(agent.Snapshot{
		Version: 1, Workspace: cfg2.Workspace, Model: cfg2.Model,
		SavedAt: time.Date(2026, 10, 10, 16, 0, 0, 0, time.Local),
		Messages: []anthropic.BetaMessageParam{{Role: anthropic.BetaMessageParamRoleUser,
			Content: []anthropic.BetaContentBlockParamUnion{{OfText: &anthropic.BetaTextBlockParam{Text: "SOLO-SESSION"}}}}},
	}); err != nil {
		t.Fatal(err)
	}
	m2.slash("/resume")
	if len(m2.pick) != 0 {
		t.Fatal("picker opened for a single saved session")
	}
	if got := m2.renderTranscript(); !strings.Contains(got, "SOLO-SESSION") {
		t.Fatalf("single-session /resume did not restore:\n%s", got)
	}
}

// TestTUIHeaderDrawnOnce: the header is pinned chrome in View — the
// transcript banner must not repeat it (startup, after /clear, after /new).
func TestTUIHeaderDrawnOnce(t *testing.T) {
	m := newTestModel(t)
	check := func(when string) {
		t.Helper()
		if n := strings.Count(m.View(), "lyon chat"); n != 1 {
			t.Fatalf("%s: header appears %d times in View:\n%s", when, n, m.View())
		}
	}
	check("startup")
	m.slash("/clear")
	check("after /clear")
	m.slash("/new")
	check("after /new")
	// The commands banner still seeds the fresh transcript.
	if !strings.Contains(m.renderTranscript(), "commands: /stats") {
		t.Fatalf("commands banner missing after /new:\n%s", m.renderTranscript())
	}
}

// TestParseResumeIndex: empty → newest slot (1); numbers pass through;
// zero, negatives and garbage are rejected.
func TestParseResumeIndex(t *testing.T) {
	cases := []struct {
		arg  string
		want int
		ok   bool
	}{
		{"", 1, true},
		{"2", 2, true},
		{" 3 ", 3, true},
		{"0", 0, false},
		{"-1", 0, false},
		{"x", 0, false},
		{"1.5", 0, false},
	}
	for _, c := range cases {
		got, ok := parseResumeIndex(c.arg)
		if got != c.want || ok != c.ok {
			t.Errorf("parseResumeIndex(%q) = %d, %v — want %d, %v", c.arg, got, ok, c.want, c.ok)
		}
	}
}
