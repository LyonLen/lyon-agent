package main

import (
	"encoding/json"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"lyon-agent/pkg/agent"
)

// TestStepArgPreview: the panel and transcript show what the model asked
// for, not raw JSON. Every fallback tier gets exercised — prefer-key,
// generic key, document-order string, non-object input, and the honest
// JSON remainder for valueless input.
func TestStepArgPreview(t *testing.T) {
	cases := []struct {
		tool  string
		input string
		want  string
		ban   string // must NOT appear
	}{
		{"run_shell", `{"command":"cd /tmp && ls -la","timeout":30}`, "cd /tmp && ls -la", "{"},
		{"run_shell", `{"command":"grep -r \"fix\" .\n echo done"}`, "grep -r \"fix\" . echo done", "\n"},
		{"web_search", `{"query":"go sse","max":5}`, "go sse", "{"},
		{"web_fetch", `{"url":"https://example.com/a"}`, "https://example.com/a", "{"},
		{"run_subagent", `{"task":"explore the cache layer","depth":1}`, "explore the cache layer", "{"},
		{"read_file", `{"path":"README.md"}`, "README.md", "{"}, // generic key list
		{"grep", `{"pattern":"relayout","path":"cmd"}`, "relayout", "{"},
		{"save_memory", `{"title":"build cmd","body":"go build ./..."}`, "build cmd", "{"},
		// Unknown schema: first string VALUE in document order — keys
		// (like "count" here) must be skipped.
		{"mcp_widget", `{"count":3,"label":"hello"}`, "hello", "{"},
		// HTML-escapable characters come back decoded, not &.
		{"run_shell", `{"command":"a + b"}`, "a + b", "{"},
		// Non-object payload still reads as one line.
		{"weird", `"just a string"`, "just a string", "{"},
	}
	for _, c := range cases {
		got := stepArgPreview(c.tool, json.RawMessage(c.input))
		if !strings.Contains(got, c.want) {
			t.Errorf("%s %s → %q, want substring %q", c.tool, c.input, got, c.want)
		}
		if c.ban != "" && strings.Contains(got, c.ban) {
			t.Errorf("%s %s → %q, must not contain %q", c.tool, c.input, got, c.ban)
		}
	}
	// Valueless input degrades to truncated JSON rather than nothing.
	got := stepArgPreview("odd", json.RawMessage(`{"n":1}`))
	if !strings.Contains(got, "1") {
		t.Fatalf("valueless fallback: %q", got)
	}
	// &-escaped input (models love HTML escapes) decodes to the plain
	// character — built from rune(92) so the source needs no literal
	// backslash gymnastics.
	escaped := `{"command":"a ` + string(rune(92)) + `u0026 b"}`
	if got = stepArgPreview("run_shell", json.RawMessage(escaped)); !strings.Contains(got, "a & b") {
		t.Fatalf("escaped ampersand decoded: %q", got)
	}
	// Empty input is empty, not a lone brace.
	if got = stepArgPreview("x", nil); got != "" {
		t.Fatalf("empty input: %q", got)
	}
}

// TestPromptGroupAndNesting: a prompt opens a titled parent row, main
// steps indent under it, a subagent's inner executions indent one level
// deeper still, and subagent_end merges into the task node instead of
// appending a duplicate.
func TestPromptGroupAndNesting(t *testing.T) {
	m := newTestModel(t)

	m.startPromptStep("fix the login bug\nin auth.go")
	m.applyEvent(agent.Event{
		Type: agent.EventToolUseStart, ToolID: "t1", ToolName: "read_file",
		Input: json.RawMessage(`{"path":"auth.go"}`),
	})
	// run_subagent task node, then an inner execution (Sub=true).
	m.applyEvent(agent.Event{
		Type: agent.EventToolUseStart, ToolID: "sa", ToolName: "run_subagent",
		Input: json.RawMessage(`{"task":"explore the session store"}`),
	})
	m.applyEvent(agent.Event{Type: agent.EventToolExecStart, ExecID: 10, ToolName: "run_subagent"})
	m.applyEvent(agent.Event{
		Type: agent.EventToolExecStart, ExecID: 11, ToolName: "grep",
		Input: json.RawMessage(`{"pattern":"histMu"}`), Sub: true,
	})

	if len(m.steps) != 4 {
		t.Fatalf("steps = %d, want 4: %+v", len(m.steps), m.steps)
	}
	if s := m.steps[0]; s.kind != stepPrompt || s.status != stepDone ||
		!strings.Contains(s.detail, "fix the login bug") {
		t.Fatalf("prompt group row: %+v", s)
	}
	if s := m.steps[3]; s.under != m.steps[2].n {
		t.Fatalf("inner exec under=%d, want parent %d (%+v)", s.under, m.steps[2].n, s)
	}

	// Depths render as 0 / 1 / 2 levels of two-space indent.
	w := 60
	promptRow := m.stepLine(m.steps[0], w)
	mainRow := m.stepLine(m.steps[1], w)
	innerRow := m.stepLine(m.steps[3], w)
	if strings.HasPrefix(promptRow, " ") || !strings.Contains(promptRow, "▸") {
		t.Fatalf("prompt row not a title: %q", promptRow)
	}
	if !strings.HasPrefix(mainRow, "  ") || strings.HasPrefix(mainRow, "   ") {
		t.Fatalf("main step must indent 2: %q", mainRow)
	}
	if !strings.HasPrefix(innerRow, "    ") {
		t.Fatalf("subagent inner step must indent 4: %q", innerRow)
	}

	// subagent_end merges: task node finishes, detail grows, no new row.
	m.applyEvent(agent.Event{
		Type: agent.EventSubagentEnd, ToolName: "claude-haiku",
		Detail: "3 API calls", DurationMS: 500,
	})
	if len(m.steps) != 4 {
		t.Fatalf("subagent_end appended a duplicate: %+v", m.steps)
	}
	node := m.steps[2]
	if node.kind != stepSubagent || node.status != stepDone || node.durMS != 500 ||
		!strings.Contains(node.detail, "explore the session store") ||
		!strings.Contains(node.detail, "3 API calls") {
		t.Fatalf("merged task node: %+v", node)
	}

	// The node's own exec_end arriving after the merge must not resurrect
	// or duplicate it.
	m.applyEvent(agent.Event{Type: agent.EventToolExecEnd, ExecID: 10, ToolName: "run_subagent", DurationMS: 520})
	if len(m.steps) != 4 || m.steps[2].kind != stepSubagent {
		t.Fatalf("late exec_end corrupted the node: %+v", m.steps)
	}
	// The inner exec completes against its own ExecID.
	m.applyEvent(agent.Event{Type: agent.EventToolExecEnd, ExecID: 11, ToolName: "grep", DurationMS: 9})
	if s := m.steps[3]; s.status != stepDone || s.durMS != 9 {
		t.Fatalf("inner exec end: %+v", s)
	}
}

// TestStepInteraction: alt+↑/↓ selection walk, alt+enter expansion, and
// alt+←/→ resizing — including the empty-panel no-ops and clamps.
func TestStepInteraction(t *testing.T) {
	m := newTestModel(t)

	// Empty panel: selection and expansion are safe no-ops.
	m.stepMove(-1)
	m.stepToggleOpen()
	if m.stepSel != 0 || m.stepOpen {
		t.Fatalf("empty panel mutated state: sel=%d open=%v", m.stepSel, m.stepOpen)
	}

	// Populate 5 steps at a wide-enough terminal (panel on).
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	for i := 0; i < 5; i++ {
		m.applyEvent(agent.Event{
			Type: agent.EventToolUseStart, ToolID: "t", ToolName: "read_file",
			Input: json.RawMessage(`{"path":"f.txt"}`),
		})
	}

	// First alt+up selects the newest; further up walks older; down walks
	// back; one past the newest returns to live-follow and closes.
	m.Update(tea.KeyMsg{Type: tea.KeyUp, Alt: true})
	if m.stepSel != 5 {
		t.Fatalf("first alt+up: sel=%d, want 5", m.stepSel)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyUp, Alt: true})
	if m.stepSel != 4 {
		t.Fatalf("alt+up: sel=%d, want 4", m.stepSel)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyDown, Alt: true})
	if m.stepSel != 5 {
		t.Fatalf("alt+down: sel=%d, want 5", m.stepSel)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyDown, Alt: true})
	if m.stepSel != 0 || m.stepOpen {
		t.Fatalf("past newest must return to live: sel=%d open=%v", m.stepSel, m.stepOpen)
	}
	// From live-follow, alt+down is a no-op (already at the tail).
	m.Update(tea.KeyMsg{Type: tea.KeyDown, Alt: true})
	if m.stepSel != 0 {
		t.Fatalf("alt+down on live: sel=%d", m.stepSel)
	}

	// Alt+enter with nothing selected inspects the newest; again collapses.
	m.Update(tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	if m.stepSel != 5 || !m.stepOpen {
		t.Fatalf("alt+enter: sel=%d open=%v", m.stepSel, m.stepOpen)
	}
	rows := m.renderSteps(40, 20)
	joined := strings.Join(rows, "\n")
	if !strings.Contains(joined, "@#5") {
		t.Fatalf("header missing selection marker:\n%s", joined)
	}
	if color && !strings.Contains(joined, "\x1b[7m") {
		t.Fatalf("selected row not highlighted:\n%q", joined)
	}
	if !strings.Contains(joined, "started ") {
		t.Fatalf("expanded body missing:\n%s", joined)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	if m.stepOpen {
		t.Fatal("second alt+enter must collapse")
	}

	// Resize: starts from the default, clamps at both ends, re-lays out.
	if m.pw != panelWidth {
		t.Fatalf("pw at 140 = %d, want %d", m.pw, panelWidth)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyLeft, Alt: true})
	if m.panelW != panelWidth-4 || m.pw != panelWidth-4 {
		t.Fatalf("shrink: panelW=%d pw=%d", m.panelW, m.pw)
	}
	if m.tw != 140-(panelWidth-4)-2 {
		t.Fatalf("transcript did not re-flow: tw=%d", m.tw)
	}
	for i := 0; i < 20; i++ {
		m.Update(tea.KeyMsg{Type: tea.KeyLeft, Alt: true})
	}
	if m.panelW != panelWMin || m.pw != panelWMin {
		t.Fatalf("clamp low: panelW=%d pw=%d", m.panelW, m.pw)
	}
	for i := 0; i < 30; i++ {
		m.Update(tea.KeyMsg{Type: tea.KeyRight, Alt: true})
	}
	if m.panelW != panelWMax || m.pw != panelWMax {
		t.Fatalf("clamp high: panelW=%d pw=%d", m.panelW, m.pw)
	}
	if !strings.Contains(m.status, "panel width") {
		t.Fatalf("resize silent: status=%q", m.status)
	}
	checkView(t, m, "panel resized")

	// With the panel toggled off the alt keys leave state alone.
	m.slash("/steps")
	before := m.stepSel
	m.Update(tea.KeyMsg{Type: tea.KeyUp, Alt: true})
	if m.stepSel != before {
		t.Fatalf("alt+up with panel off moved selection: %d → %d", before, m.stepSel)
	}
}

// TestExpandRowCap: a huge operation never eats the panel — expansion is
// capped and the cap row is visibly truncated.
func TestExpandRowCap(t *testing.T) {
	m := newTestModel(t)
	s := m.addStep(stepTool, "", "run_shell", strings.Repeat("x", 2000))
	rows := m.stepExpandRows(*s, 40)
	if len(rows) > expandMaxRows {
		t.Fatalf("expand rows = %d, cap %d", len(rows), expandMaxRows)
	}
	if !strings.HasSuffix(strings.TrimRight(rows[len(rows)-1], " "), "…") &&
		!strings.Contains(rows[len(rows)-1], "…") {
		t.Fatalf("capped expansion must end with …: %q", rows[len(rows)-1])
	}
	for _, r := range rows {
		if ansi.StringWidth(r) > 40 {
			t.Fatalf("expand row overflows: %q", r)
		}
	}
}

// TestStepsSubcommands: /steps open|close|width mirror the alt keys, and
// bad input explains itself instead of silently doing nothing.
func TestStepsSubcommands(t *testing.T) {
	m := newTestModel(t)
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	for i := 0; i < 3; i++ {
		m.applyEvent(agent.Event{
			Type: agent.EventToolUseStart, ToolID: "t", ToolName: "read_file",
			Input: json.RawMessage(`{"path":"f.txt"}`),
		})
	}

	m.slash("/steps open")
	if !m.stepsOn || m.stepSel == 0 || !m.stepOpen {
		t.Fatalf("/steps open: on=%v sel=%d open=%v", m.stepsOn, m.stepSel, m.stepOpen)
	}
	m.slash("/steps close")
	if m.stepOpen {
		t.Fatal("/steps close left the step expanded")
	}

	m.slash("/steps width 56")
	if m.panelW != 56 || m.pw != 56 {
		t.Fatalf("width 56: panelW=%d pw=%d", m.panelW, m.pw)
	}
	m.slash("/steps width 10") // below the floor: rejected with usage
	if m.panelW != 56 || !strings.Contains(m.status, "usage") {
		t.Fatalf("width 10 accepted? panelW=%d status=%q", m.panelW, m.status)
	}
	m.slash("/steps width abc")
	if m.panelW != 56 || !strings.Contains(m.status, "usage") {
		t.Fatalf("width abc: panelW=%d status=%q", m.panelW, m.status)
	}
	m.slash("/steps bogus")
	if !strings.Contains(m.status, "usage") {
		t.Fatalf("bogus subcommand silent: %q", m.status)
	}
	checkView(t, m, "steps subcommands")
}

// TestTranscriptShowsReadableParams: chat lines carry the command/query
// the model asked for — raw JSON braces never reach the transcript.
func TestTranscriptShowsReadableParams(t *testing.T) {
	m := newTestModel(t)
	m.applyEvent(agent.Event{
		Type: agent.EventToolExecStart, ExecID: 1, ToolName: "run_shell",
		Input: json.RawMessage(`{"command":"cd /tmp && ls","timeout":30}`),
	})
	m.applyEvent(agent.Event{
		Type: agent.EventToolUseStart, ToolID: "s1", ToolName: "web_search",
		Input: json.RawMessage(`{"query":"go sse retries"}`), Server: true,
	})
	var parts []string
	for _, l := range m.lines {
		parts = append(parts, l.raw)
	}
	joined := strings.Join(parts, "\n")
	if !strings.Contains(joined, "cd /tmp && ls") {
		t.Fatalf("shell command missing from transcript:\n%s", joined)
	}
	if !strings.Contains(joined, "go sse retries") {
		t.Fatalf("search query missing from transcript:\n%s", joined)
	}
	if strings.Contains(joined, `"command"`) || strings.Contains(joined, `"query"`) {
		t.Fatalf("raw JSON keys leaked into transcript:\n%s", joined)
	}
}
