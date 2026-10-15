package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// scratchTempDir is t.TempDir plus a retrying cleanup — the same
// hardening pkg/eval's scratchHome carries: Windows file scanners hold
// just-written files or drop marker dirs into %TEMP% trees mid-test, so
// the framework's one-shot RemoveAll can fail with "directory not empty".
func scratchTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() {
		var err error
		for i := 0; i < 40; i++ {
			if err = os.RemoveAll(dir); err == nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Logf("settle cleanup failed: %v", err)
	})
	return dir
}

// hitsStore is freshStore's twin that also hands back the workspace, so a
// test can drop the process cache and Open the store again — a fresh Open
// proves what actually reached the files (Reload keeps in-memory counters
// by design and can't show disk state).
func hitsStore(t *testing.T) (*Store, string) {
	t.Helper()
	home := scratchTempDir(t)
	ws := scratchTempDir(t)
	t.Setenv("LYON_HOME", home)
	t.Setenv("LYON_SYNC_URL", "")
	storesMu.Lock()
	stores = map[string]*Store{}
	storesMu.Unlock()
	return Open(ws), ws
}

// reopen discards the process cache so Open re-reads every file from disk.
func reopen(ws string) *Store {
	storesMu.Lock()
	stores = map[string]*Store{}
	storesMu.Unlock()
	return Open(ws)
}

// factBytes reads the fact's file exactly as it sits on disk.
func factBytes(t *testing.T, s *Store, scope, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(s.dirs[scope], name+".md"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestSearchKeepsDiskUntouched: repeated hits (Search and Touch) bump the
// counter in memory only — the fact file's bytes stay identical, no .tmp
// droppings appear, and a later Flush lands the count.
func TestSearchKeepsDiskUntouched(t *testing.T) {
	s, ws := hitsStore(t)
	must(t, func() (Fact, error) {
		return s.Save(Fact{Name: "build-cmd", Scope: ScopeProject,
			Body: "run go build ./... to build the agent"})
	})
	before := factBytes(t, s, ScopeProject, "build-cmd")
	if strings.Contains(string(before), "hits:") {
		t.Fatalf("fresh fact should carry no hits line: %s", before)
	}

	for i := 0; i < 5; i++ {
		if h := s.Search("run go build", 5); len(h) == 0 {
			t.Fatalf("query %d found nothing", i)
		}
	}
	s.Touch(ScopeProject, "build-cmd")

	if f, _ := s.Get(ScopeProject, "build-cmd"); f.Hits != 6 {
		t.Fatalf("in-memory hits: got %d, want 6", f.Hits)
	}
	after := factBytes(t, s, ScopeProject, "build-cmd")
	if string(before) != string(after) {
		t.Errorf("Search/Touch rewrote the fact file:\nbefore: %s\nafter:  %s", before, after)
	}
	entries, err := os.ReadDir(s.dirs[ScopeProject])
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("write droppings after search: %s", e.Name())
		}
	}

	// The exit-flush mechanism lands what the queries accumulated.
	s.Flush()
	if f, _ := s.Get(ScopeProject, "build-cmd"); f.Hits != 6 {
		t.Fatalf("hits lost across Flush: %d", f.Hits)
	}
	if got := string(factBytes(t, s, ScopeProject, "build-cmd")); !strings.Contains(got, "hits: 6") {
		t.Errorf("Flush did not persist hits:\n%s", got)
	}
	if f, ok := reopen(ws).Get(ScopeProject, "build-cmd"); !ok || f.Hits != 6 {
		t.Errorf("reopened hits: got %d ok=%v, want 6", f.Hits, ok)
	}
}

// TestSaveFlushesPendingHits: a Save — of the searched fact itself or of
// any other fact — carries queued hit counters to disk; a fresh Open reads
// them back.
func TestSaveFlushesPendingHits(t *testing.T) {
	s, ws := hitsStore(t)
	must(t, func() (Fact, error) {
		return s.Save(Fact{Name: "alpha-fact", Scope: ScopeProject,
			Body: "alpha records zebra migration paths"})
	})
	must(t, func() (Fact, error) {
		return s.Save(Fact{Name: "beta-fact", Scope: ScopeProject,
			Body: "beta records quokka feeding notes"})
	})

	for i := 0; i < 3; i++ {
		if h := s.Search("zebra migration", 5); len(h) == 0 || h[0].Fact.Name != "alpha-fact" {
			t.Fatalf("query %d: %+v", i, h)
		}
	}
	if got := string(factBytes(t, s, ScopeProject, "alpha-fact")); strings.Contains(got, "hits:") {
		t.Fatalf("Search wrote hits before any save point:\n%s", got)
	}

	// Saving ANOTHER fact is an explicit persist point: alpha's queued
	// counter lands alongside beta's own write.
	must(t, func() (Fact, error) {
		return s.Save(Fact{Name: "beta-fact", Scope: ScopeProject, Body: "beta v2"})
	})
	if got := string(factBytes(t, s, ScopeProject, "alpha-fact")); !strings.Contains(got, "hits: 3") {
		t.Errorf("Save of beta did not flush alpha's hits:\n%s", got)
	}

	// Saving the searched fact itself persists its counter too.
	for i := 0; i < 2; i++ {
		s.Search("zebra migration", 5)
	}
	must(t, func() (Fact, error) {
		return s.Save(Fact{Name: "alpha-fact", Scope: ScopeProject,
			Body: "alpha records zebra migration paths (revised)"})
	})
	if got := string(factBytes(t, s, ScopeProject, "alpha-fact")); !strings.Contains(got, "hits: 5") {
		t.Errorf("self-save did not persist hits:\n%s", got)
	}

	f, ok := reopen(ws).Get(ScopeProject, "alpha-fact")
	if !ok || f.Hits != 5 {
		t.Errorf("reopened alpha hits: got %d ok=%v, want 5", f.Hits, ok)
	}
	if f, _ := reopen(ws).Get(ScopeProject, "beta-fact"); f.Body != "beta v2" {
		t.Errorf("beta body after save: %q", f.Body)
	}
}

// TestToggleHotFlushesPendingHits: ToggleHot writes anyway, so queued
// counters ride along — even when the toggle targets a different fact.
func TestToggleHotFlushesPendingHits(t *testing.T) {
	s, ws := hitsStore(t)
	must(t, func() (Fact, error) {
		return s.Save(Fact{Name: "alpha-fact", Scope: ScopeProject,
			Body: "alpha records zebra migration paths"})
	})
	must(t, func() (Fact, error) {
		return s.Save(Fact{Name: "beta-fact", Scope: ScopeProject, Body: "beta notes"})
	})
	s.Search("zebra migration", 5)
	s.Search("zebra migration", 5)

	if on, err := s.ToggleHot(ScopeProject, "beta-fact"); err != nil || !on {
		t.Fatalf("toggle: %v %v", on, err)
	}
	if got := string(factBytes(t, s, ScopeProject, "alpha-fact")); !strings.Contains(got, "hits: 2") {
		t.Errorf("ToggleHot did not flush pending hits of another fact:\n%s", got)
	}
	f, ok := reopen(ws).Get(ScopeProject, "beta-fact")
	if !ok || !f.Hot {
		t.Errorf("hot flag not persisted: %+v", f)
	}
}

// TestFailedFlushKeepsCounts: a write that fails must not drop the
// in-memory count — the next persist point lands it once I/O recovers.
func TestFailedFlushKeepsCounts(t *testing.T) {
	s, ws := hitsStore(t)
	must(t, func() (Fact, error) {
		return s.Save(Fact{Name: "alpha-fact", Scope: ScopeProject,
			Body: "alpha records zebra migration paths"})
	})
	must(t, func() (Fact, error) {
		return s.Save(Fact{Name: "beta-fact", Scope: ScopeProject, Body: "beta notes"})
	})
	s.Search("zebra migration", 5)
	s.Search("zebra migration", 5)
	before := factBytes(t, s, ScopeProject, "alpha-fact")

	// Occupy alpha's tmp path with a directory: atomicWrite's staging
	// file can't be created, so alpha's queued flush fails for real.
	block := filepath.Join(s.dirs[ScopeProject], "alpha-fact.md.tmp")
	if err := os.Mkdir(block, 0o755); err != nil {
		t.Fatal(err)
	}
	// beta's save still succeeds — its own write is untouched by the
	// failure, and alpha's count must stay queued rather than vanish.
	must(t, func() (Fact, error) {
		return s.Save(Fact{Name: "beta-fact", Scope: ScopeProject, Body: "beta v2"})
	})
	if string(factBytes(t, s, ScopeProject, "alpha-fact")) != string(before) {
		t.Errorf("alpha changed despite the blocked write")
	}
	if f, _ := s.Get(ScopeProject, "alpha-fact"); f.Hits != 2 {
		t.Fatalf("in-memory count lost after failed flush: %d", f.Hits)
	}

	// I/O recovers → the next save point lands the queued count.
	if err := os.Remove(block); err != nil {
		t.Fatal(err)
	}
	must(t, func() (Fact, error) {
		return s.Save(Fact{Name: "alpha-fact", Scope: ScopeProject,
			Body: "alpha records zebra migration paths (edited)"})
	})
	f, ok := reopen(ws).Get(ScopeProject, "alpha-fact")
	if !ok || f.Hits != 2 {
		t.Errorf("reopened hits after recovery: got %d ok=%v, want 2", f.Hits, ok)
	}
}
