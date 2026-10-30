package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"lyon-agent/pkg/tools"
)

func runShellTool(t *testing.T, sess *Session) anthropic.BetaTool {
	t.Helper()
	for _, bt := range sess.betaTools {
		if bt.Name() == "run_shell" {
			return bt
		}
	}
	t.Fatal("run_shell missing from the adapted tools")
	return nil
}

// TestRunShellOwnClockBeatsToolTimeout: the adapter must NOT wrap
// run_shell in the fixed ToolTimeout (the exemption in newSession) —
// the tool's own clock is the only wall clock. The configured default
// still kills a runaway command, and timeout_s outranks both the default
// AND the 400ms adapter budget that would exist without the exemption.
func TestRunShellOwnClockBeatsToolTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	sess := subTimeoutSession(t, subTimeoutConfig(t, 400*time.Millisecond), nil)
	sh := runShellTool(t, sess)

	// No timeout_s: the default (= ToolTimeout, via Shell.Timeout) kills it.
	in, _ := json.Marshal(map[string]string{"command": "sleep 5"})
	start := time.Now()
	if _, err := sh.Execute(sess.streamCtx, in); err == nil {
		t.Fatal("the default wall clock must kill sleep 5")
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("default clock took %v, want ~400ms", el)
	}

	// timeout_s=10: if the adapter still wrapped the tool at 400ms, this
	// would die at 400ms regardless of the inner clock. The budget is 10s,
	// not 2s, so a full `go test ./...` (Windows process spawn under CPU
	// starvation occasionally eats seconds) can't false-fail it — the 25×
	// gap over the 400ms adapter clock keeps the regression signal sharp.
	in, _ = json.Marshal(map[string]any{"command": "sleep 1", "timeout_s": 10})
	start = time.Now()
	out, err := sh.Execute(sess.streamCtx, in)
	if err != nil {
		t.Fatalf("timeout_s=10 must survive ToolTimeout=400ms: %v", err)
	}
	if el := time.Since(start); el < 800*time.Millisecond {
		t.Fatalf("returned after %v — the 1s sleep did not run to completion", el)
	}
	if !strings.Contains(out[0].OfText.Text, "exit_code: 0") {
		t.Fatalf("want clean exit, got %q", out[0].OfText.Text)
	}
}

// TestOnBgEndDelivery: chat always hears (background_end on the stream);
// the model hears through the live channel — a system note when idle,
// the inbox for gap injection when a run is in flight.
func TestOnBgEndDelivery(t *testing.T) {
	sess := subTimeoutSession(t, subTimeoutConfig(t, time.Minute), nil)

	// Idle: the tail lands as the next prompt's system note.
	sess.onBgEnd(tools.BgInfo{
		ID: 3, Code: 0, Log: filepath.Join("ws", "bg.log"),
		Tail: "ok-done", ElapsedMS: 65000,
	})
	note := sess.takePendingNote()
	for _, want := range []string{"background task #3", "exit 0", "1m05s", "ok-done", "bg.log"} {
		if !strings.Contains(note, want) {
			t.Fatalf("system note missing %q: %q", want, note)
		}
	}

	// The event hit the stream with the render attributes.
	select {
	case ev := <-sess.Events():
		if ev.Type != EventBackgroundEnd {
			t.Fatalf("event type = %q, want %q", ev.Type, EventBackgroundEnd)
		}
		if ev.Detail == "" || ev.Attrs["task"] != "3" || ev.Attrs["code"] != "0" ||
			ev.Attrs["tail"] != "ok-done" || ev.Attrs["log"] != filepath.Join("ws", "bg.log") {
			t.Fatalf("event = %+v, want task/code/log/tail attrs", ev)
		}
		if ev.IsError {
			t.Error("exit 0 marked as error")
		}
	default:
		t.Fatal("no background_end event on the stream")
	}

	// A failed completion: marked as error, note keeps the tail.
	sess.onBgEnd(tools.BgInfo{ID: 4, Code: 1, Log: "l", Tail: "boom", ElapsedMS: 10})
	note = sess.takePendingNote()
	if !strings.Contains(note, "exit 1") || !strings.Contains(note, "boom") {
		t.Fatalf("failed note = %q", note)
	}
	select {
	case ev := <-sess.Events():
		if !ev.IsError || ev.Attrs["task"] != "4" {
			t.Fatalf("failed event = %+v", ev)
		}
	default:
		t.Fatal("no event for the failed completion")
	}

	// Live run: the line parks in the inbox for the next turn gap instead.
	sess.busy.Store(true)
	sess.onBgEnd(tools.BgInfo{ID: 5, Code: 0, Log: "l5", Tail: "t5", ElapsedMS: 1})
	if sess.QueueLen() != 1 {
		t.Fatalf("queue = %d, want the completion parked for gap injection", sess.QueueLen())
	}
	if note := sess.takePendingNote(); note != "" {
		t.Fatalf("live run must deliver via inbox, not a note: %q", note)
	}
	sess.mu.Lock()
	sess.inbox = nil
	sess.mu.Unlock()
	sess.busy.Store(false)
}

// TestSessionCloseKillsBackground: the full wiring — a detached task
// started through the session's own streamCtx keeps ticking until Close,
// which takes the tree down (and the completion notify lands on a
// closing stream without panicking: emit vs close(events) is the race
// this exercises).
func TestSessionCloseKillsBackground(t *testing.T) {
	workspace := tempDir(t)
	cfg := subTimeoutConfig(t, time.Minute)
	cfg.Workspace = workspace
	sess := subTimeoutSession(t, cfg, nil)
	sh := runShellTool(t, sess)

	marker := filepath.Join(workspace, "tick.txt")
	in, _ := json.Marshal(map[string]any{
		"command":    "while true; do echo tick; sleep 0.2; done > tick.txt",
		"background": true,
	})
	if _, err := sh.Execute(sess.streamCtx, in); err != nil {
		t.Fatalf("start: %v", err)
	}
	size := func() int64 {
		st, err := os.Stat(marker)
		if err != nil {
			return 0
		}
		return st.Size()
	}
	waitForTest(t, "the task to start ticking", func() bool { return size() > 0 })

	sess.Close() // idempotent with the fixture's cleanup
	time.Sleep(300 * time.Millisecond) // let the kill land; in-flight writes flush
	before := size()
	time.Sleep(600 * time.Millisecond) // >2 write cycles if it were alive
	if after := size(); after != before {
		t.Fatalf("background tree survived Close: %d → %d bytes", before, after)
	}
}

// waitForTest polls fn until it holds or 5s pass (local twin of the
// tools package helper — agent tests have no shared wait utility).
func waitForTest(t *testing.T, what string, fn func() bool) {
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

func TestBgDurAndClip(t *testing.T) {
	if got := bgDur(8600); got != "8.6s" {
		t.Errorf("bgDur(8600) = %q", got)
	}
	if got := bgDur(65000); got != "1m05s" {
		t.Errorf("bgDur(65000) = %q", got)
	}
	if got := clipTail(strings.Repeat("x", 700), 600); got != strings.Repeat("x", 600)+"…" {
		t.Errorf("clipTail = len %d, want 600 bytes + ellipsis", len(got))
	}
	if got := clipTail("short", 600); got != "short" {
		t.Errorf("clipTail(short) = %q", got)
	}
}
