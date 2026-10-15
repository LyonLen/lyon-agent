package memory

import (
	"fmt"
	"strings"
	"testing"
)

// TestSaveDiagnosisDedup: the same post-mortem twice leaves exactly one
// fact (the fnv key in the name short-circuits the second write), a
// changed lesson appends a second one in the same category, and an
// empty post-mortem is a silent no-op.
func TestSaveDiagnosisDedup(t *testing.T) {
	st := freshStore(t)
	conds := []string{"fixtures updated before claiming done"}
	patch := "update the fixture first"

	f1, saved, err := st.SaveDiagnosis(conds, patch)
	if err != nil || !saved {
		t.Fatalf("first save: saved=%v err=%v", saved, err)
	}
	if f1.Scope != ScopeProject || !f1.Hot {
		t.Fatalf("fact must be project-scope and hot: %+v", f1)
	}
	if !strings.HasPrefix(f1.Name, DiagCategory+"-") {
		t.Fatalf("name not in category %q: %q", DiagCategory, f1.Name)
	}
	if !hasTag(f1.Tags, DiagCategory) {
		t.Fatalf("tags lack %q: %v", DiagCategory, f1.Tags)
	}
	if !strings.Contains(f1.Body, patch) || !strings.Contains(f1.Body, conds[0]) {
		t.Fatalf("body misses the lesson: %q", f1.Body)
	}

	// Same content again: dedup — no second file, same fact back.
	f2, saved, err := st.SaveDiagnosis(conds, patch)
	if err != nil || saved {
		t.Fatalf("repeat must dedup: saved=%v err=%v", saved, err)
	}
	if f2.Name != f1.Name {
		t.Fatalf("dedup returned %q, want %q", f2.Name, f1.Name)
	}
	if got := st.List(ScopeProject); len(got) != 1 {
		t.Fatalf("same lesson stacked up: %d facts", len(got))
	}

	// Different content: appends a second fact of the same category.
	_, saved, err = st.SaveDiagnosis([]string{"go vet must pass"}, "run go vet before done")
	if err != nil || !saved {
		t.Fatalf("distinct save: saved=%v err=%v", saved, err)
	}
	if got := st.List(ScopeProject); len(got) != 2 {
		t.Fatalf("distinct lesson must append: %d facts", len(got))
	}

	// Nothing extracted: no write, no error.
	if _, saved, err = st.SaveDiagnosis(nil, "  "); err != nil || saved {
		t.Fatalf("empty diagnosis: saved=%v err=%v", saved, err)
	}
	if got := st.List(ScopeProject); len(got) != 2 {
		t.Fatalf("empty diagnosis wrote: %d facts", len(got))
	}
}

// TestSaveDiagnosisHotWindow: the just-saved lesson plus the newest
// diagHotKeep-1 of the rest stay hot (what MemoryFacts injects); older
// lessons drop to the cold layer — still listed, no longer pinned.
func TestSaveDiagnosisHotWindow(t *testing.T) {
	st := freshStore(t)
	var last Fact
	for i := 0; i < diagHotKeep+2; i++ {
		f, saved, err := st.SaveDiagnosis(
			[]string{fmt.Sprintf("lesson number %d", i)},
			fmt.Sprintf("patch number %d", i),
		)
		if err != nil || !saved {
			t.Fatalf("save %d: saved=%v err=%v", i, saved, err)
		}
		last = f
	}
	if got := len(st.List(ScopeProject)); got != diagHotKeep+2 {
		t.Fatalf("all lessons must stay on file: %d facts", got)
	}
	hot := 0
	for _, f := range st.List(ScopeProject) {
		if f.Hot {
			hot++
		}
	}
	if hot != diagHotKeep {
		t.Fatalf("hot lessons = %d, want %d", hot, diagHotKeep)
	}
	if cur, ok := st.Get(ScopeProject, last.Name); !ok || !cur.Hot {
		t.Fatalf("newest lesson must stay hot: ok=%v fact=%+v", ok, cur)
	}
}

// TestSaveDiagnosisRoundTrip: what lands on disk reloads as the same
// lesson — a fresh Store over the same dirs reads the fact back hot
// with the patch intact (the next process's view).
func TestSaveDiagnosisRoundTrip(t *testing.T) {
	st := freshStore(t)
	patch := "build before finishing, always"
	if _, saved, err := st.SaveDiagnosis([]string{"build never ran"}, patch); err != nil || !saved {
		t.Fatalf("save: saved=%v err=%v", saved, err)
	}
	// Reload re-reads from disk — the next process's view of the store.
	st.Reload()
	for _, f := range st.List(ScopeProject) {
		if !strings.HasPrefix(f.Name, DiagCategory+"-") {
			continue
		}
		if !f.Hot || !strings.Contains(f.Body, patch) {
			t.Fatalf("roundtrip lost the lesson: %+v", f)
		}
		return
	}
	t.Fatal("diagnosis fact missing after reload")
}
