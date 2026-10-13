package agent

import "testing"

// TestToolUseQFIFO: executions claim streamed tool_use ids in block
// order, per tool name — the correlation that lets the TUI nest parallel
// exec rows under their own intent (#45). Subagent-inner windows and
// empty queues must never claim.
func TestToolUseQFIFO(t *testing.T) {
	var s Session
	s.trackToolUse("call_a", "run_shell")
	s.trackToolUse("call_b", "run_shell")
	s.trackToolUse("call_c", "write_file")

	if got := s.takeToolUse("run_shell"); got != "call_a" {
		t.Fatalf("first run_shell pop = %q, want call_a", got)
	}
	if got := s.takeToolUse("run_shell"); got != "call_b" {
		t.Fatalf("second run_shell pop = %q, want call_b", got)
	}
	if got := s.takeToolUse("run_shell"); got != "" {
		t.Fatalf("drained queue must return empty, got %q", got)
	}
	if got := s.takeToolUse("write_file"); got != "call_c" {
		t.Fatalf("write_file pop = %q, want call_c", got)
	}
	// While a subagent runs, nothing registers or claims: inner calls
	// share the session observer but have no main-flow intent rows.
	s.subNest = 1
	s.trackToolUse("call_d", "grep")
	if got := s.takeToolUse("grep"); got != "" {
		t.Fatalf("subagent window must not claim, got %q", got)
	}
	s.subNest = 0
	s.trackToolUse("call_e", "grep")
	if got := s.takeToolUse("grep"); got != "call_e" {
		t.Fatalf("post-subagent pop = %q, want call_e", got)
	}
}
