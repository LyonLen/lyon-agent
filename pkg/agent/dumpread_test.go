package agent

import (
	"os"
	"testing"
	"time"
)

// TestDumpReadRoundTrip drives the read side of the dump store against
// the real writer: two same-day dumps via writeRunDump, then list (order,
// limit, empty day, bad day) and load (field round-trip, run-id
// validation, missing file). All under a scratch LYON_HOME — no real
// ~/.lyon touches.
func TestDumpReadRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("LYON_HOME", home)

	early := time.Date(2026, 10, 16, 9, 0, 0, 0, time.Local)
	late := early.Add(2 * time.Hour)
	write := func(started time.Time, prompt string) *runDumpFile {
		t.Helper()
		doc := &runDumpFile{
			SchemaVersion: dumpSchemaVersion,
			Workspace:     "/ws",
			Model:         "m1",
			Prompt:        prompt,
			FirstPrompt:   prompt,
			StartedAt:     started,
			EndedAt:       started.Add(time.Second),
			DurationMS:    1000,
			Iterations:    1,
			StopReason:    "end_turn",
			Usage:         Usage{Input: 10, Output: 5, CacheRead: 3},
			Events: []dumpEvent{
				{Seq: 1, Event: Event{Type: EventToolUseStart, ToolName: "read_file"}},
				{Seq: 2, Event: Event{Type: EventPromptEnd, Turn: 7, StopReason: "end_turn"}},
			},
			History: "final",
		}
		if _, err := writeRunDump(doc); err != nil {
			t.Fatalf("writeRunDump(%s): %v", prompt, err)
		}
		return doc
	}
	earlyDoc := write(early, "first prompt")
	lateDoc := write(late, "second prompt")

	refs, err := ListRunDumps("2026-10-16", 0)
	if err != nil {
		t.Fatalf("ListRunDumps: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("got %d refs, want 2", len(refs))
	}
	if refs[0].RunID != lateDoc.RunID || refs[1].RunID != earlyDoc.RunID {
		t.Fatalf("order = [%s %s], want newest first [%s %s]",
			refs[0].RunID, refs[1].RunID, lateDoc.RunID, earlyDoc.RunID)
	}
	if refs[0].Prompt != "second prompt" || refs[0].DurationMS != 1000 || refs[0].Failed {
		t.Fatalf("ref[0] = %+v", refs[0])
	}

	one, err := ListRunDumps("2026-10-16", 1)
	if err != nil || len(one) != 1 || one[0].RunID != lateDoc.RunID {
		t.Fatalf("limit 1 = %+v, err %v", one, err)
	}

	if none, err := ListRunDumps("2026-10-15", 0); err != nil || len(none) != 0 {
		t.Fatalf("missing day = %+v, err %v (want empty, nil)", none, err)
	}
	for _, bad := range []string{"../../etc", "2026-13-40", "20261016", "not-a-day"} {
		if _, err := ListRunDumps(bad, 0); err == nil {
			t.Errorf("ListRunDumps(%q) accepted a bad day", bad)
		}
	}

	doc, err := LoadRunDump(lateDoc.RunID)
	if err != nil {
		t.Fatalf("LoadRunDump(%s): %v", lateDoc.RunID, err)
	}
	if doc.Prompt != "second prompt" || doc.DurationMS != 1000 ||
		doc.Usage.Input != 10 || doc.Usage.CacheRead != 3 || doc.History != "final" {
		t.Fatalf("doc = %+v", doc)
	}
	if len(doc.Events) != 2 || doc.Events[0].Seq != 1 ||
		doc.Events[0].ToolName != "read_file" || doc.Events[1].Turn != 7 {
		t.Fatalf("events = %+v", doc.Events)
	}

	for _, bad := range []string{
		"../../../etc/passwd",
		"20261016-120000-ABCDEFGH1234", // uppercase hex
		"20261016-120000-abc",          // short suffix
		"20261301-120000-abcdef012345", // impossible date
		"",
	} {
		if _, err := LoadRunDump(bad); err == nil {
			t.Errorf("LoadRunDump(%q) accepted a bad id", bad)
		}
	}
	// Well-formed but nobody wrote it: must be NotFound-shaped, not a
	// bad-id error — the distinction is what maps to Connect codes.
	if _, err := LoadRunDump("20261016-120000-abcdefabcdef"); !os.IsNotExist(err) {
		t.Errorf("missing dump err = %v, want os.IsNotExist", err)
	}
}
