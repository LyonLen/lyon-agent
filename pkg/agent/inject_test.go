package agent

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

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

// TestRequestAgain pins the final-gap predicate to the SDK's step mapping
// (determineNextStepFromStopReason + nextRequest's budget refusal):
// continuing turns post unless the budget refuses, terminal stops never do.
// The difference between those answers is delivering a parked line versus
// announcing a POST that never happens.
func TestRequestAgain(t *testing.T) {
	turn := func(stop anthropic.BetaStopReason) *anthropic.BetaMessage {
		var m anthropic.BetaMessage
		m.StopReason = stop
		return &m
	}
	cases := []struct {
		name          string
		lm            *anthropic.BetaMessage
		iter, maxIter int
		want          bool
	}{
		{"fresh run posts", nil, 0, 5, true},
		{"tool round posts", turn(anthropic.BetaStopReasonToolUse), 1, 5, true},
		{"paused turn resumes", turn(anthropic.BetaStopReasonPauseTurn), 1, 5, true},
		{"compaction resumes", turn(anthropic.BetaStopReasonCompaction), 1, 5, true},
		{"fresh runner under a shrunk budget posts", nil, 1, 5, true},
		{"cap refuses a tool round", turn(anthropic.BetaStopReasonToolUse), 5, 5, false},
		{"cap refuses a paused turn", turn(anthropic.BetaStopReasonPauseTurn), 5, 5, false},
		{"cap refuses a fresh runner", nil, 5, 5, false},
		{"cap disabled posts", turn(anthropic.BetaStopReasonToolUse), 3, 0, true},
		{"end_turn is final", turn(anthropic.BetaStopReasonEndTurn), 0, 5, false},
		{"max_tokens is final", turn(anthropic.BetaStopReasonMaxTokens), 0, 5, false},
		{"stop_sequence is final", turn(anthropic.BetaStopReasonStopSequence), 0, 5, false},
		{"refusal is final", turn(anthropic.BetaStopReasonRefusal), 0, 5, false},
		{"context overflow is final", turn(anthropic.BetaStopReasonModelContextWindowExceeded), 0, 5, false},
		{"omitted stop_reason finalizes like end_turn", turn(""), 1, 5, false},
		{"unknown stop finalizes", turn("something_new"), 0, 5, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := requestAgain(tc.lm, tc.iter, tc.maxIter); got != tc.want {
				t.Fatalf("requestAgain(%v, %d, %d) = %v, want %v",
					tc.lm.StopReason, tc.iter, tc.maxIter, got, tc.want)
			}
		})
	}
}

// newParkedMock is newGapMock with a long pre-response stall: Enqueue
// lands mid-request — after the first loop-top gap has gone by (it runs
// microseconds after Submit) and before the run's final gap — which is the
// only way a test can reach the final gap from outside the drive.
func newParkedMock(t *testing.T, script []mockapi.Turn) *mockapi.Server {
	t.Helper()
	mock := mockapi.New(mockapi.Config{Script: script, Latency: 100 * time.Millisecond})
	t.Cleanup(mock.Close)
	t.Setenv("ANTHROPIC_BASE_URL", mock.URL())
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "gap-injection-test-token")
	return mock
}

// parkDuringRun enqueues the marker line inside the mock's stall window:
// long past the first loop-top gap, well before the response unblocks the
// run's next gap.
func parkDuringRun(t *testing.T, sess *Session, marker string) {
	t.Helper()
	time.Sleep(30 * time.Millisecond)
	if err := sess.Enqueue(marker); err != nil {
		t.Fatalf("Enqueue mid-run: %v", err)
	}
}

// drainUntilPromptEnd reads the session's stream to prompt_end, reporting
// whether queue_injected fired on the way.
func drainUntilPromptEnd(t *testing.T, sess *Session) bool {
	t.Helper()
	sawInjected := false
	deadline := time.After(15 * time.Second)
	for {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				t.Fatal("event stream closed while waiting for prompt_end")
			}
			switch ev.Type {
			case EventQueueInjected:
				sawInjected = true
			case EventPromptEnd:
				if ev.Err != "" {
					t.Fatalf("prompt failed: %s", ev.Err)
				}
				return sawInjected
			}
		case <-deadline:
			t.Fatal("timeout waiting for prompt_end")
		}
	}
}

// assertDeliveredOrParked pins the whole delivery contract for a line
// parked mid-run: either queue_injected fired AND a request carried it (the
// client may drop its copy), or neither happened (the client keeps its
// copy for prompt_end to re-send). Announced-but-unsent is the reported
// bug — the transcript shows the line and nothing ever processes it;
// in-history-but-unannounced would double the line after the re-send.
func assertDeliveredOrParked(t *testing.T, sess *Session, mock *mockapi.Server, marker string, sawInjected bool) {
	t.Helper()
	onWire := bytes.Contains(mock.LastBody(), []byte(marker))
	if sawInjected && !onWire {
		t.Fatalf("queue_injected announced %q but no request ever carried it — the client drops its copy for a delivery that never happens", marker)
	}
	if !sawInjected && onWire {
		t.Fatalf("%q reached the wire without queue_injected — prompt_end would re-send the client's copy: double delivery", marker)
	}
	hits := 0
	for _, msg := range sess.Export().Messages {
		for _, b := range msg.Content {
			if b.OfText != nil && strings.Contains(b.OfText.Text, marker) {
				hits++
			}
		}
	}
	if onWire && hits != 1 {
		t.Fatalf("delivered line appears in %d history blocks, want 1", hits)
	}
	if !onWire && hits != 0 {
		t.Fatalf("line landed in %d history blocks without ever reaching the wire", hits)
	}
	sess.mu.Lock()
	leftover := len(sess.inbox)
	sess.mu.Unlock()
	if leftover != 0 {
		t.Fatalf("inbox leaked %d entries past prompt_end", leftover)
	}
}

// TestQueueSkippedOnFinalGap: a line Enqueued while the LAST turn is still
// streaming (the reported case — typed during the final thinking, the ReAct
// loop exits) must not be drained into the finished run. The ghost gap
// after end_turn has no request behind it: draining there would announce
// queue_injected for a POST that never happens and the client, told to
// drop its copy, would re-send nothing. The stall window parks the line
// exactly there; the invariant then holds whichever way the timing falls.
func TestQueueSkippedOnFinalGap(t *testing.T) {
	mock := newParkedMock(t, []mockapi.Turn{
		{Text: []string{"task done"}}, // one end_turn turn — the only later gap is the ghost
	})
	home := tempDir(t)
	t.Setenv("LYON_HOME", home)

	reg := tools.NewRegistry()
	eng := NewEngine(Config{Workspace: tempDir(t)}, reg)
	t.Cleanup(eng.Close)
	sess := eng.NewSession(nil)
	t.Cleanup(sess.Close)

	if err := sess.Submit("original-task-marker"); err != nil {
		t.Fatal(err)
	}
	parkDuringRun(t, sess, "PARKED-ON-FINAL-GAP")

	assertDeliveredOrParked(t, sess, mock, "PARKED-ON-FINAL-GAP",
		drainUntilPromptEnd(t, sess))
}

// TestQueueSkippedOnIterationCap: the other final gap — the last turn asks
// for tools but the iteration budget is spent, so nextRequest refuses
// before executing anything. Same contract as the end_turn ghost.
func TestQueueSkippedOnIterationCap(t *testing.T) {
	mock := newParkedMock(t, []mockapi.Turn{
		{Tool: &mockapi.ToolCall{Name: "noop", Input: "{}"}},
		{Text: []string{"never reached"}}, // cap hit: the second round never POSTs
	})
	home := tempDir(t)
	t.Setenv("LYON_HOME", home)

	reg := tools.NewRegistry()
	if err := tools.RegisterFunc(reg, "noop", "does nothing", false,
		func(ctx context.Context, _ struct{}) (string, error) { return "ok", nil }); err != nil {
		t.Fatal(err)
	}
	eng := NewEngine(Config{Workspace: tempDir(t), MaxIterations: 1}, reg)
	t.Cleanup(eng.Close)
	sess := eng.NewSession(nil)
	t.Cleanup(sess.Close)

	if err := sess.Submit("original-task-marker"); err != nil {
		t.Fatal(err)
	}
	parkDuringRun(t, sess, "PARKED-ON-CAP-GAP")

	assertDeliveredOrParked(t, sess, mock, "PARKED-ON-CAP-GAP",
		drainUntilPromptEnd(t, sess))
}
