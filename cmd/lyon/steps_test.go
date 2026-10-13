package main

import (
	"encoding/json"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"lyon-agent/pkg/agent"
)

// TestStepLifecycle: every effective action folds into a numbered step
// with the right running/done/error state, and prompt_end truthfully
// closes whatever never got its end event.
func TestStepLifecycle(t *testing.T) {
	m := newTestModel(t)

	// Server-side tool: decided → result, no harness exec events.
	m.applyEvent(agent.Event{
		Type: agent.EventToolUseStart, ToolID: "srv1", ToolName: "web_search",
		Input: json.RawMessage(`{"query":"go sse"}`), Server: true,
	})
	if len(m.steps) != 1 || m.steps[0].status != stepRunning || m.steps[0].kind != stepServer {
		t.Fatalf("server step after use_start: %+v", m.steps)
	}
	m.applyEvent(agent.Event{Type: agent.EventServerToolResult, ToolID: "srv1", ToolName: "web_search"})
	if s := m.steps[0]; s.status != stepDone {
		t.Fatalf("server step after result: %+v", s)
	}

	// Harness tool: use_start → exec_start pairs by name, end by ExecID.
	m.applyEvent(agent.Event{
		Type: agent.EventToolUseStart, ToolID: "t1", ToolName: "read_file",
		Input: json.RawMessage(`{"path":"README.md"}`),
	})
	m.applyEvent(agent.Event{Type: agent.EventToolExecStart, ExecID: 7, ToolName: "read_file"})
	if s := m.steps[1]; s.execID != 7 || s.status != stepRunning {
		t.Fatalf("step after exec_start: %+v", s)
	}
	m.applyEvent(agent.Event{Type: agent.EventToolExecEnd, ExecID: 7, ToolName: "read_file", DurationMS: 120})
	if s := m.steps[1]; s.status != stepDone || s.durMS != 120 {
		t.Fatalf("step after exec_end: %+v", s)
	}

	// Failing tool records the error text.
	m.applyEvent(agent.Event{Type: agent.EventToolUseStart, ToolID: "t2", ToolName: "run_shell"})
	m.applyEvent(agent.Event{Type: agent.EventToolExecStart, ExecID: 8, ToolName: "run_shell"})
	m.applyEvent(agent.Event{
		Type: agent.EventToolExecEnd, ExecID: 8, ToolName: "run_shell",
		DurationMS: 5, IsError: true, Err: "boom",
	})
	if s := m.steps[2]; s.status != stepError || s.err != "boom" {
		t.Fatalf("failing step: %+v", s)
	}

	// Subagent and compaction land as finished steps of their own kind.
	m.applyEvent(agent.Event{
		Type: agent.EventSubagentEnd, ToolName: "claude-haiku",
		Detail: "3 API calls", DurationMS: 500,
	})
	m.applyEvent(agent.Event{
		Type: agent.EventCompaction, Detail: "summary replaced 12 messages (40.0KB → 2.0KB)",
	})
	if s := m.steps[3]; s.kind != stepSubagent || s.status != stepDone {
		t.Fatalf("subagent step: %+v", s)
	}
	if s := m.steps[4]; s.kind != stepCompact || s.status != stepDone {
		t.Fatalf("compact step: %+v", s)
	}
	m.applyEvent(agent.Event{Type: agent.EventCompaction, Detail: "compaction failed: no summarizer"})
	if s := m.steps[5]; s.status != stepError || s.err == "" {
		t.Fatalf("failed compaction step: %+v", s)
	}

	// Interrupted run: a step still running at prompt_end is closed as
	// interrupted, never left dangling.
	m.applyEvent(agent.Event{Type: agent.EventToolUseStart, ToolID: "t3", ToolName: "write_file"})
	m.applyEvent(agent.Event{
		Type: agent.EventPromptEnd, Err: agent.ErrInterrupted.Error(), Iterations: 4,
	})
	if s := m.steps[6]; s.status != stepError || s.err != "interrupted" {
		t.Fatalf("interrupted step: %+v", s)
	}

	// Session-wide numbering stays sequential.
	for i, s := range m.steps {
		if s.n != i+1 {
			t.Fatalf("step %d numbered %d", i+1, s.n)
		}
	}
}

// TestRenderStepsSizing: the renderer's contract — exactly h rows, each
// at most w cells, newest step at the bottom of the window.
func TestRenderStepsSizing(t *testing.T) {
	m := newTestModel(t)
	for i := 0; i < 50; i++ {
		m.applyEvent(agent.Event{Type: agent.EventToolUseStart, ToolID: "t", ToolName: "read_file"})
	}
	rows := m.renderSteps(40, 10)
	if len(rows) != 10 {
		t.Fatalf("rows = %d, want 10", len(rows))
	}
	for i, r := range rows {
		if n := ansi.StringWidth(r); n > 40 {
			t.Fatalf("row %d is %d cells: %q", i, n, r)
		}
	}
	if !strings.Contains(rows[0], "steps") {
		t.Fatalf("header row: %q", rows[0])
	}
	// The last row is the key-hint footer; the newest step sits just
	// above it (#41).
	if !strings.Contains(rows[8], "#50") {
		t.Fatalf("newest step must sit above the footer: %q", rows[8])
	}
	if !strings.Contains(stripColor(rows[9]), "alt+↑↓") {
		t.Fatalf("footer key-hint row missing: %q", rows[9])
	}
}

// TestStepsPanelGeometry: at wide sizes the View splits into transcript +
// gutter + panel and still satisfies the exact-frame invariants; at 100
// columns (every legacy TUI test's size) the panel stays away; /steps
// toggles the split.
func TestStepsPanelGeometry(t *testing.T) {
	m := newTestModel(t)
	m.applyEvent(agent.Event{
		Type: agent.EventToolUseStart, ToolID: "t1", ToolName: "read_file",
		Input: json.RawMessage(`{"path":"a.txt"}`),
	})
	m.applyEvent(agent.Event{Type: agent.EventToolExecStart, ExecID: 1, ToolName: "read_file"})

	// 100 columns: below the floor, transcript full width.
	if m.pw != 0 || m.tw != 100 {
		t.Fatalf("layout at 100: tw=%d pw=%d", m.tw, m.pw)
	}
	checkView(t, m, "panel floor: 100 cols")

	// 140 columns: split appears, geometry holds, panel is in the frame.
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	if m.pw != panelWidth || m.tw != 140-panelWidth-2 {
		t.Fatalf("layout at 140: tw=%d pw=%d", m.tw, m.pw)
	}
	checkView(t, m, "panel on: 140 cols")
	view := m.View()
	if !strings.Contains(view, "│") {
		t.Fatal("gutter missing at 140 cols")
	}
	if !strings.Contains(view, "steps") {
		t.Fatal("panel header missing at 140 cols")
	}
	if !strings.Contains(view, "read_file") {
		t.Fatal("step label missing from the panel")
	}

	// /steps toggles off and back on (re-laying out the transcript width).
	m.slash("/steps")
	if m.pw != 0 || m.tw != 140 {
		t.Fatalf("/steps off layout: tw=%d pw=%d", m.tw, m.pw)
	}
	checkView(t, m, "panel toggled off")
	if strings.Contains(m.View(), "│") {
		t.Fatal("gutter still present after toggle off")
	}
	m.slash("/steps")
	if m.pw != panelWidth {
		t.Fatalf("/steps on pw=%d", m.pw)
	}
	checkView(t, m, "panel toggled on")
}
