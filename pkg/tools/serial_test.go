package tools

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// scopedTool is a mutating test double that names its lock resource, so the
// adapter can serialize per key instead of per session (#32).
type scopedTool struct {
	funcTool
	key string
}

func (t *scopedTool) SerialKey(context.Context, json.RawMessage) string { return t.key }

// overlapProbe counts concurrent executions and remembers the maximum.
type overlapProbe struct {
	mu       sync.Mutex
	running  int
	max      int
	holdTime time.Duration
}

func (p *overlapProbe) run(context.Context) error {
	p.mu.Lock()
	p.running++
	if p.running > p.max {
		p.max = p.running
	}
	p.mu.Unlock()
	time.Sleep(p.holdTime)
	p.mu.Lock()
	p.running--
	p.mu.Unlock()
	return nil
}

func (p *overlapProbe) peak() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.max
}

// runPair executes both tools concurrently.
func runPair(a, b anthropic.BetaTool) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = a.Execute(context.Background(), json.RawMessage(`{}`)) }()
	go func() { defer wg.Done(); _, _ = b.Execute(context.Background(), json.RawMessage(`{}`)) }()
	wg.Wait()
}

func TestSerializerScopesByKey(t *testing.T) {
	// Same key: serialized.
	same := &overlapProbe{holdTime: 60 * time.Millisecond}
	var ser Serializer
	runPair(
		Adapt(&scopedTool{funcTool: funcTool{name: "a", fn: same.run}, key: "file:x"}, AdapterConfig{Serial: &ser}),
		Adapt(&scopedTool{funcTool: funcTool{name: "b", fn: same.run}, key: "file:x"}, AdapterConfig{Serial: &ser}),
	)
	if same.peak() > 1 {
		t.Fatalf("same key overlapped: peak=%d", same.peak())
	}

	// Different keys: concurrent.
	diff := &overlapProbe{holdTime: 60 * time.Millisecond}
	runPair(
		Adapt(&scopedTool{funcTool: funcTool{name: "c", fn: diff.run}, key: "file:x"}, AdapterConfig{Serial: &ser}),
		Adapt(&scopedTool{funcTool: funcTool{name: "d", fn: diff.run}, key: "file:y"}, AdapterConfig{Serial: &ser}),
	)
	if diff.peak() < 2 {
		t.Fatalf("different keys did not overlap: peak=%d", diff.peak())
	}
}

func TestSerializerGlobalExcludesScoped(t *testing.T) {
	// A global holder (shell) must not race any scoped holder (file edit).
	p := &overlapProbe{holdTime: 60 * time.Millisecond}
	var ser Serializer
	runPair(
		Adapt(&funcTool{name: "shell", fn: p.run}, AdapterConfig{Serial: &ser}),
		Adapt(&scopedTool{funcTool: funcTool{name: "edit", fn: p.run}, key: "file:x"}, AdapterConfig{Serial: &ser}),
	)
	if p.peak() > 1 {
		t.Fatalf("global and scoped overlapped: peak=%d", p.peak())
	}
}

func TestSerializerParallelSafeBypasses(t *testing.T) {
	p := &overlapProbe{holdTime: 60 * time.Millisecond}
	var ser Serializer
	runPair(
		Adapt(&funcTool{name: "shell", fn: p.run}, AdapterConfig{Serial: &ser}),
		Adapt(&funcTool{name: "read", parallel: true, fn: p.run}, AdapterConfig{Serial: &ser}),
	)
	if p.peak() < 2 {
		t.Fatalf("parallel-safe tool did not bypass the serial class: peak=%d", p.peak())
	}
}

// TestFileToolsSerialKey: the real write/edit tools name the confined path,
// so two operations on one file share a class and two files do not.
func TestFileToolsSerialKey(t *testing.T) {
	root := tempDir(t)
	w := &WriteFile{Root: root}
	e := &EditFile{Root: root}
	rel := func(p string) json.RawMessage {
		b, _ := json.Marshal(map[string]string{"path": p, "content": "x", "old_string": "x", "new_string": "y"})
		return b
	}
	bg := context.Background()
	if k1, k2 := w.SerialKey(bg, rel("a.txt")), e.SerialKey(bg, rel("a.txt")); k1 != k2 || k1 == "" {
		t.Fatalf("same file keys differ: %q vs %q", k1, k2)
	}
	if w.SerialKey(bg, rel("a.txt")) == w.SerialKey(bg, rel("b.txt")) {
		t.Fatal("different files share a key")
	}
	// An escaping path yields "" so the adapter falls back to exclusive.
	if k := w.SerialKey(bg, rel("../escape.txt")); k != "" {
		t.Fatalf("escape key = %q, want empty", k)
	}
}
