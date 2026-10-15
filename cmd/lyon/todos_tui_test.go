package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"lyon-agent/pkg/agent"
)

// todoBlockRows returns the stripped text of the todo-tagged block lines.
func todoBlockRows(m *tuiModel) []string {
	var out []string
	for _, l := range m.lines {
		if l.todo {
			out = append(out, stripANSI(l.raw))
		}
	}
	return out
}

// rightColumnRows splits composeSplit's output into the right column's
// rows (ANSI stripped) — one per row, len == vh.
func rightColumnRows(m *tuiModel, vh int) []string {
	out := make([]string, 0, vh)
	for _, line := range strings.Split(m.composeSplit(vh), "\n") {
		plain := stripColor(line)
		if i := strings.Index(plain, "│"); i >= 0 {
			out = append(out, plain[i+len("│"):]) // "│" is 3 bytes
			continue
		}
		out = append(out, "")
	}
	return out
}

const todoPlan1 = `{"todos":[
	{"content":"read the spec","status":"in_progress"},
	{"content":"wire the tool","status":"pending"}
]}`

const todoPlan2 = `{"todos":[
	{"content":"read the spec","status":"completed"},
	{"content":"wire the tool","status":"in_progress"}
]}`

// TestTodoBlockUpdatesInPlace: the first todo_write opens the block
// (header + one glyph row per item); the next one REPLACES it in place —
// ten refreshes never bury the transcript — and the busy status strip
// shows the scale plus the current subtask (#66).
func TestTodoBlockUpdatesInPlace(t *testing.T) {
	m := newTestModel(t)
	m.applyEvent(agent.Event{
		Type: agent.EventToolExecStart, ExecID: 1, ToolName: "todo_write",
		Input: json.RawMessage(todoPlan1),
	})

	rows := todoBlockRows(m)
	if len(rows) != 3 {
		t.Fatalf("first plan rows = %d (%v), want header + 2 items", len(rows), rows)
	}
	if !strings.Contains(rows[0], "📋") || !strings.Contains(rows[0], "0/2") {
		t.Fatalf("header = %q, want 📋 0/2", rows[0])
	}
	if !strings.Contains(rows[1], "◐") || !strings.Contains(rows[1], "read the spec") {
		t.Fatalf("in-progress row = %q", rows[1])
	}
	if !strings.Contains(rows[2], "☐") {
		t.Fatalf("pending row = %q", rows[2])
	}

	// The next plan rewrites the same block — still exactly one header.
	m.applyEvent(agent.Event{
		Type: agent.EventToolExecStart, ExecID: 2, ToolName: "todo_write",
		Input: json.RawMessage(todoPlan2),
	})
	rows = todoBlockRows(m)
	if len(rows) != 3 {
		t.Fatalf("plan not replaced in place: %d rows (%v)", len(rows), rows)
	}
	if !strings.Contains(rows[0], "1/2") || !strings.Contains(rows[1], "☑") {
		t.Fatalf("updated rows = %v, want 1/2 with ☑ read the spec", rows)
	}

	// Busy strip: glanceable scale + current subtask.
	m.busy = true
	line := stripANSI(m.statusLine())
	if !strings.Contains(line, "📋 1/2") || !strings.Contains(line, "▸ wire the tool") {
		t.Fatalf("busy status = %q, want 📋 1/2 ▸ wire the tool", line)
	}

	// The steps panel preview reads as progress, not raw JSON.
	preview := stepArgPreview("todo_write", json.RawMessage(todoPlan2))
	if !strings.Contains(preview, "1/2") || !strings.Contains(preview, "wire the tool") {
		t.Fatalf("panel preview = %q, want scale + current", preview)
	}
}

// TestTodoWriteErrorKeepsPlan: a rejected plan leaves the old block
// standing — only the failure row is new.
func TestTodoWriteErrorKeepsPlan(t *testing.T) {
	m := newTestModel(t)
	m.applyEvent(agent.Event{
		Type: agent.EventToolExecStart, ExecID: 1, ToolName: "todo_write",
		Input: json.RawMessage(todoPlan1),
	})
	m.applyEvent(agent.Event{
		Type: agent.EventToolExecEnd, ExecID: 1, ToolName: "todo_write",
		IsError: true, Err: "invalid status", DurationMS: 2,
	})

	rows := todoBlockRows(m)
	if len(rows) != 3 {
		t.Fatalf("error wiped the plan: %d rows", len(rows))
	}
	if out := stripANSI(m.renderTranscript()); !strings.Contains(out, "todo_write") ||
		!strings.Contains(out, "invalid status") {
		t.Fatalf("failure row missing:\n%s", out)
	}
}

// TestSubagentTodosIgnored: a subagent's own plan never overwrites the
// main flow's block — the fan-out reports on its own line (#53/#66).
func TestSubagentTodosIgnored(t *testing.T) {
	m := newTestModel(t)
	m.applyEvent(agent.Event{
		Type: agent.EventToolExecStart, ExecID: 1, ToolName: "todo_write",
		Input: json.RawMessage(todoPlan1),
	})
	m.applyEvent(agent.Event{
		Type: agent.EventToolExecStart, ExecID: 2, ToolName: "todo_write", Sub: true,
		Input: json.RawMessage(`{"todos":[{"content":"sub plan","status":"in_progress"}]}`),
	})

	rows := todoBlockRows(m)
	if len(rows) != 3 || strings.Contains(strings.Join(rows, "\n"), "sub plan") {
		t.Fatalf("subagent plan leaked into the main block: %v", rows)
	}
}

// TestTodosSnapshotRoundTrip: the TUI plan serializes for the snapshot
// and applyTodos rebuilds the block on /resume (#66).
func TestTodosSnapshotRoundTrip(t *testing.T) {
	m := newTestModel(t)
	m.setTodos(parseTodos(json.RawMessage(todoPlan2)))
	raw := todosState(m.todos)
	if len(raw) == 0 {
		t.Fatal("todosState = nil for a live plan")
	}

	m2 := newTestModel(t)
	if !m2.applyTodos(raw) {
		t.Fatal("applyTodos rejected a valid plan")
	}
	rows := todoBlockRows(m2)
	if len(rows) != 3 || !strings.Contains(rows[0], "1/2") {
		t.Fatalf("restored block = %v, want header + 2 items at 1/2", rows)
	}

	// Empty state stays empty — an unplanned session never draws a block.
	if todosState(nil) != nil {
		t.Fatal("todosState(nil) must be nil")
	}
	if m2.applyTodos(nil) {
		t.Fatal("applyTodos(nil) must report nothing to restore")
	}
}

// TestPlanSectionResident: the right column keeps a plan seat (#1) — a
// dim placeholder before the first todo_write, then a scale header
// carrying the live subtask plus one glyph row per item — composed
// directly above the shared key-hint footer, whichever panel is open.
// A plan longer than the block windows from the top with a "+n more"
// tail (the header still names the current subtask).
func TestPlanSectionResident(t *testing.T) {
	m := newTestModel(t)

	// Contract: exactly h rows of ≤ w cells; the seat exists when empty.
	rows := m.renderPlan(52, 4)
	if len(rows) != 4 {
		t.Fatalf("renderPlan rows = %d, want 4", len(rows))
	}
	for i, r := range rows {
		if n := ansi.StringWidth(r); n > 52 {
			t.Fatalf("row %d is %d cells: %q", i, n, r)
		}
	}
	if got := stripColor(rows[0]); !strings.Contains(got, "plan") ||
		!strings.Contains(got, "none yet") {
		t.Fatalf("empty placeholder = %q, want dim plan seat", got)
	}

	// Live plan: header scale + current subtask, glyph rows below.
	m.setTodos(parseTodos(json.RawMessage(todoPlan1)))
	rows = m.renderPlan(52, 3)
	if len(rows) != 3 {
		t.Fatalf("live renderPlan rows = %d, want 3", len(rows))
	}
	if head := stripColor(rows[0]); !strings.Contains(head, "0/2") ||
		!strings.Contains(head, "read the spec") {
		t.Fatalf("header = %q, want scale + in-progress subtask", head)
	}
	if !strings.Contains(stripColor(rows[1]), "read the spec") ||
		!strings.Contains(stripColor(rows[2]), "wire the tool") {
		t.Fatalf("item rows = %q / %q", rows[1], rows[2])
	}

	// Long plan, short block: window from the top, "+n more" tail.
	var long []todoItem
	for i := 0; i < 8; i++ {
		status := "pending"
		if i == 0 {
			status = "in_progress"
		}
		long = append(long, todoItem{Content: fmt.Sprintf("task %d", i), Status: status})
	}
	m.setTodos(long)
	rows = m.renderPlan(52, 5)
	if len(rows) != 5 {
		t.Fatalf("windowed renderPlan rows = %d, want 5", len(rows))
	}
	if tail := stripColor(rows[4]); !strings.Contains(tail, "+5") {
		t.Fatalf("tail = %q, want +5 more", tail)
	}
	if head := stripColor(rows[0]); !strings.Contains(head, "0/8") ||
		!strings.Contains(head, "task 0") {
		t.Fatalf("windowed header = %q, want scale + current (the window may cut items)", head)
	}

	// Column composition: plan block directly above the steps footer.
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	col := rightColumnRows(m, 20)
	if len(col) != 20 {
		t.Fatalf("composed column rows = %d, want 20", len(col))
	}
	planAt, footerAt := -1, -1
	for i, r := range col {
		if strings.Contains(r, "📋") {
			planAt = i
		}
		if strings.Contains(r, "alt+enter") {
			footerAt = i
		}
	}
	if planAt < 0 || footerAt != 19 || planAt >= footerAt {
		t.Fatalf("plan/footer placement: plan@%d footer@%d (footer must be row 19, plan above it)", planAt, footerAt)
	}

	// The seat also holds (above the footer) before any todo_write.
	m2 := newTestModel(t)
	m2.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	col = rightColumnRows(m2, 20)
	if joined := strings.Join(col, "\n"); !strings.Contains(joined, "none yet") {
		t.Fatalf("empty plan seat missing from the column:\n%s", joined)
	}
	if last := col[19]; !strings.Contains(last, "alt+enter") {
		t.Fatalf("footer under the empty seat = %q", last)
	}

	// Evolution panel open: the seat and footer both stay in place.
	m2.evoOn = true
	col = rightColumnRows(m2, 20)
	joined := strings.Join(col, "\n")
	if !strings.Contains(joined, "none yet") {
		t.Fatalf("plan seat vanished with the evo panel open:\n%s", joined)
	}
	if last := col[19]; !strings.Contains(last, "/exam") {
		t.Fatalf("evo footer = %q", last)
	}
}
