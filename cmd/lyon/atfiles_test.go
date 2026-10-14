package main

import (
	"os"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestAtToken: only the trailing bare @token opens the file popup —
// quoted paths and plain words stay manual input.
func TestAtToken(t *testing.T) {
	cases := []struct {
		in    string
		tok   string
		start int
		ok    bool
	}{
		{"@", "", 0, true},
		{"look at @doc", "doc", 8, true},
		{"@pkg/rea", "pkg/rea", 0, true},
		{"hello", "", 0, false},
		{"", "", 0, false},
		{`@"my shot.png"`, "", 0, false}, // quoted: manual
		{"@a b", "", 0, false},           // space mid-token: not bare
		{"see @x, then", "", 0, false},   // comma breaks bareness naturally? token is "@x," — allowed, filtered by listing
	}
	for _, c := range cases {
		tok, start, ok := atToken(c.in)
		if ok != c.ok || (ok && (tok != c.tok || start != c.start)) {
			t.Fatalf("atToken(%q) = (%q,%d,%v), want (%q,%d,%v)",
				c.in, tok, start, ok, c.tok, c.start, c.ok)
		}
	}
}

// TestAtEntriesListing: dirs first, dotfiles hidden until asked, ".."
// only inside a subdirectory, and "../" never escapes the workspace.
func TestAtEntriesListing(t *testing.T) {
	ws := t.TempDir()
	mustMkdir := func(rel string) {
		if err := os.MkdirAll(filepath.Join(ws, rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite := func(rel, body string) {
		if err := os.WriteFile(filepath.Join(ws, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustMkdir("pkg/sub")
	mustMkdir("docs")
	mustWrite("main.go", "package main")
	mustWrite("README.md", "# lyon")
	mustWrite(".secret", "hidden")
	mustWrite("docs/guide.md", "hi")

	cmds := func(items []sugItem) []string {
		out := make([]string, len(items))
		for i, it := range items {
			out[i] = it.cmd
		}
		return out
	}

	got := cmds(atEntries(ws, ""))
	if len(got) != 4 || got[0] != "@docs/" || got[1] != "@pkg/" ||
		got[2] != "@README.md" || got[3] != "@main.go" {
		t.Fatalf("root listing = %v, want dirs (docs,pkg) then files", got)
	}

	if got := cmds(atEntries(ws, ".s")); len(got) != 1 || got[0] != "@.secret" {
		t.Fatalf("dot-prefix listing = %v, want just @.secret", got)
	}
	if got := cmds(atEntries(ws, "zz")); len(got) != 0 {
		t.Fatalf("no-match listing = %v, want empty", got)
	}

	// Inside pkg/: ".." climbs out, subdirectory listed.
	got = cmds(atEntries(ws, "pkg/"))
	if len(got) != 2 || got[0] != "@" || got[1] != "@pkg/sub/" {
		t.Fatalf("pkg/ listing = %v, want [\"@\" .., @pkg/sub/]", got)
	}

	// One level deeper: ".." lands back at pkg/.
	got = cmds(atEntries(ws, "pkg/sub/"))
	if len(got) != 1 || got[0] != "@pkg/" {
		t.Fatalf("pkg/sub/ listing = %v, want climb to @pkg/", got)
	}

	// Escapes list nothing.
	if got := atEntries(ws, "../"); len(got) != 0 {
		t.Fatalf("../ escape listed %v", cmds(got))
	}
	if got := atEntries(ws, "pkg/../../"); len(got) != 0 {
		t.Fatalf("nested escape listed %v", cmds(got))
	}
}

// TestAtPopupInsertFlow: enter with the popup open inserts instead of
// sending — a directory keeps browsing, a file lands with a trailing
// space, and the half-typed line never reaches the session.
func TestAtPopupInsertFlow(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "docs", "guide.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := newTestModel(t)
	m.workspace = ws
	m.ti.SetValue("look at @")
	m.updateSuggestions()
	if len(m.sugs) == 0 || m.sugs[0].cmd != "@docs/" {
		t.Fatalf("popup after @ = %+v, want @docs/ first", m.sugs)
	}

	// Enter picks the highlighted directory: insert and keep browsing.
	if cmd := m.submit(); cmd != nil {
		t.Fatal("enter on a dir pick must not produce a submit command")
	}
	if got := m.ti.Value(); got != "look at @docs/" {
		t.Fatalf("input after dir pick = %q", got)
	}
	// docs/ is now open: ".." climbs, guide.md lists.
	if len(m.sugs) != 2 || m.sugs[0].cmd != "@" || m.sugs[1].cmd != "@docs/guide.md" {
		t.Fatalf("docs/ listing = %+v", m.sugs)
	}

	// Arrow keys move the highlight: down → the file.
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if m.sugIdx != 1 {
		t.Fatalf("sugIdx after down = %d, want 1", m.sugIdx)
	}
	// Enter on the file: path + trailing space, popup gone, no submit.
	m.submit()
	if got := m.ti.Value(); got != "look at @docs/guide.md " {
		t.Fatalf("input after file pick = %q", got)
	}
	m.updateSuggestions()
	if len(m.sugs) != 0 {
		t.Fatalf("finished token still pops: %+v", m.sugs)
	}
	if len(m.hist) != 0 {
		t.Fatalf("picking must not send: hist = %v", m.hist)
	}

	// A line with no popup match submits normally (manual @path).
	m.ti.SetValue("describe @zzz-not-there.txt")
	m.updateSuggestions()
	if len(m.sugs) != 0 {
		t.Fatalf("no-match popup = %+v, want none", m.sugs)
	}
	if cmd := m.submit(); cmd == nil && m.status != "" {
		// submit proceeds via sendPrompt; the session errors (no gateway)
		// but the line is what we assert — it reached the history as typed.
	}
	if got := m.hist[len(m.hist)-1]; got != "describe @zzz-not-there.txt" {
		t.Fatalf("no-match line not sent as typed: %q", got)
	}
}

// TestAtPopupVsSlashLand: an @token mid-sentence still pops, while the
// slash popup keeps its own land — the two never collide.
func TestAtPopupVsSlashLand(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newTestModel(t)
	m.workspace = ws

	m.ti.SetValue("/mar")
	m.updateSuggestions()
	if len(m.sugs) != 1 || m.sugs[0].cmd != "/market" {
		t.Fatalf("slash land broken: %+v", m.sugs)
	}

	m.ti.SetValue("compare @a.txt with the report")
	m.updateSuggestions()
	if len(m.sugs) != 0 {
		t.Fatalf("mid-sentence @ (not trailing) must not pop: %+v", m.sugs)
	}
}
