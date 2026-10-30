package tools

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// waitFor polls fn until it holds or the deadline passes — bg tests
// synchronize on observed state, never on fixed sleeps.
func waitFor(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func bgCount() int {
	bgMu.Lock()
	defer bgMu.Unlock()
	return len(bgByID)
}

// TestShellBackgroundNotifiesOnExit: background=true returns immediately
// (no wall clock), streams the command's output to a log inside the
// workspace, and the owner callback lands exactly once with the tail.
func TestShellBackgroundNotifiesOnExit(t *testing.T) {
	sh := &Shell{Root: tempDir(t)}
	got := make(chan BgInfo, 1)
	ctx := WithBackgroundOwner(context.Background(), "t-bg-exit", func(b BgInfo) { got <- b })

	in, _ := json.Marshal(shellInput{Command: "echo lyon-bg-marker", Background: true})
	start := time.Now()
	out, err := sh.Execute(ctx, in)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("background start blocked %v — it must return immediately", el)
	}
	text := out[0].OfText.Text
	if !strings.Contains(text, "background task #") || !strings.Contains(text, "log:") {
		t.Fatalf("start message odd: %q", text)
	}

	select {
	case b := <-got:
		if b.Code != 0 {
			t.Errorf("code = %d (err %q), want 0", b.Code, b.Err)
		}
		if !strings.Contains(b.Tail, "lyon-bg-marker") {
			t.Errorf("tail = %q, want the marker", b.Tail)
		}
		data, err := os.ReadFile(b.Log)
		if err != nil || !strings.Contains(string(data), "lyon-bg-marker") {
			t.Errorf("log %s unreadable or missing marker: %v", b.Log, err)
		}
		if b.ElapsedMS < 0 {
			t.Errorf("elapsed = %d", b.ElapsedMS)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no completion callback within 5s")
	}
	waitFor(t, "registry drained", func() bool { return bgCount() == 0 })
}

// TestShellBackgroundKilledWithOwner: KillBackgroundOwner takes down the
// task's whole tree fast (Windows: job Terminate), the waiter reaps, and
// the callback reports a non-clean end.
func TestShellBackgroundKilledWithOwner(t *testing.T) {
	sh := &Shell{Root: tempDir(t)}
	got := make(chan BgInfo, 1)
	ctx := WithBackgroundOwner(context.Background(), "t-bg-kill", func(b BgInfo) { got <- b })

	in, _ := json.Marshal(shellInput{Command: "sleep 30", Background: true})
	if _, err := sh.Execute(ctx, in); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitFor(t, "task registered", func() bool { return bgCount() == 1 })

	killAt := time.Now()
	KillBackgroundOwner("t-bg-kill")
	select {
	case b := <-got:
		if b.Code == 0 && b.Err == "" {
			t.Errorf("killed task reported a clean exit: %+v", b)
		}
		if el := time.Since(killAt); el > 5*time.Second {
			t.Errorf("kill took %v — tree teardown must be fast", el)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("kill did not end the task")
	}
	waitFor(t, "registry drained", func() bool { return bgCount() == 0 })
	// Idempotent: a second sweep of the same key is a no-op, not a panic.
	KillBackgroundOwner("t-bg-kill")
	KillBackgroundOwner("") // empty key: nothing to do
}

// TestShellKillAllBackground: engine-shutdown sweep reaches tasks under
// every key at once.
func TestShellKillAllBackground(t *testing.T) {
	sh := &Shell{Root: tempDir(t)}
	got := make(chan BgInfo, 2)
	ctxA := WithBackgroundOwner(context.Background(), "t-all-a", func(b BgInfo) { got <- b })
	ctxB := WithBackgroundOwner(context.Background(), "t-all-b", func(b BgInfo) { got <- b })

	for _, ctx := range []context.Context{ctxA, ctxB} {
		in, _ := json.Marshal(shellInput{Command: "sleep 30", Background: true})
		if _, err := sh.Execute(ctx, in); err != nil {
			t.Fatalf("start: %v", err)
		}
	}
	waitFor(t, "both tasks registered", func() bool { return bgCount() == 2 })

	KillAllBackground()
	for i := 0; i < 2; i++ {
		select {
		case <-got:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of 2 tasks reported after KillAllBackground", i)
		}
	}
	waitFor(t, "registry drained", func() bool { return bgCount() == 0 })
}

// TestShellForegroundTimeoutParam: the adapter no longer wraps
// run_shell — the tool's own clock is the only wall clock. A tiny
// configured default kills a declared-long sleep; timeout_s outranks it.
func TestShellForegroundTimeoutParam(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	sh := &Shell{Root: tempDir(t), Timeout: 400 * time.Millisecond}

	in, _ := json.Marshal(shellInput{Command: "sleep 5"})
	start := time.Now()
	if _, err := sh.Execute(context.Background(), in); err == nil {
		t.Fatal("the configured default must kill sleep 5")
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("default clock took %v, want ~400ms", el)
	}

	// 10s not 2s: full-suite CPU starvation (Windows spawn latency) must
	// not false-fail this; the 25× gap over the 400ms default keeps the
	// regression signal sharp either way.
	in, _ = json.Marshal(shellInput{Command: "sleep 1", TimeoutS: 10})
	start = time.Now()
	out, err := sh.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("timeout_s=10 must outrank the 400ms default: %v", err)
	}
	if el := time.Since(start); el < 800*time.Millisecond {
		t.Fatalf("returned after %v — the 1s sleep did not run to completion", el)
	}
	if !strings.Contains(out[0].OfText.Text, "exit_code: 0") {
		t.Fatalf("want clean exit, got %q", out[0].OfText.Text)
	}
}

// TestShellTimeoutFor: default < per-call < clamp; non-positive falls back.
func TestShellTimeoutFor(t *testing.T) {
	sh := &Shell{}
	if got := sh.timeoutFor(0); got != shellDefaultTimeout {
		t.Errorf("bare default = %v, want %v", got, shellDefaultTimeout)
	}
	sh.Timeout = 30 * time.Second
	if got := sh.timeoutFor(0); got != 30*time.Second {
		t.Errorf("configured default = %v, want 30s", got)
	}
	if got := sh.timeoutFor(-5); got != 30*time.Second {
		t.Errorf("negative timeout_s = %v, want the configured default", got)
	}
	if got := sh.timeoutFor(45); got != 45*time.Second {
		t.Errorf("timeout_s=45 = %v, want 45s", got)
	}
	if got := sh.timeoutFor(100000); got != shellTimeoutMax {
		t.Errorf("timeout_s=100000 = %v, want the %v clamp", got, shellTimeoutMax)
	}
}
