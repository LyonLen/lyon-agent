package memory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// freshStore builds a Store over isolated dirs (LYON_HOME + workspace),
// clearing the package cache so tests never share state. Both dirs use the
// retrying cleanup (scratchTempDir): fact writes are still settling when the
// framework's RemoveAll walks the tree on Windows.
func freshStore(t *testing.T) *Store {
	t.Helper()
	home := scratchTempDir(t)
	ws := scratchTempDir(t)
	t.Setenv("LYON_HOME", home)
	t.Setenv("LYON_SYNC_URL", "")
	storesMu.Lock()
	stores = map[string]*Store{}
	storesMu.Unlock()
	return Open(ws)
}

// TestFactRoundTrip: encode/decode preserves every field, and a
// hand-written file without frontmatter still loads (name from filename).
func TestFactRoundTrip(t *testing.T) {
	f := Fact{
		Name:        "toast-preference",
		Description: "通知要右下角 toast",
		Scope:       ScopePersonal,
		Tags:        []string{"ui", "notification"},
		Hot:         true,
		Created:     time.Date(2026, 10, 10, 18, 0, 0, 0, time.UTC),
		Updated:     time.Date(2026, 10, 10, 18, 30, 0, 0, time.UTC),
		Hits:        3,
		Body:        "第一行\n\n第二行",
	}
	got, err := decodeFact("x.md", ScopePersonal, f.encode())
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != f.Name || got.Description != f.Description || got.Scope != f.Scope ||
		got.Hot != f.Hot || got.Hits != f.Hits || got.Body != f.Body {
		t.Fatalf("roundtrip mismatch:\n got %+v\nwant %+v", got, f)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "ui" {
		t.Errorf("tags: %v", got.Tags)
	}
	if !got.Updated.Equal(f.Updated) {
		t.Errorf("updated: %v", got.Updated)
	}

	plain, err := decodeFact("handwritten.md", ScopeTeam, []byte("just some prose\nwith lines"))
	if err != nil {
		t.Fatal(err)
	}
	if plain.Name != "handwritten" || !strings.Contains(plain.Body, "prose") {
		t.Errorf("frontless decode: %+v", plain)
	}
}

// TestSaveListSearchHot: the full store lifecycle across scopes — save,
// list ordering (project first), BM25 search hits CJK bigrams, hot text
// renders only hot facts within budget.
func TestSaveListSearchHot(t *testing.T) {
	s := freshStore(t)
	must(t, func() (Fact, error) {
		return s.Save(Fact{Name: "context-window", Scope: ScopeTeam,
			Description: "模型上下文窗口大小", Body: "opus 默认 200k 上下文窗口"})
	})
	must(t, func() (Fact, error) {
		return s.Save(Fact{Name: "toast-ui", Scope: ScopePersonal, Hot: true,
			Description: "通知样式", Body: "Windows 通知用右下角 toast，不要老式弹窗"})
	})
	must(t, func() (Fact, error) {
		return s.Save(Fact{Name: "build", Scope: ScopeProject,
			Description: "构建命令", Body: "go build -o bin/ ./cmd/lyon"})
	})

	// List: project beats personal beats team on the way out.
	all := s.List("")
	if len(all) != 3 {
		t.Fatalf("list: %d facts", len(all))
	}
	if all[0].Scope != ScopeProject || all[2].Scope != ScopeTeam {
		t.Errorf("scope order: %s %s %s", all[0].Scope, all[1].Scope, all[2].Scope)
	}

	// CJK bigram search: "上下文" and "窗口" both hit the team fact even
	// though the query as a whole never appears in it verbatim.
	hits := s.Search("上下文窗口大小", 5)
	if len(hits) == 0 || hits[0].Fact.Name != "context-window" {
		t.Fatalf("bm25 cjk search: %+v", hits)
	}

	// Case-insensitive ASCII term.
	if h := s.Search("BUILD", 5); len(h) == 0 || h[0].Fact.Name != "build" {
		t.Errorf("ascii search: %+v", h)
	}

	// Hot text: only the hot fact, with scope tag.
	hot := s.HotText(0)
	if !strings.Contains(hot, "toast-ui") || strings.Contains(hot, "context-window") {
		t.Errorf("hot tier leak:\n%s", hot)
	}
	if !strings.Contains(hot, "(personal)") {
		t.Errorf("hot tier missing scope tag:\n%s", hot)
	}

	// Toggle flips hot and survives reload from disk.
	if on, err := s.ToggleHot(ScopeTeam, "context-window"); err != nil || !on {
		t.Fatalf("toggle on: %v %v", on, err)
	}
	s.Reload()
	if f, ok := s.Get(ScopeTeam, "context-window"); !ok || !f.Hot {
		t.Error("hot flag not persisted")
	}

	// Delete removes file and index entry.
	if err := s.Delete(ScopeProject, "build"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get(ScopeProject, "build"); ok {
		t.Error("deleted fact still visible")
	}
	if h := s.Search("build", 5); len(h) != 0 {
		t.Errorf("deleted fact still indexed: %+v", h)
	}
}

func must(t *testing.T, fn func() (Fact, error)) {
	t.Helper()
	if _, err := fn(); err != nil {
		t.Fatal(err)
	}
}

// TestSearchPrefersCloserScope: identical facts in two scopes rank the
// project copy first.
func TestSearchPrefersCloserScope(t *testing.T) {
	s := freshStore(t)
	must(t, func() (Fact, error) {
		return s.Save(Fact{Name: "shared", Scope: ScopeTeam, Body: "shared decision alpha"})
	})
	must(t, func() (Fact, error) {
		return s.Save(Fact{Name: "shared", Scope: ScopeProject, Body: "shared decision alpha"})
	})
	hits := s.Search("shared decision", 10)
	if len(hits) < 2 {
		t.Fatalf("want both scopes, got %d", len(hits))
	}
	if hits[0].Fact.Scope != ScopeProject {
		t.Errorf("closer scope lost tie: %s first", hits[0].Fact.Scope)
	}
}

// TestMigrationFromLegacyJSON: the old memory.json becomes project facts
// exactly once; the original is parked as memory.json.imported.
func TestMigrationFromLegacyJSON(t *testing.T) {
	home := scratchTempDir(t)
	ws := scratchTempDir(t)
	t.Setenv("LYON_HOME", home)
	storesMu.Lock()
	stores = map[string]*Store{}
	storesMu.Unlock()
	legacy := map[string]any{
		"version": 1,
		"entries": []map[string]any{
			{"title": "Build Command", "content": "go build ./...", "updated_at": time.Now()},
			{"title": "部署环境", "content": "生产在内网", "updated_at": time.Now()},
		},
	}
	b, _ := json.Marshal(legacy)
	if err := os.MkdirAll(filepath.Join(ws, ".lyon"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".lyon", "memory.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	s := Open(ws)
	if f, ok := s.Get(ScopeProject, "build-command"); !ok || f.Body != "go build ./..." {
		t.Errorf("ascii legacy import: %+v", f)
	}
	if f, ok := s.Get(ScopeProject, "部署环境"); !ok || f.Body != "生产在内网" {
		t.Errorf("cjk legacy import: %+v", f)
	}
	if _, err := os.Stat(filepath.Join(ws, ".lyon", "memory.json")); !os.IsNotExist(err) {
		t.Error("legacy file should be renamed after import")
	}
	if _, err := os.Stat(filepath.Join(ws, ".lyon", "memory.json.imported")); err != nil {
		t.Error("parked legacy file missing")
	}
}

// TestHotTextBudget: over-budget stores keep whole facts only — the cut
// never slices a fact in half.
func TestHotTextBudget(t *testing.T) {
	s := freshStore(t)
	for i := 0; i < 20; i++ {
		must(t, func() (Fact, error) {
			return s.Save(Fact{Name: string(rune('a' + i)), Scope: ScopeProject, Hot: true,
				Body: strings.Repeat("内容 ", 40)})
		})
	}
	hot := s.HotText(512)
	if hot == "" {
		t.Fatal("no hot text under budget")
	}
	full := s.HotText(1 << 20)
	if len(hot) >= len(full) {
		t.Errorf("budget had no effect: %d vs full %d", len(hot), len(full))
	}
	if n := strings.Count(hot, "## (project)"); n == 0 || n >= 20 {
		t.Errorf("unexpected fact count %d in budgeted hot text", n)
	}
	// Whole facts only: every rendered fact ends with a complete body line.
	if !strings.HasSuffix(strings.TrimSpace(hot), "内容 内容 内容") {
		t.Errorf("hot text cut mid-fact:\n%s", hot)
	}
}

// TestSyncPushPullMerge: an httptest server implements the protocol; a
// second store merges pulled facts, and newer-remote / older-remote rules
// both hold.
func TestSyncPushPullMerge(t *testing.T) {
	var received []Fact
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/memory/push":
			var body struct {
				Facts []Fact `json:"facts"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			received = body.Facts
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]int{"accepted": len(body.Facts)})
		case "/memory/pull":
			mu.Lock()
			remote := []Fact{
				{Name: "remote-new", Scope: ScopeTeam, Body: "from teammate",
					Updated: time.Now().Add(time.Hour)},
				{Name: "remote-old", Scope: ScopeTeam, Body: "stale copy",
					Updated: time.Now().Add(-time.Hour)},
				{Name: "evil-project", Scope: ScopeProject, Body: "must be rejected",
					Updated: time.Now()},
			}
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"facts": remote})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("LYON_SYNC_URL", srv.URL)

	s := freshStore(t)
	t.Setenv("LYON_SYNC_URL", srv.URL)
	must(t, func() (Fact, error) {
		return s.Save(Fact{Name: "local-old", Scope: ScopeTeam, Body: "mine",
			Updated: time.Now().Add(-2 * time.Hour)})
	})
	// Remote copy of local-old with same name but older timestamp must not
	// clobber; remote-new lands.
	must(t, func() (Fact, error) {
		return s.Save(Fact{Name: "remote-old", Scope: ScopeTeam, Body: "stale local copy",
			Updated: time.Now()})
	})

	pushed, pulled, err := s.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if pushed == 0 || pulled != 1 {
		t.Errorf("sync counts: pushed=%d pulled=%d", pushed, pulled)
	}
	mu.Lock()
	if len(received) < 2 {
		t.Errorf("push payload: %d facts", len(received))
	}
	mu.Unlock()
	if f, _ := s.Get(ScopeTeam, "remote-new"); !strings.Contains(f.Body, "teammate") {
		t.Errorf("remote-new not merged: %+v", f)
	}
	if f, _ := s.Get(ScopeTeam, "remote-old"); f.Body != "stale local copy" {
		t.Errorf("older remote clobbered newer local: %q", f.Body)
	}
	if _, ok := s.Get(ScopeProject, "evil-project"); ok {
		t.Error("project-scope fact accepted from sync")
	}
}

// TestSyncDisabled: no URL → clean error, no network.
func TestSyncDisabled(t *testing.T) {
	s := freshStore(t)
	if _, _, err := s.Sync(context.Background()); err != ErrSyncDisabled {
		t.Errorf("want ErrSyncDisabled, got %v", err)
	}
}
