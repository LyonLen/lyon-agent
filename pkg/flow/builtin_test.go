package flow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuiltinsLoadAndValidate: the shipped toolbox parses, validates and
// matches the design §3.3 table (four slugs, expected node flows).
func TestBuiltinsLoadAndValidate(t *testing.T) {
	wfs, err := Builtins()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"feature-dev": {"clarify", "design", "implement", "selftest", "review", "deliver"},
		"bugfix":      {"reproduce", "locate", "fix", "regression", "summarize"},
		"code-review": {"inventory", "per-file", "verdict", "autofix"},
		"release":     {"status-check", "version", "changelog", "build-verify", "tag"},
	}
	if len(wfs) != len(want) {
		t.Fatalf("builtins = %d, want %d", len(wfs), len(want))
	}
	for _, wf := range wfs {
		nodes, ok := want[wf.Slug]
		if !ok {
			t.Errorf("unexpected builtin %q", wf.Slug)
			continue
		}
		if len(wf.Trigger) == 0 {
			t.Errorf("%s: no trigger keywords", wf.Slug)
		}
		if len(wf.Nodes) != len(nodes) {
			t.Errorf("%s: nodes = %d, want %d", wf.Slug, len(wf.Nodes), len(nodes))
			continue
		}
		for i, n := range wf.Nodes {
			if n.Name != nodes[i] {
				t.Errorf("%s node[%d] = %q, want %q", wf.Slug, i, n.Name, nodes[i])
			}
			if strings.TrimSpace(n.Prompt) == "" {
				t.Errorf("%s/%s: empty prompt", wf.Slug, n.Name)
			}
		}
		if err := validate(wf); err != nil {
			t.Errorf("%s: validate: %v", wf.Slug, err)
		}
	}
	// Spot-check: the test nodes carry the objective probes.
	bug := find(t, wfs, "bugfix")
	if bug.Nodes[3].Exit.Kind != ExitCommand || bug.Nodes[3].Exit.Command != "go test ./..." {
		t.Fatalf("regression exit = %+v", bug.Nodes[3].Exit)
	}
	if wfs2 := find(t, wfs, "release"); wfs2.Nodes[0].Fallback != FallbackAbort {
		t.Fatalf("release status-check fallback = %q, want abort", wfs2.Nodes[0].Fallback)
	}
}

func find(t *testing.T, wfs []Workflow, slug string) Workflow {
	t.Helper()
	for _, wf := range wfs {
		if wf.Slug == slug {
			return wf
		}
	}
	t.Fatalf("builtin %q missing", slug)
	return Workflow{}
}

// TestUserOverrideAndSave: a user workflow in $LYON_HOME/workflows
// replaces the builtin of the same slug; broken files are skipped, not
// fatal. Checkout copies a builtin out for editing.
func TestUserOverrideAndSave(t *testing.T) {
	home := t.TempDir()
	settle(t, home)
	t.Setenv("LYON_HOME", home)

	// Nothing user-authored yet: pure builtins.
	base, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if find(t, base, "bugfix").Title == "my overridden bugfix" {
		t.Fatal("unexpected override before Save")
	}

	// Checkout then hand-edit the copy.
	path, err := Checkout("bugfix")
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(UserDir(), "bugfix.json") {
		t.Fatalf("checkout path = %s", path)
	}
	override := Workflow{
		Slug: "bugfix", Title: "my overridden bugfix",
		Trigger: []string{"fix"},
		Nodes:   []Node{{Name: "only", Exit: Exit{Kind: ExitAlways}}},
	}
	if err := Save(override); err != nil {
		t.Fatal(err)
	}
	// A broken file next to it must not take Load down.
	if err := os.WriteFile(filepath.Join(UserDir(), "junk.json"), []byte("{nope"), 0o644); err != nil {
		t.Fatal(err)
	}

	after, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := find(t, after, "bugfix"); got.Title != "my overridden bugfix" || len(got.Nodes) != 1 {
		t.Fatalf("override not applied: %+v", got)
	}
	if len(after) != len(base) {
		t.Fatalf("junk file added a workflow: %d → %d", len(base), len(after))
	}

	// Save rejects structurally broken workflows.
	if err := Save(Workflow{Slug: "bad", Nodes: []Node{{Name: "a", Exit: Exit{Kind: "vibes"}}}}); err == nil {
		t.Fatal("Save accepted a broken workflow")
	}
	if _, err := Checkout("does-not-exist"); err == nil {
		t.Fatal("Checkout must miss on unknown slug")
	}
}

// TestLoadDirSkipsForeignFiles: only validated *.json workflows count.
func TestLoadDirSkipsForeignFiles(t *testing.T) {
	dir := t.TempDir()
	settle(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ok.json"),
		[]byte(`{"slug":"ok","title":"t","trigger":["k"],"nodes":[{"name":"a"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := LoadDir(dir)
	if len(got) != 1 || got[0].Slug != "ok" {
		t.Fatalf("LoadDir = %+v", got)
	}
}

// homeDirIsEmptyGuard keeps the compiler honest about the imported time
// in case settle's shape changes — asserting cleanup ran within bounds.
func TestUserDirUnderHome(t *testing.T) {
	home := t.TempDir()
	settle(t, home)
	t.Setenv("LYON_HOME", home)
	if got, want := UserDir(), filepath.Join(home, "workflows"); got != want {
		t.Fatalf("UserDir = %s, want %s", got, want)
	}
	// Default (unset) home resolves under the user profile, never "".
	t.Setenv("LYON_HOME", "")
	ud := UserDir()
	if ud == "" || ud == "workflows" {
		t.Fatalf("UserDir without LYON_HOME = %q", ud)
	}
}
