package tools

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// tempDir is t.TempDir with a deletion retry: these tests churn atomic
// tmp+rename writes, and on Windows the cleanup occasionally races a
// transient handle (defender/indexer) in %TEMP% — RemoveAll fails with
// "directory not empty" right after emptying it. The retry runs before
// testing's own cleanup (LIFO), turning a ~1-in-4 package flake green.
func tempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() {
		for i := 0; i < 20; i++ {
			if os.RemoveAll(dir) == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	})
	return dir
}

func TestMemorySaveReadForget(t *testing.T) {
	path := filepath.Join(tempDir(t), "memory.json")

	// Save a new entry.
	msg, err := saveFact(path, "project codename", "the project is lychee")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "project codename") {
		t.Fatalf("save message: %q", msg)
	}
	// List everything.
	all, err := readFacts(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(all, "project codename") || !strings.Contains(all, "lychee") {
		t.Fatalf("list: %q", all)
	}
	// Read one by title (case-insensitive).
	one, err := readFacts(path, "Project Codename")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(one, "lychee") {
		t.Fatalf("read one: %q", one)
	}
	// Upsert: same title (different case) updates in place, no duplicate.
	if _, err := saveFact(path, "PROJECT codename", "renamed: dragnet"); err != nil {
		t.Fatal(err)
	}
	all, _ = readFacts(path, "")
	if strings.Count(all, "## ") != 1 || !strings.Contains(all, "dragnet") {
		t.Fatalf("upsert failed: %q", all)
	}
	// Read a missing title errors (becomes an is_error tool result).
	if _, err := readFacts(path, "nope"); err == nil {
		t.Fatal("missing title must error")
	}
	// Forget it.
	if _, err := forgetFact(path, "project codename"); err != nil {
		t.Fatal(err)
	}
	all, _ = readFacts(path, "")
	if all != "(no memories saved yet)" {
		t.Fatalf("after forget: %q", all)
	}
	// Forgetting a missing title errors.
	if _, err := forgetFact(path, "gone"); err == nil {
		t.Fatal("forget missing must error")
	}
	// Empty inputs rejected.
	if _, err := saveFact(path, " ", "x"); err == nil {
		t.Fatal("empty title must error")
	}
	if _, err := saveFact(path, "x", " "); err == nil {
		t.Fatal("empty content must error")
	}
}

func TestMemoryFactsInjection(t *testing.T) {
	ws := tempDir(t)
	if got := MemoryFacts(ws); got != "" {
		t.Fatalf("empty workspace must render nothing, got %q", got)
	}
	if _, err := saveFact(MemoryPath(ws), "prefers tests", "always run go test -count=1"); err != nil {
		t.Fatal(err)
	}
	got := MemoryFacts(ws)
	if !strings.HasPrefix(got, "[persistent-memory]") {
		t.Fatalf("missing sentinel: %q", got)
	}
	if !strings.Contains(got, "## prefers tests") || !strings.Contains(got, "go test -count=1") {
		t.Fatalf("rendering: %q", got)
	}
}

func TestMemoryLimits(t *testing.T) {
	path := filepath.Join(tempDir(t), "memory.json")
	// Content clipped at memoryMaxContent runes with a marker.
	huge := strings.Repeat("字", memoryMaxContent+500)
	if _, err := saveFact(path, "huge", huge); err != nil {
		t.Fatal(err)
	}
	all, _ := readFacts(path, "huge")
	if !strings.Contains(all, "... [truncated]") {
		t.Fatal("oversized content not clipped")
	}
	if r := []rune(all); len(r) > memoryMaxContent+100 {
		t.Fatalf("content not bounded: %d runes", len(r))
	}
	// Entry cap: oldest evicted past memoryMaxEntries.
	for i := 0; i <= memoryMaxEntries; i++ {
		if _, err := saveFact(path, strconv.Itoa(i), "x"); err != nil {
			t.Fatal(err)
		}
	}
	entries := loadMemory(path)
	if len(entries) != memoryMaxEntries {
		t.Fatalf("entries = %d, want %d", len(entries), memoryMaxEntries)
	}
	if entries[0].Title == "0" {
		t.Fatal("oldest entry not evicted")
	}
}

func TestRegisterMemory(t *testing.T) {
	reg := NewRegistry()
	if err := RegisterMemory(reg, filepath.Join(tempDir(t), "memory.json")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"save_memory", "read_memory", "forget_memory"} {
		tool, ok := reg.Get(name)
		if !ok {
			t.Fatalf("%s not registered", name)
		}
		if tool.Name() != name {
			t.Fatalf("name mismatch: %q", tool.Name())
		}
		wantParallel := name == "read_memory"
		if tool.ParallelSafe() != wantParallel {
			t.Fatalf("%s parallelSafe = %v, want %v", name, tool.ParallelSafe(), wantParallel)
		}
	}
	// Handler path through Execute: save then read via the registry.
	save, _ := reg.Get("save_memory")
	if _, err := save.Execute(context.Background(), []byte(`{"title":"t1","content":"c1"}`)); err != nil {
		t.Fatal(err)
	}
	read, _ := reg.Get("read_memory")
	out, err := read.Execute(context.Background(), []byte(`{"title":"t1"}`))
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, c := range out {
		if c.OfText != nil {
			joined += c.OfText.Text
		}
	}
	if !strings.Contains(joined, "c1") {
		t.Fatalf("execute round trip: %q", joined)
	}
	// Missing title surfaces as an error (is_error result upstream).
	if _, err := read.Execute(context.Background(), []byte(`{"title":"missing"}`)); err == nil {
		t.Fatal("missing title must error through Execute")
	}
}
