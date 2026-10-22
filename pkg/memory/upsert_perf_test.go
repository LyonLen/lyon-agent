package memory

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// tmpWS returns an isolated workspace with its own LYON_HOME so the
// process-wide store cache keyed by workspace cannot bleed between tests.
// Both roots use the retrying cleanup (scratchTempDir): Save writes facts
// right up to the end, and Windows can still hold them at teardown.
func tmpWS(t *testing.T) string {
	t.Helper()
	home := scratchTempDir(t)
	t.Setenv("LYON_HOME", home)
	ws := scratchTempDir(t)
	t.Cleanup(func() {
		storesMu.Lock()
		delete(loaders, mustAbs(ws))
		if s, ok := stores[mustAbs(ws)]; ok {
			delete(stores, mustAbs(s.workspace))
		}
		storesMu.Unlock()
	})
	return ws
}

func mustAbs(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return a
}

// TestRescanKeepsLocalHitsAfterDelete pins the dirty-counter + rescan
// interaction: Search bumps hits in memory only, Delete must not let the
// next rescan resurrect a stale on-disk view of the counters.
func TestRescanKeepsLocalHitsAfterDelete(t *testing.T) {
	ws := tmpWS(t)
	s := Open(ws)

	if _, err := s.Save(Fact{Scope: ScopeProject, Name: "alpha", Body: "alpha body"}); err != nil {
		t.Fatalf("save alpha: %v", err)
	}
	if _, err := s.Save(Fact{Scope: ScopeProject, Name: "beta", Body: "beta body"}); err != nil {
		t.Fatalf("save beta: %v", err)
	}

	// Search bumps hits without rewriting files -> counters become dirty.
	for i := 0; i < 3; i++ {
		s.Search("alpha", 5)
	}
	s.mu.Lock()
	dirtyBefore := len(s.dirty)
	alphaHits := s.facts[ScopeProject]["alpha"].Hits
	s.mu.Unlock()
	if dirtyBefore == 0 {
		t.Fatalf("expected dirty counters after Search, got none")
	}
	if alphaHits < 3 {
		t.Fatalf("expected alpha hits >= 3 in memory, got %d", alphaHits)
	}

	// Delete beta: its file goes away.
	if err := s.Delete(ScopeProject, "beta"); err != nil {
		t.Fatalf("delete beta: %v", err)
	}

	// Force a rescan as a later query would.
	s.mu.Lock()
	s.rescanLocked()
	_, alphaOK := s.facts[ScopeProject]["alpha"]
	_, betaOK := s.facts[ScopeProject]["beta"]
	hitsAfter := 0
	if alphaOK {
		hitsAfter = s.facts[ScopeProject]["alpha"].Hits
	}
	s.mu.Unlock()

	if !alphaOK {
		t.Fatalf("alpha lost across rescan")
	}
	if betaOK {
		t.Fatalf("beta resurrected by rescan after Delete")
	}
	if hitsAfter < 3 {
		t.Fatalf("local hits lost across rescan: got %d want >= 3", hitsAfter)
	}
	if _, err := os.Stat(filepath.Join(ws, ".lyon", "memory", "beta.md")); err == nil {
		t.Fatalf("beta.md still on disk after Delete")
	}
}

// TestDeleteThenSaveSameName checks the index slot accounting: after a
// delete-rebuild, saving the same name again must not duplicate postings.
func TestDeleteThenSaveSameName(t *testing.T) {
	ws := tmpWS(t)
	s := Open(ws)

	if _, err := s.Save(Fact{Scope: ScopeProject, Name: "gamma", Body: "first body"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := s.Save(Fact{Scope: ScopeProject, Name: "gamma", Body: "second body"}); err != nil {
		t.Fatalf("save update: %v", err)
	}
	if err := s.Delete(ScopeProject, "gamma"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Save(Fact{Scope: ScopeProject, Name: "gamma", Body: "third body"}); err != nil {
		t.Fatalf("save after delete: %v", err)
	}

	hits := s.Search("third", 5)
	if len(hits) != 1 {
		t.Fatalf("want exactly 1 hit for gamma, got %d", len(hits))
	}
	if hits[0].Fact.Body != "third body" {
		t.Fatalf("stale body: %q", hits[0].Fact.Body)
	}
}

// TestUpsertMatchesFullBuild is the correctness contract for #7: the
// incremental upsert must leave the index observably identical to a
// wholesale rebuild over the same fact set.
func TestUpsertMatchesFullBuild(t *testing.T) {
	facts := []*Fact{
		{Scope: ScopeProject, Name: "alpha", Body: "the quick brown fox"},
		{Scope: ScopePersonal, Name: "beta", Body: "quick jumps over lazy dog"},
		{Scope: ScopeProject, Name: "gamma", Body: "brown foxes are quick", Tags: []string{"quick"}},
		{Scope: ScopeTeam, Name: "delta", Body: "unrelated text entirely", Description: "brown and quick"},
	}

	// Incremental path: build empty, then upsert one by one.
	var inc index
	for _, f := range facts {
		inc.upsert(f)
	}

	// Wholesale path.
	var full index
	full.build(facts)

	if len(inc.docs) != len(full.docs) {
		t.Fatalf("doc count: inc=%d full=%d", len(inc.docs), len(full.docs))
	}
	if len(inc.terms) != len(full.terms) {
		t.Fatalf("term count: inc=%d full=%d", len(inc.terms), len(full.terms))
	}
	if inc.avgLen != full.avgLen {
		t.Fatalf("avgLen: inc=%v full=%v", inc.avgLen, full.avgLen)
	}
	for term, want := range full.terms {
		got := inc.terms[term]
		if len(got) != len(want) {
			t.Fatalf("term %q: inc=%d full=%d postings", term, len(got), len(want))
		}
		gotByDoc := map[int]float64{}
		for _, p := range got {
			gotByDoc[p.doc] = p.tf
		}
		for _, p := range want {
			tf, ok := gotByDoc[p.doc]
			if !ok {
				t.Fatalf("term %q missing posting for doc %d", term, p.doc)
			}
			if tf != p.tf {
				t.Fatalf("term %q doc %d: tf inc=%v full=%v", term, p.doc, tf, p.tf)
			}
		}
	}

	// Updating an existing fact must converge to the same index as a
	// rebuild over the updated set.
	updated := &Fact{Scope: ScopeProject, Name: "alpha", Body: "completely different words here"}
	inc.upsert(updated)
	facts[0] = updated
	var full2 index
	full2.build(facts)
	if len(inc.terms) != len(full2.terms) {
		t.Fatalf("after update term count: inc=%d full=%d", len(inc.terms), len(full2.terms))
	}
	if inc.avgLen != full2.avgLen {
		t.Fatalf("after update avgLen: inc=%v full=%v", inc.avgLen, full2.avgLen)
	}
	for term, want := range full2.terms {
		if len(inc.terms[term]) != len(want) {
			t.Fatalf("after update term %q: inc=%d full=%d", term, len(inc.terms[term]), len(want))
		}
	}
	if inc.docByKey[factKey(updated)] != 0 {
		t.Fatalf("alpha should keep slot 0, got %d", inc.docByKey[factKey(updated)])
	}
}

// TestUpsertDropsStalePostings pins the exact failure incremental BM25
// indexes hit: a term that disappears from a fact must vanish from the
// index, not linger and keep scoring the doc.
func TestUpsertDropsStalePostings(t *testing.T) {
	var ix index
	ix.build([]*Fact{{Scope: ScopeProject, Name: "one", Body: "zebra uniqueanimal"}})

	if got := ix.search("uniqueanimal", 10); len(got) == 0 {
		t.Fatalf("precondition: uniqueanimal should hit")
	}

	ix.upsert(&Fact{Scope: ScopeProject, Name: "one", Body: "totally new wording"})

	if _, ok := ix.terms["uniqueanimal"]; ok {
		t.Fatalf("stale term uniqueanimal survived the update")
	}
	if got := ix.search("uniqueanimal", 10); len(got) != 0 {
		t.Fatalf("stale term still scores: %d hits", len(got))
	}
	if got := ix.search("totally", 10); len(got) == 0 {
		t.Fatalf("new term totally should hit")
	}
}

// TestOpenConcurrentNoDoubleLoad: two goroutines racing on the first
// Open of the same workspace must produce the same store, with disk I/O
// held outside storesMu.
func TestOpenConcurrentNoDoubleLoad(t *testing.T) {
	ws := tmpWS(t)
	s0 := Open(ws)
	if _, err := s0.Save(Fact{Scope: ScopeProject, Name: "seed", Body: "seed body"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Drop the cached store so the next Opens take the loading path.
	storesMu.Lock()
	delete(stores, mustAbs(ws))
	storesMu.Unlock()

	const n = 8
	out := make([]*Store, n)
	done := make(chan int, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			out[i] = Open(ws)
			done <- i
		}(i)
	}
	for i := 0; i < n; i++ {
		<-done
	}
	for i := 1; i < n; i++ {
		if out[i] != out[0] {
			t.Fatalf("goroutine %d got a different store instance", i)
		}
	}
	if hits := out[0].Search("seed", 5); len(hits) != 1 {
		t.Fatalf("loaded store lost the seed fact: %d hits", len(hits))
	}
}

// TestSaveIsCheaperThanFullRebuild is the #7 performance gate: Save must
// not be O(all facts). It compares one Save against a full rebuild over
// the same corpus and requires the incremental path to win outright.
func TestSaveIsCheaperThanFullRebuild(t *testing.T) {
	ws := tmpWS(t)
	s := Open(ws)

	const n = 800
	facts := make([]Fact, 0, n)
	for i := 0; i < n; i++ {
		facts = append(facts, Fact{
			Scope:       ScopeProject,
			Name:        "fact" + string(rune('a'+i%26)) + itoa(i),
			Description: "a description with several searchable words in it",
			Body:        "body text number " + itoa(i) + " carrying more tokens to index here",
		})
	}
	// Seed the store so Save hits the update (upsert-replace) path.
	if _, err := s.Save(facts[0]); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s.mu.Lock()
	for _, f := range facts[1:] {
		cp := f
		s.facts[f.Scope][lower(cp.Name)] = &cp
	}
	all := make([]*Fact, 0, len(facts))
	for _, m := range s.facts {
		for _, fp := range m {
			all = append(all, fp)
		}
	}
	s.ix.build(all)
	s.mu.Unlock()

	// Time a full rebuild.
	start := time.Now()
	s.mu.Lock()
	s.ix.build(all)
	s.mu.Unlock()
	fullCost := time.Since(start)

	// Time one incremental Save (update of an existing fact).
	one := facts[n/2]
	one.Body = "rewritten body with fresh wording " + itoa(n)
	start = time.Now()
	if _, err := s.Save(one); err != nil {
		t.Fatalf("save: %v", err)
	}
	upsertCost := time.Since(start)

	// Both sides are wall-clock on a shared machine: a single round can be
	// eaten by a scheduler slice or an antivirus scan of the fresh file, and
	// Save also pays a real disk write that the rebuild does not. Take the
	// best of a few rounds each (the same rule bench/load_test.go uses) —
	// the ratio is what this gate is about, not one unlucky round.
	for i := 1; i < 3; i++ {
		start = time.Now()
		s.mu.Lock()
		s.ix.build(all)
		s.mu.Unlock()
		if d := time.Since(start); d < fullCost {
			fullCost = d
		}
		start = time.Now()
		if _, err := s.Save(one); err != nil {
			t.Fatalf("save: %v", err)
		}
		if d := time.Since(start); d < upsertCost {
			upsertCost = d
		}
	}

	t.Logf("full rebuild=%v single Save=%v over %d facts", fullCost, upsertCost, n)
	if upsertCost >= fullCost {
		t.Fatalf("incremental Save (%v) is not cheaper than full rebuild (%v)", upsertCost, fullCost)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

func lower(s string) string {
	out := []rune(s)
	for i, r := range out {
		if r >= 'A' && r <= 'Z' {
			out[i] = r + 32
		}
	}
	return string(out)
}
