package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func repoMapEngine(t *testing.T, ws string) *Engine {
	t.Helper()
	t.Setenv("LYON_HOME", t.TempDir()) // keep hooks off the real home
	cfg := DefaultConfig()
	cfg.Workspace = ws
	cfg.WebToolMode = "off"
	eng := NewEngine(cfg, nil)
	t.Cleanup(eng.Close)
	return eng
}

// TestRepoMapEmptyWorkspace: no files → no note at all — the exact
// system-prompt assertions elsewhere keep byte equality.
func TestRepoMapEmptyWorkspace(t *testing.T) {
	ws := t.TempDir()
	if got := buildRepoMap(ws); got != "" {
		t.Fatalf("empty workspace note = %q, want \"\"", got)
	}
	eng := repoMapEngine(t, ws)
	if got := eng.repoMapSection(ws); got != "" {
		t.Fatalf("section = %q, want \"\"", got)
	}
}

// TestRepoMapListsFilesAndSymbols: paths ride the map, and declarations
// of the known languages are pulled in next to their file.
func TestRepoMapListsFilesAndSymbols(t *testing.T) {
	ws := t.TempDir()
	mustWrite := func(rel, body string) {
		p := filepath.Join(ws, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("a.go", "package a\n\nfunc Alpha() {}\ntype Beta struct{}\n")
	mustWrite("b.py", "def gamma():\n    pass\nclass Delta:\n    pass\n")
	mustWrite("docs/notes.md", "# notes\n")

	got := buildRepoMap(ws)
	for _, want := range []string{"## Repo map", "a.go", "b.py", "docs/notes.md",
		"Alpha", "Beta", "gamma", "Delta"} {
		if !strings.Contains(got, want) {
			t.Fatalf("map missing %q:\n%s", want, got)
		}
	}
}

// TestRepoMapSkipsHeavyDirs: the fallback walk never lists dependency
// forests or VCS internals.
func TestRepoMapSkipsHeavyDirs(t *testing.T) {
	ws := t.TempDir()
	for _, rel := range []string{
		"src/app.ts",
		"node_modules/pkg/index.js",
		".git/config",
		"dist/bundle.js",
	} {
		p := filepath.Join(ws, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := buildRepoMap(ws)
	if !strings.Contains(got, "src/app.ts") {
		t.Fatalf("src/app.ts missing:\n%s", got)
	}
	for _, bad := range []string{"node_modules", ".git/config", "dist/bundle"} {
		if strings.Contains(got, bad) {
			t.Fatalf("%s must not be listed:\n%s", bad, got)
		}
	}
}

// TestRepoMapGitPreferred: inside a repo the map comes from ls-files —
// .gitignore'd files stay out even though they sit in the tree.
func TestRepoMapGitPreferred(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	ws := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", ws}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init")
	if err := os.WriteFile(filepath.Join(ws, ".gitignore"), []byte("secret.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "keep.go"), []byte("package keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "secret.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".gitignore", "keep.go")

	got := buildRepoMap(ws)
	if !strings.Contains(got, "keep.go") {
		t.Fatalf("tracked keep.go missing:\n%s", got)
	}
	if strings.Contains(got, "secret.txt") {
		t.Fatalf("gitignored secret.txt leaked into the map:\n%s", got)
	}
}

// TestRepoMapBudget: a fat workspace truncates inside the rune budget
// and says so instead of silently dropping files.
func TestRepoMapBudget(t *testing.T) {
	ws := t.TempDir()
	for i := range 120 {
		rel := fmt.Sprintf("pkg%02d/file_%03d_%s.go", i%10, i, strings.Repeat("x", 60))
		p := filepath.Join(ws, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("package p\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := buildRepoMap(ws)
	if r := utf8.RuneCountInString(got); r > repoMapBudget+120 {
		t.Fatalf("note %d runes > budget %d (+marker slack):\n%s…", r, repoMapBudget, got[:200])
	}
	if !strings.Contains(got, "more files not shown") {
		t.Fatalf("truncation must be announced:\n%s", got[:200])
	}
}

// TestRepoMapFrozenPerSession: newSession captures the note once; sibling
// sessions inside the TTL share byte-identical text (the cross-session
// prefix stays cacheable); after the TTL the engine rescans and picks up
// new files.
func TestRepoMapFrozenPerSession(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "one.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	eng := repoMapEngine(t, ws)

	s1 := eng.NewSession(nil)
	t.Cleanup(s1.Close)
	if !strings.Contains(s1.repoMapText, "one.go") {
		t.Fatalf("first session note missing one.go:\n%s", s1.repoMapText)
	}

	// Within the TTL the scan is cached: a sibling session gets the
	// same bytes even though the tree changed underneath.
	if err := os.WriteFile(filepath.Join(ws, "two.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s2 := eng.NewSession(nil)
	t.Cleanup(s2.Close)
	if s2.repoMapText != s1.repoMapText {
		t.Fatalf("warm cache must be byte-identical:\n--- s1 ---\n%s--- s2 ---\n%s", s1.repoMapText, s2.repoMapText)
	}

	// Past the TTL the engine rescans — and the frozen s1 copy does not
	// move (per-session stability is about the session, not the engine).
	eng.repoMapMu.Lock()
	eng.repoMapAt = time.Now().Add(-repoMapTTL - time.Second)
	eng.repoMapMu.Unlock()
	s3 := eng.NewSession(nil)
	t.Cleanup(s3.Close)
	if !strings.Contains(s3.repoMapText, "two.go") {
		t.Fatalf("expired cache must rescan:\n%s", s3.repoMapText)
	}
	if !strings.Contains(s1.repoMapText, "one.go") || strings.Contains(s1.repoMapText, "two.go") {
		t.Fatalf("frozen session copy must not move:\n%s", s1.repoMapText)
	}
}

// TestRepoMapInSystemPrompt: the note rides the system block, and two
// builds of the same session's params are byte-identical (cache prefix).
func TestRepoMapInSystemPrompt(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "app.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	eng := repoMapEngine(t, ws)
	sess := eng.NewSession(nil)
	t.Cleanup(sess.Close)

	p1 := sess.baseParams(nil).System[0].Text
	p2 := sess.baseParams(nil).System[0].Text
	if p1 != p2 {
		t.Fatal("system prompt must be byte-stable across builds")
	}
	if !strings.Contains(p1, "## Repo map") || !strings.Contains(p1, "app.go") {
		t.Fatalf("system prompt missing repo map:\n%s", p1)
	}
}
