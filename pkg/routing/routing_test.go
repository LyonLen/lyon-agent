package routing

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeCorrupt(dir string) error {
	return os.WriteFile(filepath.Join(dir, "routing.json"), []byte("{broken"), 0o644)
}

// settle cleans the scratch dir before the testing framework's TempDir
// cleanup gets to it. Windows file scanners (Defender, indexer) can hold
// a just-written file just long enough that an immediate RemoveAll hits a
// delete-pending entry — "directory not empty" — so retry briefly and log
// whatever actually remains if the window never opens.
func settle(t *testing.T, dir string) {
	t.Helper()
	t.Cleanup(func() {
		var err error
		for i := 0; i < 40; i++ {
			if err = os.RemoveAll(dir); err == nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		entries, _ := os.ReadDir(dir)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Logf("settle cleanup failed after retries: %v (left: %v)", err, names)
	})
}

func TestClassifyBilingualAndSpecificity(t *testing.T) {
	cases := []struct {
		in   string
		want Class
	}{
		{"fix the crash when saving sessions", ClassFix},
		{"这个函数报错了，帮我排查一下", ClassFix},
		{"review my pull request please", ClassReview},
		{"帮我评审这段代码", ClassReview},
		{"design the storage schema for events", ClassDesign},
		{"设计一个三层记忆方案", ClassDesign},
		{"research the trade-offs between sqlite and postgres", ClassResearch},
		{"implement the parser for CSV files", ClassCoding},
		{"实现一个新的子代理功能", ClassCoding},
		{"write a changelog entry for v2", ClassWriting},
		{"把这段话翻译成英文", ClassWriting},
		{"parse this json and print stats", ClassData},
		{"hello there", ClassGeneral},
		{"", ClassGeneral},
	}
	for _, c := range cases {
		if got := Classify(c.in); got != c.want {
			t.Errorf("Classify(%q) = %s, want %s", c.in, got, c.want)
		}
	}
	// Counting over first-match: a fix task that mentions testing once
	// stays fix (fix keywords outweigh one coding hit).
	if got := Classify("fix the failing test in rollback"); got != ClassFix {
		t.Fatalf("fix+test = %s, want fix", got)
	}
}

func TestOpennessGrading(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"design a plugin architecture — what are the trade-offs, should we migrate?", 5},
		{"设计一套缓存方案，权衡一下利弊", 5},
		{"rename this file to config.json", 1}, // one closed signal pulls one step down
		{"把目录格式化一下", 1},
		{"run the unit tests", 0},           // two closed signals → clearly mechanical
		{"hello", 2},                        // no signal → slightly open
		{"fix the crash and add a test", 2}, // mixed / weak signals → neutral
	}
	for _, c := range cases {
		if got := Openness(c.in); got != c.want {
			t.Errorf("Openness(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestTableRecordLookupAndConfidenceRamp(t *testing.T) {
	dir := t.TempDir()
	settle(t, dir)
	t.Setenv("LYON_HOME", dir)
	tbl := Open()
	if _, ok := tbl.Lookup("coding"); ok {
		t.Fatal("empty table must miss")
	}
	e := tbl.Record("coding", "model-a", "model-a", 0.9)
	if e.Samples != 1 || e.MeanScore != 0.9 {
		t.Fatalf("first record = %+v", e)
	}
	first := e.Confidence
	// One sample must stay unsure — mean alone is not confidence.
	if first >= 0.9 {
		t.Fatalf("confidence after 1 sample = %v, want mean×ramp < mean", first)
	}
	for i := 0; i < 19; i++ {
		e = tbl.Record("coding", "model-a", "model-a", 0.9)
	}
	if e.Samples != 20 || e.MeanScore != 0.9 {
		t.Fatalf("after 20 = %+v", e)
	}
	if e.Confidence <= first || e.Confidence < 0.7 {
		t.Fatalf("confidence must ramp with samples: %v → %v", first, e.Confidence)
	}
	got, ok := tbl.Lookup("coding")
	if !ok || got.Agent != "model-a" {
		t.Fatalf("lookup = %+v ok=%v", got, ok)
	}
}

func TestTablePersistsAndSnapshotsSorted(t *testing.T) {
	dir := t.TempDir()
	settle(t, dir)
	t.Setenv("LYON_HOME", dir)
	tbl := Open()
	tbl.Record("fix", "m", "m", 1)
	tbl.Record("coding", "n", "n", 0.5)
	tbl.Record("design", "o", "o", 0.75)

	tbl2 := Open()
	if len(tbl2.All()) != 3 {
		t.Fatalf("reloaded entries = %d", len(tbl2.All()))
	}
	all := tbl2.All()
	for i := 1; i < len(all); i++ {
		if all[i-1].Class > all[i].Class {
			t.Fatalf("All() not sorted: %+v", all)
		}
	}
	// Corrupt state → empty table, never fatal.
	if err := writeCorrupt(dir); err != nil {
		t.Fatal(err)
	}
	if len(Open().All()) != 0 {
		t.Fatal("corrupt file must yield an empty table")
	}
}
