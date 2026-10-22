package market

import (
	"strings"
	"testing"
	"time"

	"lyon-agent/pkg/routing"
)

// codingText / researchText are pinned by routing.Classify in the test —
// records and queries always use texts whose class we assert first, so
// the market math is tested without keyword brittleness.
const codingText = "fix the failing unit test in the parser package and commit"

func rec(class string, durMS int64, errStr string) Record {
	return Record{Class: class, Model: "m", Prompt: "task", DurMS: durMS, Err: errStr,
		TS: time.Now()}
}

// TestRecordEstimateRoundTrip: prices come from the class's own recent
// successful runs — failed rows stay on the ledger but out of the math,
// an unpriced class borrows the global pool, and an empty ledger has no
// opinion at all.
func TestRecordEstimateRoundTrip(t *testing.T) {
	scratchHome(t)
	tab := Open()

	// Empty ledger: no history, no number.
	if _, ok := tab.Estimate(codingText); ok {
		t.Fatal("empty ledger must not invent a price")
	}

	class := string(routing.Classify(codingText))
	if class == "" {
		t.Fatal("classify failed for the coding probe text")
	}

	// Three coding runs at 10s, one research at 60s, one failed coding
	// run at 1s that must not drag the price down.
	for i := 0; i < 3; i++ {
		tab.Record(rec(class, 10_000, ""))
	}
	tab.Record(rec("research", 60_000, ""))
	tab.Record(rec(class, 1_000, "interrupted"))

	est, ok := tab.Estimate(codingText)
	if !ok {
		t.Fatal("priced class returned no estimate")
	}
	if est.Class != class || est.DurMS != 10_000 || est.Samples != 3 || est.Fallback {
		t.Fatalf("estimate = %+v, want class=%s 10000ms n=3 primary", est, class)
	}

	// A task in an unpriced class borrows the global pool, flagged.
	est, ok = tab.Estimate("write a poem about the sea")
	if !ok {
		t.Fatal("global fallback must price from any history")
	}
	if !est.Fallback || est.Samples != 4 { // 3 coding ok + 1 research ok
		t.Fatalf("fallback = %+v, want global pool over 4 ok runs", est)
	}
}

// TestEstimateWindowTracksRecentPace: the window is the LAST n successful
// runs, so a pace change shows up without waiting for old rows to age out.
func TestEstimateWindowTracksRecentPace(t *testing.T) {
	scratchHome(t)
	tab := Open()
	class := string(routing.Classify(codingText))

	for i := 0; i < estWindow; i++ {
		tab.Record(rec(class, 100_000, "")) // the slow old days
	}
	tab.Record(rec(class, 2_000, "")) // work got much faster

	est, ok := tab.Estimate(codingText)
	if !ok {
		t.Fatal("no estimate")
	}
	// 9 old + 1 new inside the window of 10.
	if want := int64((9*100_000 + 2_000) / estWindow); est.DurMS != want {
		t.Fatalf("estimate = %dms, want recent-window mean %dms", est.DurMS, want)
	}
}

// TestPersistenceAndClip: the ledger survives a reload, prompts clip to
// their preview length, and the cap trims the oldest rows.
func TestPersistenceAndClip(t *testing.T) {
	scratchHome(t)

	tab := Open()
	tab.Record(Record{Class: "coding", Prompt: strings.Repeat("x", 300), DurMS: 5, TS: time.Now()})

	// Reload: the long prompt clipped to its preview.
	tab = Open()
	if got := tab.Recent(1)[0].Prompt; got != strings.Repeat("x", 120)+"…" {
		t.Fatalf("prompt not clipped to 120+ellipsis: %q", got)
	}

	// Overflow the cap; the oldest (clipped) row trims off the front.
	for i := 0; i < historyCap+5; i++ {
		tab.Record(rec("bulk", 1, ""))
	}
	tab = Open()
	if n := len(tab.Recent(10_000)); n != historyCap {
		t.Fatalf("ledger size = %d, want capped at %d", n, historyCap)
	}
	if r := tab.Recent(1)[0]; r.Class != "bulk" {
		t.Fatalf("newest = %+v, want the last bulk row", r)
	}
}

// TestStatsAggregation: the /market table's per-class rollup counts only
// successful runs, newest class first, averages correct.
func TestStatsAggregation(t *testing.T) {
	scratchHome(t)
	tab := Open()
	tab.Record(rec("coding", 10_000, ""))
	tab.Record(rec("coding", 20_000, ""))
	tab.Record(rec("coding", 99_000, "failed")) // excluded
	tab.Record(rec("research", 60_000, ""))

	stats := tab.Stats()
	if len(stats) != 2 {
		t.Fatalf("stats = %+v, want 2 classes", stats)
	}
	if stats[0].Class != "research" || stats[1].Class != "coding" {
		t.Fatalf("order = %s,%s — newest class first", stats[0].Class, stats[1].Class)
	}
	if c := stats[1]; c.Samples != 2 || c.AvgMS != 15_000 || c.LastMS != 20_000 {
		t.Fatalf("coding stat = %+v, want n=2 avg=15000 last=20000", c)
	}
}
