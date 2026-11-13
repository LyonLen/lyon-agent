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
	// Cold-only since 2026-11-11: a hot post-mortem about one incident
	// hijacked unrelated sessions (budget chat → FQC skill).
	if f1.Scope != ScopeProject || f1.Hot {
		t.Fatalf("fact must be project-scope and cold: %+v", f1)
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

// TestSaveDiagnosisColdOnly: post-mortems never ride the hot tier —
// MemoryFacts must not inject them, only read_memory/search finds them.
func TestSaveDiagnosisColdOnly(t *testing.T) {
	st := freshStore(t)
	for i := 0; i < 4; i++ {
		if _, saved, err := st.SaveDiagnosis(
			[]string{fmt.Sprintf("lesson number %d", i)},
			fmt.Sprintf("patch number %d", i),
		); err != nil || !saved {
			t.Fatalf("save %d: saved=%v err=%v", i, saved, err)
		}
	}
	if got := len(st.List(ScopeProject)); got != 4 {
		t.Fatalf("all lessons must stay on file: %d facts", got)
	}
	for _, f := range st.List(ScopeProject) {
		if f.Hot {
			t.Fatalf("diagnosis %q rode the hot tier: %+v", f.Name, f)
		}
	}
	if hot := st.HotText(0); strings.Contains(hot, DiagCategory) {
		t.Fatalf("hot injection leaked a post-mortem:\n%s", hot)
	}
}

// TestOpenDemotesLegacyHotDiagnosis: facts saved hot by the pre-2026-11-11
// policy are demoted when the store loads, so old incident-bound lessons
// stop steering fresh sessions without a manual /memory pass.
func TestOpenDemotesLegacyHotDiagnosis(t *testing.T) {
	st := freshStore(t)
	// Simulate a legacy save: hot flag written straight to disk.
	if _, err := st.Save(Fact{
		Name:   "failed-conditions-legacyhot",
		Scope:  ScopeProject,
		Tags:   []string{DiagCategory},
		Hot:    true,
		Body:   "old lesson that used to inject",
	}); err != nil {
		t.Fatalf("seed legacy hot fact: %v", err)
	}
	if f, ok := st.Get(ScopeProject, "failed-conditions-legacyhot"); !ok || !f.Hot {
		t.Fatalf("seed must start hot: ok=%v fact=%+v", ok, f)
	}
	// Non-diag hot facts are untouched — standing preferences stay hot.
	if _, err := st.Save(Fact{
		Name:  "standing-preference",
		Scope: ScopeProject,
		Hot:   true,
		Body:  "always ask before destructive ops",
	}); err != nil {
		t.Fatalf("seed hot preference: %v", err)
	}

	// Drop the cached store and Open the same workspace again — the
	// load-time sweep in Open is what the test pins.
	ws := st.workspace
	storesMu.Lock()
	delete(stores, ws)
	storesMu.Unlock()
	st2 := Open(ws)

	if f, ok := st2.Get(ScopeProject, "failed-conditions-legacyhot"); !ok || f.Hot {
		t.Fatalf("legacy hot diagnosis must be demoted: ok=%v fact=%+v", ok, f)
	}
	if f, ok := st2.Get(ScopeProject, "standing-preference"); !ok || !f.Hot {
		t.Fatalf("non-diag hot fact must survive the sweep: ok=%v fact=%+v", ok, f)
	}
}

// TestSaveDiagnosisRoundTrip: what lands on disk reloads as the same
// lesson — a fresh Store over the same dirs reads the fact back cold
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
		if f.Hot || !strings.Contains(f.Body, patch) {
			t.Fatalf("roundtrip lost the lesson (or re-hot it): %+v", f)
		}
		return
	}
	t.Fatal("diagnosis fact missing after reload")
}
