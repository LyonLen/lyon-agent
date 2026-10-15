package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newClosedCheck builds a manager under a scratch home and registers the
// usual audit/stats assertions against it.
func newScratchManager(t *testing.T) (*Manager, string) {
	t.Helper()
	dir := tempDir(t)
	t.Setenv("LYON_HOME", dir)
	return New("ws"), dir
}

// TestCloseRetiresWriter: Close must drain queued audit lines, persist
// stats, and actually finish the writer goroutine (doneCh only closes from
// writeLoop's defer) — this is the per-Engine goroutine leak fix.
func TestCloseRetiresWriter(t *testing.T) {
	m, dir := newScratchManager(t)

	for i := 0; i < 50; i++ {
		m.Fire(Record{Event: EvToolExecEnd, Tool: "shell", DurationMS: 1})
	}
	m.Close()

	select {
	case <-m.doneCh:
	default:
		t.Fatal("Close returned but the writer goroutine is still running")
	}

	// Drained: every queued line made it to disk during Close.
	auditPath := filepath.Join(dir, "audit", time.Now().Format("2006-01-02")+".jsonl")
	b, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("audit file after Close: %v", err)
	}
	lines := 0
	for _, ln := range bytesLines(b) {
		var r Record
		if err := json.Unmarshal(ln, &r); err != nil {
			t.Fatalf("audit line invalid: %v", err)
		}
		lines++
	}
	if lines != 50 {
		t.Errorf("audit lines = %d, want 50 (queue not drained)", lines)
	}

	// Stats persisted by the drain, no Flush needed.
	sb, err := os.ReadFile(filepath.Join(dir, "stats.json"))
	if err != nil || len(sb) == 0 {
		t.Errorf("stats.json missing after Close: %v", err)
	}
}

// TestCloseIsIdempotentAndUnblocksFlush: double Close is safe, and neither
// Flush nor Fire may hang or touch disk after the manager is finished —
// tests tear their TempDir down right after Close.
func TestCloseIsIdempotentAndUnblocksFlush(t *testing.T) {
	m, dir := newScratchManager(t)
	m.Fire(Record{Event: EvPromptStart, Prompt: "hi"})
	m.Close()
	m.Close() // must not panic on the stop channel

	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	done := make(chan struct{})
	go func() {
		m.Flush() // would block forever on a barrier nobody reads
		m.Fire(Record{Event: EvPromptEnd})
		m.Save()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Flush/Fire after Close hung")
	}

	if _, err := os.Stat(dir); err == nil {
		t.Error("Flush after Close recreated the removed home directory")
	}
}

// TestCloseWithoutAnyFire: a manager that never recorded anything still
// starts and stops its writer cleanly (Close is the one who starts it).
func TestCloseWithoutAnyFire(t *testing.T) {
	m, _ := newScratchManager(t)
	m.Close()
	select {
	case <-m.doneCh:
	default:
		t.Fatal("writer never retired")
	}
	m.Flush()
	m.Close()
}

func bytesLines(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, c := range b {
		if c == '\n' {
			if i > start {
				out = append(out, b[start:i])
			}
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, b[start:])
	}
	return out
}
