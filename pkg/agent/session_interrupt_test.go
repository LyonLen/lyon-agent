package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"lyon-agent/internal/mockapi"
	"lyon-agent/pkg/tools"
)

// waitEvent drains the session event stream until a event of type typ
// arrives, failing the test on timeout or stream close.
func waitEvent(t *testing.T, sess *Session, typ EventType) Event {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				t.Fatalf("event stream closed while waiting for %s", typ)
			}
			if ev.Type == typ {
				return ev
			}
		case <-deadline:
			t.Fatalf("timeout waiting for %s", typ)
		}
	}
}

// TestInterruptDuringToolExecution: Esc-equivalent mid tool run cancels the
// tool, ends the prompt with ErrInterrupted, keeps the session alive, and
// the follow-up prompt replays a coherent history (the interrupted call is
// answered by a tool_result, never a bare tool_use).
func TestInterruptDuringToolExecution(t *testing.T) {
	mock := mockapi.New(mockapi.Config{
		Script: []mockapi.Turn{
			{Tool: &mockapi.ToolCall{Name: "blocker", Input: "{}"}},
			{Text: []string{"recovered"}},
		},
		Latency: 5 * time.Millisecond,
	})
	t.Cleanup(mock.Close)
	t.Setenv("ANTHROPIC_BASE_URL", mock.URL())
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "interrupt-test-token")

	reg := tools.NewRegistry()
	if err := tools.RegisterFunc(reg, "blocker", "blocks until its context is canceled", false,
		func(ctx context.Context, _ struct{}) (string, error) {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(20 * time.Second):
				return "unblocked without interrupt", nil
			}
		}); err != nil {
		t.Fatal(err)
	}

	eng := NewEngine(Config{}, reg)
	sess := eng.NewSession(nil)
	defer sess.Close()

	if sess.Interrupt() {
		t.Fatal("Interrupt on an idle session must return false")
	}
	if err := sess.Submit("start the blocking tool"); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, sess, EventToolExecStart)
	if !sess.Interrupt() {
		t.Fatal("Interrupt returned false while a prompt was running")
	}
	pe := waitEvent(t, sess, EventPromptEnd)
	if pe.Err != ErrInterrupted.Error() {
		t.Fatalf("prompt_end err = %q, want %q", pe.Err, ErrInterrupted)
	}
	if sess.Interrupt() {
		t.Fatal("Interrupt after prompt_end must return false")
	}

	// The session survives: next prompt runs to completion.
	if err := sess.Submit("continue after the interrupt"); err != nil {
		t.Fatal(err)
	}
	pe = waitEvent(t, sess, EventPromptEnd)
	if pe.Err != "" {
		t.Fatalf("follow-up prompt failed: %s", pe.Err)
	}
	body := string(mock.LastBody())
	if !strings.Contains(body, `"tool_result"`) {
		t.Fatalf("follow-up request answered no tool_result for the interrupted call:\n%.2000s", body)
	}
}

// TestRepairAfterMaxIterations: MaxIterations can cut the loop after the
// assistant emitted tool_use but before the tools ran, leaving a history
// the API would reject (tool_use without tool_result). repairInterrupted-
// History must answer those calls in the next prompt's message.
func TestRepairAfterMaxIterations(t *testing.T) {
	mock := mockapi.New(mockapi.Config{
		Script: []mockapi.Turn{
			{Tool: &mockapi.ToolCall{Name: "never_ran", Input: "{}"}},
			{Text: []string{"second prompt answered"}},
		},
		Latency: 5 * time.Millisecond,
	})
	t.Cleanup(mock.Close)
	t.Setenv("ANTHROPIC_BASE_URL", mock.URL())
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "repair-test-token")

	eng := NewEngine(Config{MaxIterations: 1}, nil)
	sess := eng.NewSession(nil)
	defer sess.Close()

	if err := sess.Submit("call a tool"); err != nil {
		t.Fatal(err)
	}
	if pe := waitEvent(t, sess, EventPromptEnd); pe.Err != "" {
		t.Fatalf("first prompt (max-iter cut): %s", pe.Err)
	} else if pe.StopReason != "max_iterations" {
		t.Fatalf("cut prompt_end stop_reason = %q, want max_iterations", pe.StopReason)
	}

	if err := sess.Submit("next"); err != nil {
		t.Fatal(err)
	}
	if pe := waitEvent(t, sess, EventPromptEnd); pe.Err != "" {
		t.Fatalf("second prompt: %s", pe.Err)
	} else if pe.StopReason != "" {
		t.Fatalf("natural end stop_reason = %q, want empty", pe.StopReason)
	}

	body := string(mock.LastBody())
	if !strings.Contains(body, "prompt interrupted before the tool finished") {
		t.Fatalf("second prompt did not carry the synthetic tool_result:\n%.2000s", body)
	}
	if !strings.Contains(body, `"tool_result"`) {
		t.Fatalf("second prompt replayed an unanswered tool_use:\n%.2000s", body)
	}
}
