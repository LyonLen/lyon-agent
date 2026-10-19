package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lyon-agent/pkg/memory"
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

// memWorkspace isolates one test's memory: a fresh workspace (so the
// package-level Store cache keys apart) with LYON_HOME redirected inside
// it — team/personal dirs would otherwise touch the developer's real
// ~/.lyon. t.Setenv restores the environment after the test.
func memWorkspace(t *testing.T) string {
	t.Helper()
	ws := tempDir(t)
	t.Setenv("LYON_HOME", filepath.Join(ws, ".lyon-home"))
	t.Setenv("LYON_SYNC_URL", "")
	return ws
}

// call invokes a registered tool through Execute and joins text blocks —
// the same path the model's tool_use takes.
func call(t *testing.T, reg *Registry, name, input string) (string, error) {
	t.Helper()
	tool, ok := reg.Get(name)
	if !ok {
		t.Fatalf("%s not registered", name)
	}
	out, err := tool.Execute(context.Background(), []byte(input))
	if err != nil {
		return "", err
	}
	joined := ""
	for _, c := range out {
		if c.OfText != nil {
			joined += c.OfText.Text
		}
	}
	return joined, nil
}

func mustCall(t *testing.T, reg *Registry, name, input string) string {
	t.Helper()
	out, err := call(t, reg, name, input)
	if err != nil {
		t.Fatalf("%s(%s): %v", name, input, err)
	}
	return out
}

func TestMemorySaveReadForget(t *testing.T) {
	reg := NewRegistry()
	if err := RegisterMemory(reg, memWorkspace(t)); err != nil {
		t.Fatal(err)
	}
	// Save a new entry (default scope: project).
	msg := mustCall(t, reg, "save_memory", `{"title":"project codename","content":"the project is lychee"}`)
	if !strings.Contains(msg, "project codename") || !strings.Contains(msg, "project scope") {
		t.Fatalf("save message: %q", msg)
	}
	// List everything (compact: scope, slug, description — body via title read).
	all := mustCall(t, reg, "read_memory", `{}`)
	if !strings.Contains(all, "project-codename") || !strings.Contains(all, "project codename") {
		t.Fatalf("list: %q", all)
	}
	// Read one by title (any case → same slug).
	one := mustCall(t, reg, "read_memory", `{"title":"Project Codename"}`)
	if !strings.Contains(one, "lychee") {
		t.Fatalf("read one: %q", one)
	}
	// Upsert: same title (different case) updates in place, no duplicate.
	mustCall(t, reg, "save_memory", `{"title":"PROJECT codename","content":"renamed: dragnet"}`)
	all = mustCall(t, reg, "read_memory", `{}`)
	if strings.Count(all, "project-codename") != 1 || !strings.Contains(all, "PROJECT codename") {
		t.Fatalf("upsert failed: %q", all)
	}
	if one := mustCall(t, reg, "read_memory", `{"title":"project codename"}`); !strings.Contains(one, "dragnet") {
		t.Fatalf("upserted body: %q", one)
	}
	// Read a missing title errors (becomes an is_error tool result).
	if _, err := call(t, reg, "read_memory", `{"title":"nope"}`); err == nil {
		t.Fatal("missing title must error")
	}
	// Forget it (accepts the human title form).
	mustCall(t, reg, "forget_memory", `{"title":"project codename"}`)
	all = mustCall(t, reg, "read_memory", `{}`)
	if all != "(no memories saved yet)" {
		t.Fatalf("after forget: %q", all)
	}
	// Forgetting a missing title errors.
	if _, err := call(t, reg, "forget_memory", `{"title":"gone"}`); err == nil {
		t.Fatal("forget missing must error")
	}
	// Empty inputs rejected.
	if _, err := call(t, reg, "save_memory", `{"title":" ","content":"x"}`); err == nil {
		t.Fatal("empty title must error")
	}
	if _, err := call(t, reg, "save_memory", `{"title":"x","content":" "}`); err == nil {
		t.Fatal("empty content must error")
	}
	// Bogus scope rejected with guidance.
	if _, err := call(t, reg, "save_memory", `{"title":"x","content":"y","scope":"bogus"}`); err == nil {
		t.Fatal("bad scope must error")
	}
}

func TestMemoryScopesAndSearch(t *testing.T) {
	reg := NewRegistry()
	if err := RegisterMemory(reg, memWorkspace(t)); err != nil {
		t.Fatal(err)
	}
	// Facts in two scopes, one sharing a slug across scopes.
	mustCall(t, reg, "save_memory",
		`{"title":"dark mode","content":"user prefers dark mode everywhere","scope":"personal"}`)
	mustCall(t, reg, "save_memory",
		`{"title":"deploy checklist","content":"上下文窗口 sanity: run go test before deploy","scope":"project"}`)
	// BM25 query search finds ASCII and CJK content across scopes.
	got := mustCall(t, reg, "read_memory", `{"query":"dark mode preference"}`)
	if !strings.Contains(got, "(personal)") || !strings.Contains(got, "dark mode") {
		t.Fatalf("ascii query: %q", got)
	}
	got = mustCall(t, reg, "read_memory", `{"query":"上下文窗口"}`)
	if !strings.Contains(got, "(project)") || !strings.Contains(got, "go test") {
		t.Fatalf("cjk query: %q", got)
	}
	// Query + scope filter: only matching scope survives.
	got = mustCall(t, reg, "read_memory", `{"query":"dark mode","scope":"project"}`)
	if strings.Contains(got, "prefers dark mode") {
		t.Fatalf("scope filter leaked: %q", got)
	}
	// Same slug in two scopes: default reads the closer (project) copy;
	// scope= pins the other.
	mustCall(t, reg, "save_memory", `{"title":"build command","content":"project says: go build ./..."}`)
	mustCall(t, reg, "save_memory", `{"title":"build command","content":"personal says: go build -tags dev","scope":"personal"}`)
	got = mustCall(t, reg, "read_memory", `{"title":"build command"}`)
	if !strings.Contains(got, "go build ./...") || !strings.Contains(got, "(project)") {
		t.Fatalf("default scope read: %q", got)
	}
	got = mustCall(t, reg, "read_memory", `{"title":"build command","scope":"personal"}`)
	if !strings.Contains(got, "tags dev") || !strings.Contains(got, "(personal)") {
		t.Fatalf("scoped read: %q", got)
	}
	// List respects the scope filter.
	got = mustCall(t, reg, "read_memory", `{"scope":"personal"}`)
	if strings.Contains(got, "(project)") {
		t.Fatalf("list leaked project facts: %q", got)
	}
	// Forget with scope removes only that copy; the other survives.
	mustCall(t, reg, "forget_memory", `{"title":"build command","scope":"personal"}`)
	got = mustCall(t, reg, "read_memory", `{"title":"build command"}`)
	if !strings.Contains(got, "go build ./...") {
		t.Fatalf("project copy lost: %q", got)
	}
	if _, err := call(t, reg, "read_memory", `{"title":"build command","scope":"personal"}`); err == nil {
		t.Fatal("personal copy should be gone")
	}
}

func TestMemoryFactsInjection(t *testing.T) {
	ws := memWorkspace(t)
	reg := NewRegistry()
	if err := RegisterMemory(reg, ws); err != nil {
		t.Fatal(err)
	}
	// Empty store still renders the write-path guidance (cold-start
	// deadlock fix) and carries no facts.
	if got := MemoryFacts(ws); !strings.Contains(got, "save_memory") || strings.Contains(got, "## (") {
		t.Fatalf("empty workspace must render guidance only, got %q", got)
	}
	// Non-hot facts stay out of the injected tier (guidance still ships).
	mustCall(t, reg, "save_memory", `{"title":"cold fact","content":"not resident"}`)
	if got := MemoryFacts(ws); strings.Contains(got, "not resident") {
		t.Fatalf("cold fact leaked into hot tier: %q", got)
	}
	// hot=true pins it; MemoryFacts renders name and body.
	mustCall(t, reg, "save_memory", `{"title":"prefers tests","content":"always run go test -count=1","hot":true}`)
	got := MemoryFacts(ws)
	if !strings.HasPrefix(got, "[persistent-memory") {
		t.Fatalf("missing sentinel: %q", got)
	}
	if !strings.Contains(got, "prefers-tests") || !strings.Contains(got, "go test -count=1") {
		t.Fatalf("rendering: %q", got)
	}
	if strings.Contains(got, "not resident") {
		t.Fatalf("cold fact rendered: %q", got)
	}
	// Updating the fact must not silently drop its hot status.
	mustCall(t, reg, "save_memory", `{"title":"prefers tests","content":"always run go test -count=1 (updated)"}`)
	if got := MemoryFacts(ws); !strings.Contains(got, "(updated)") {
		t.Fatalf("hot lost on update: %q", got)
	}
}

func TestMemoryContentClip(t *testing.T) {
	reg := NewRegistry()
	if err := RegisterMemory(reg, memWorkspace(t)); err != nil {
		t.Fatal(err)
	}
	huge := strings.Repeat("字", memoryMaxContent+500)
	mustCall(t, reg, "save_memory", `{"title":"huge","content":"`+huge+`"}`)
	got := mustCall(t, reg, "read_memory", `{"title":"huge"}`)
	if !strings.Contains(got, "... [truncated]") {
		t.Fatal("oversized content not clipped")
	}
	if r := []rune(got); len(r) > memoryMaxContent+200 {
		t.Fatalf("content not bounded: %d runes", len(r))
	}
}

func TestRegisterMemory(t *testing.T) {
	reg := NewRegistry()
	if err := RegisterMemory(reg, memWorkspace(t)); err != nil {
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
	mustCall(t, reg, "save_memory", `{"title":"t1","content":"c1"}`)
	got := mustCall(t, reg, "read_memory", `{"title":"t1"}`)
	if !strings.Contains(got, "c1") {
		t.Fatalf("execute round trip: %q", got)
	}
	// Missing title surfaces as an error (is_error result upstream).
	if _, err := call(t, reg, "read_memory", `{"title":"missing"}`); err == nil {
		t.Fatal("missing title must error through Execute")
	}
}

// TestMemoryLegacyMigrationSanity proves the store the tools talk to is
// the same layered store pkg/memory migrates legacy memory.json into.
func TestMemoryLegacyMigrationSanity(t *testing.T) {
	ws := memWorkspace(t)
	// Drop a legacy single-file memory before the store opens.
	if err := os.MkdirAll(filepath.Join(ws, ".lyon"), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `{"entries":[{"title":"部署环境","content":"prod 在阿里云济南","updated_at":"2026-01-02T03:04:05Z"}]}`
	if err := os.WriteFile(filepath.Join(ws, ".lyon", "memory.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	st := memory.Open(ws) // first open migrates
	_ = st
	reg := NewRegistry()
	if err := RegisterMemory(reg, ws); err != nil {
		t.Fatal(err)
	}
	got := mustCall(t, reg, "read_memory", `{"query":"阿里云"}`)
	if !strings.Contains(got, "prod") || !strings.Contains(got, "(project)") {
		t.Fatalf("legacy import not searchable: %q", got)
	}
}

// TestMemoryChangeSeam: save/forget successes notify the listener attached
// to the handler ctx (the session's D1 acknowledgment seam), failures stay
// silent, and handlers with no listener run unchanged — probes and direct
// calls must neither panic nor fabricate notifications.
func TestMemoryChangeSeam(t *testing.T) {
	reg := NewRegistry()
	if err := RegisterMemory(reg, memWorkspace(t)); err != nil {
		t.Fatal(err)
	}

	var got []MemoryChange
	ctx := WithMemoryChange(context.Background(), func(c MemoryChange) {
		got = append(got, c)
	})
	save, ok := reg.Get("save_memory")
	if !ok {
		t.Fatal("save_memory not registered")
	}
	if _, err := save.Execute(ctx, []byte(`{"title":"Deploy Gate","content":"gates every deploy"}`)); err != nil {
		t.Fatalf("save: %v", err)
	}
	forget, ok := reg.Get("forget_memory")
	if !ok {
		t.Fatal("forget_memory not registered")
	}
	if _, err := forget.Execute(ctx, []byte(`{"title":"Deploy Gate"}`)); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("notified %d times, want 2: %+v", len(got), got)
	}
	if got[0].Action != "save" || got[0].Name != "deploy-gate" || got[0].Scope != "project" {
		t.Fatalf("save change = %+v", got[0])
	}
	if got[1].Action != "forget" || got[1].Name != "deploy-gate" || got[1].Scope != "project" {
		t.Fatalf("forget change = %+v", got[1])
	}

	// A failed save writes nothing — it must not notify either.
	if _, err := save.Execute(ctx, []byte(`{"title":" ","content":"x"}`)); err == nil {
		t.Fatal("empty title must error")
	}
	if len(got) != 2 {
		t.Fatalf("failed save notified: %+v", got)
	}

	// No listener: nil fn returns ctx unchanged; the handler still works
	// and the attached listener never fires.
	plain := context.Background()
	if WithMemoryChange(plain, nil) != plain {
		t.Fatal("nil fn must return ctx unchanged")
	}
	if _, err := save.Execute(plain, []byte(`{"title":"Solo","content":"body"}`)); err != nil {
		t.Fatalf("save without listener: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("listener fired without attachment: %+v", got)
	}
}
