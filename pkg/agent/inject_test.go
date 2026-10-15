package agent

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lyon-agent/internal/mockapi"
	"lyon-agent/pkg/tools"
)

// newGapMock points the process at a scriptable mock whose per-request
// latency outlasts the test's Enqueue — the inbox drain may miss the very
// first gap but is guaranteed to catch a later one before prompt_end.
func newGapMock(t *testing.T, script []mockapi.Turn) *mockapi.Server {
	t.Helper()
	mock := mockapi.New(mockapi.Config{Script: script, Latency: 20 * time.Millisecond})
	t.Cleanup(mock.Close)
	t.Setenv("ANTHROPIC_BASE_URL", mock.URL())
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "gap-injection-test-token")
	return mock
}

// TestEnqueueLifecycle: idle sessions reject (Submit instead), closed
// sessions stay closed — Enqueue never parks a line with no run to join.
func TestEnqueueLifecycle(t *testing.T) {
	s := newOutputSession(t)
	if err := s.Enqueue("hello"); err != ErrNoPromptRunning {
		t.Fatalf("idle Enqueue = %v, want ErrNoPromptRunning", err)
	}
	// newOutputSession skips NewSession's context wiring — give the bare
	// session a real cancelable pair so Close models an actual shutdown.
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.Close()
	if err := s.Enqueue("after close"); err != ErrSessionClosed {
		t.Fatalf("closed Enqueue = %v, want ErrSessionClosed", err)
	}
}

// TestQueueGapInjection: a line Enqueued while a prompt runs joins the
// conversation at a turn gap — queue_injected fires before prompt_end, the
// wire body carries the text (together with the round's tool_results), the
// history holds it exactly once, the audit records the delivery as a
// prompt_start but never the noisy event itself, and nothing leaks into
// the inbox after the run.
func TestQueueGapInjection(t *testing.T) {
	mock := newGapMock(t, []mockapi.Turn{
		{Tool: &mockapi.ToolCall{Name: "noop", Input: "{}"}},
		{Text: []string{"task done"}},
	})
	home := tempDir(t)
	t.Setenv("LYON_HOME", home)

	reg := tools.NewRegistry()
	if err := tools.RegisterFunc(reg, "noop", "does nothing", false,
		func(ctx context.Context, _ struct{}) (string, error) { return "ok", nil }); err != nil {
		t.Fatal(err)
	}
	eng := NewEngine(Config{Workspace: tempDir(t)}, reg)
	t.Cleanup(eng.Close)
	sess := eng.NewSession(nil)
	t.Cleanup(sess.Close)

	if err := sess.Submit("original-task-marker"); err != nil {
		t.Fatal(err)
	}
	if err := sess.Enqueue("INJECTED-MARKER"); err != nil {
		t.Fatalf("Enqueue while running: %v", err)
	}

	sawInjected := false
	deadline := time.After(15 * time.Second)
collect:
	for {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				t.Fatal("event stream closed while waiting for prompt_end")
			}
			switch ev.Type {
			case EventQueueInjected:
				if ev.Text != "INJECTED-MARKER" {
					t.Fatalf("injected text = %q", ev.Text)
				}
				if sawInjected {
					t.Fatal("queue_injected delivered twice")
				}
				sawInjected = true
			case EventPromptEnd:
				if ev.Err != "" {
					t.Fatalf("prompt failed: %s", ev.Err)
				}
				break collect
			}
		case <-deadline:
			t.Fatal("timeout waiting for prompt_end")
		}
	}
	if !sawInjected {
		t.Fatal("queue_injected never arrived before prompt_end")
	}

	// Wire: the last request carries the injected line — and, since the
	// script's first round called a tool, the merged turn that also holds
	// the tool_results the runner appended after it.
	body := mock.LastBody()
	if !bytes.Contains(body, []byte("INJECTED-MARKER")) {
		t.Fatalf("last POST missing the injected line:\n%.2000s", body)
	}
	if !bytes.Contains(body, []byte("tool_result")) {
		t.Fatalf("last POST missing tool_results:\n%.2000s", body)
	}

	// History: exactly one delivery — the prompt-end sweep merged the
	// injected turn into its neighbour, it never double-appended.
	hits := 0
	for _, msg := range sess.Export().Messages {
		for _, b := range msg.Content {
			if b.OfText != nil && strings.Contains(b.OfText.Text, "INJECTED-MARKER") {
				hits++
			}
		}
	}
	if hits != 1 {
		t.Fatalf("injected line appears in %d history blocks, want 1", hits)
	}

	// Nothing left to deliver into the next run (no double-send source).
	sess.mu.Lock()
	leftover := len(sess.inbox)
	sess.mu.Unlock()
	if leftover != 0 {
		t.Fatalf("inbox leaked %d entries past prompt_end", leftover)
	}

	// Audit: delivery is a prompt_start (two of them — initial + injected);
	// the queue_injected event itself is stream noise and stays out.
	eng.Hooks().Flush()
	raw, err := os.ReadFile(filepath.Join(home, "audit", time.Now().Format("2006-01-02")+".jsonl"))
	if err != nil {
		t.Fatalf("audit file: %v", err)
	}
	audit := string(raw)
	if n := strings.Count(audit, `"event":"prompt_start"`); n != 2 {
		t.Fatalf("prompt_start count = %d, want 2:\n%s", n, audit)
	}
	if !strings.Contains(audit, "injected at a turn gap") {
		t.Fatalf("injected prompt_start missing its marker:\n%s", audit)
	}
	if strings.Contains(audit, "queue_injected") {
		t.Fatalf("queue_injected must not be audited:\n%s", audit)
	}
	if got := eng.Hooks().Stats().Prompts; got != 2 {
		t.Fatalf("Stats().Prompts = %d, want 2 (initial + injected)", got)
	}
}
