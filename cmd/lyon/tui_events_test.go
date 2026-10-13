package main

import (
	"encoding/json"
	"strings"
	"testing"

	"lyon-agent/pkg/agent"
)

// TestTranscriptEventHierarchy: the transcript renders the event stream as
// a readable tree — no raw API call ids, tool calls indented under the
// turn's prose, executions under their tool_use, and subagent-inner
// executions one level deeper. A screen full of `tool_use … call_<hex>`
// rows was unreadable; these are the regression guards for the rewrite.
func TestTranscriptEventHierarchy(t *testing.T) {
	m := newTestModel(t)

	m.appendDelta(lineThink, "let me check the flow")
	m.flushPend()
	m.applyEvent(agent.Event{Type: agent.EventToolUseStart,
		ToolID: "call_deadbeefcafe0123456789", ToolName: "run_shell"})
	m.applyEvent(agent.Event{Type: agent.EventToolExecStart, ExecID: 7, ToolName: "run_shell",
		Input: json.RawMessage(`{"command":"grep -rn flow pkg/"}`)})
	m.applyEvent(agent.Event{Type: agent.EventToolExecEnd, ExecID: 7, ToolName: "run_shell", DurationMS: 528})
	// A subagent round: task call, then inner executions (Sub=true).
	m.applyEvent(agent.Event{Type: agent.EventToolUseStart,
		ToolID: "call_sub000111222333444", ToolName: "run_subagent"})
	m.applyEvent(agent.Event{Type: agent.EventToolExecStart, ExecID: 8, ToolName: "run_subagent",
		Input: json.RawMessage(`{"task":"fix the sum bug"}`)})
	m.applyEvent(agent.Event{Type: agent.EventToolExecStart, ExecID: 9, ToolName: "grep",
		Input: json.RawMessage(`{"pattern":"Sum"}`), Sub: true})
	m.applyEvent(agent.Event{Type: agent.EventToolExecEnd, ExecID: 9, ToolName: "grep", DurationMS: 400, Sub: true})
	m.applyEvent(agent.Event{Type: agent.EventToolExecEnd, ExecID: 8, ToolName: "run_subagent", DurationMS: 12000})
	m.applyEvent(agent.Event{Type: agent.EventSubagentEnd, ToolName: "claude-haiku", DurationMS: 12000,
		Detail: "3 API calls"})

	var styled []string
	for _, l := range m.lines {
		if l.kind == lineStyled {
			styled = append(styled, stripColor(l.raw))
		}
	}
	byPrefix := func(prefix string) string {
		for _, l := range styled {
			if strings.HasPrefix(strings.TrimLeft(l, " "), strings.TrimLeft(prefix, " ")) &&
				strings.HasPrefix(l, prefix) {
				return l
			}
		}
		t.Fatalf("no styled line starting with %q in %q", prefix, styled)
		return ""
	}

	tool := byPrefix(indTool + "run_shell")
	if strings.Contains(tool, "call_") {
		t.Fatalf("tool_use row must not expose the API call id: %q", tool)
	}
	exec := byPrefix(indExec + "exec #7")
	if !strings.Contains(exec, "grep -rn flow") {
		t.Fatalf("exec row must carry the arguments: %q", exec)
	}
	done := byPrefix(indExec + "done #7")
	if strings.Contains(done, "run_shell") {
		t.Fatalf("done row must not repeat the tool name: %q", done)
	}
	if !strings.Contains(done, "528ms") {
		t.Fatalf("done row must carry the duration: %q", done)
	}
	subExec := byPrefix(indSub + "exec #9")
	if !strings.HasPrefix(subExec, indSub) || len(indSub) <= len(indExec) {
		t.Fatalf("subagent-inner exec must sit deeper than %q: %q", indExec, subExec)
	}
	summary := byPrefix(indTool + "🧠 subagent")
	if !strings.Contains(summary, "3 API calls") {
		t.Fatalf("subagent summary row: %q", summary)
	}

	// Error executions keep the failure text on the done row.
	m.applyEvent(agent.Event{Type: agent.EventToolExecStart, ExecID: 10, ToolName: "run_shell",
		Input: json.RawMessage(`{"command":"false"}`)})
	m.applyEvent(agent.Event{Type: agent.EventToolExecEnd, ExecID: 10, ToolName: "run_shell",
		DurationMS: 9, IsError: true, Err: "exit status 1"})
	styled = styled[:0]
	for _, l := range m.lines {
		if l.kind == lineStyled {
			styled = append(styled, stripColor(l.raw))
		}
	}
	found := false
	for _, l := range styled {
		if strings.Contains(l, "failed #10") && strings.Contains(l, "exit status 1") {
			found = true
		}
		if strings.Contains(l, "call_") {
			t.Fatalf("no line may contain a raw call id: %q", l)
		}
	}
	if !found {
		t.Fatalf("error done row with message missing in %q", styled)
	}

	checkView(t, m, "event hierarchy")
}

// TestParallelExecsNestUnderOwnToolUse: two tool_use blocks of the same
// tool stream first, then their executions interleave — each exec/done
// pair must sit under its OWN intent row, not pile under the last one
// (the regression that sent both run_shells under the second ⚒ row).
func TestParallelExecsNestUnderOwnToolUse(t *testing.T) {
	m := newTestModel(t)
	m.applyEvent(agent.Event{Type: agent.EventToolUseStart, ToolID: "call_aaa", ToolName: "run_shell"})
	m.applyEvent(agent.Event{Type: agent.EventToolUseStart, ToolID: "call_bbb", ToolName: "run_shell"})
	m.applyEvent(agent.Event{Type: agent.EventToolExecStart, ExecID: 1, ToolID: "call_aaa",
		ToolName: "run_shell", Input: json.RawMessage(`{"command":"first"}`)})
	m.applyEvent(agent.Event{Type: agent.EventToolExecStart, ExecID: 2, ToolID: "call_bbb",
		ToolName: "run_shell", Input: json.RawMessage(`{"command":"second"}`)})
	m.applyEvent(agent.Event{Type: agent.EventToolExecEnd, ExecID: 1, ToolName: "run_shell", DurationMS: 10})
	m.applyEvent(agent.Event{Type: agent.EventToolExecEnd, ExecID: 2, ToolName: "run_shell", DurationMS: 20})

	var styled []string
	for _, l := range m.lines {
		if l.kind == lineStyled {
			styled = append(styled, stripColor(l.raw))
		}
	}
	idxOf := func(sub string) int {
		for i, l := range styled {
			if strings.Contains(l, sub) {
				return i
			}
		}
		t.Fatalf("no row containing %q in %q", sub, styled)
		return -1
	}
	// Nesting invariant: A's whole pair sits between A's intent row and
	// B's intent row — parallel calls never share a parent visually.
	iIntentA := idxOf(indTool + "run_shell")
	iExecA := idxOf("exec #1")
	iDoneA := idxOf("done #1")
	iIntentB := -1
	for i := iIntentA + 1; i < len(styled); i++ {
		if strings.HasPrefix(styled[i], indTool+"run_shell") {
			iIntentB = i
			break
		}
	}
	if iIntentB < 0 {
		t.Fatalf("second intent row missing in %q", styled)
	}
	iExecB := -1
	for i := iIntentB + 1; i < len(styled); i++ {
		if strings.Contains(styled[i], "exec #2") {
			iExecB = i
			break
		}
	}
	if iExecB < 0 {
		t.Fatalf("exec #2 not under its own intent: %q", styled)
	}
	iDoneB := idxOf("done #2")
	if !(iIntentA < iExecA && iExecA < iDoneA && iDoneA < iIntentB &&
		iIntentB < iExecB && iExecB < iDoneB) {
		t.Fatalf("interleaved order wrong: %q", styled)
	}
	// And the args prove the pairing: #1 carries the first command.
	if !strings.Contains(styled[iExecA], "first") || !strings.Contains(styled[iExecB], "second") {
		t.Fatalf("exec args paired with the wrong tool_use: %q", styled)
	}
}
