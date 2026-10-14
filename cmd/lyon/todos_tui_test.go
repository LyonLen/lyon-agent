package main

import (
	"encoding/json"
	"strings"
	"testing"

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
