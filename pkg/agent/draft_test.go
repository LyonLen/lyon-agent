package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeDraft drops snap into the workspace's draft dir under id.
func writeDraft(t *testing.T, ws, id string, snap Snapshot) string {
	t.Helper()
	path, err := DraftPath(ws, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteSnapshotFile(path, snap); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSweepDraftsArchivesAndDedupes: a draft left by a hard-killed lyond
// becomes a normal archive entry; a draft whose content is already stored is
// dropped without a duplicate; corrupt drafts are kept (they may be the only
// copy) and reported.
func TestSweepDraftsArchivesAndDedupes(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))
	ws := tempDir(t)

	writeDraft(t, ws, "aaaa", Snapshot{Version: 1, Workspace: ws, Title: "crash-lost",
		SavedAt: time.Now(), Messages: storeMsg("DRAFT-RECOVERED")})

	n, err := SweepDrafts(ws)
	if err != nil || n != 1 {
		t.Fatalf("SweepDrafts = (%d, %v), want (1, nil)", n, err)
	}
	refs, err := ListSnapshots(ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Title != "crash-lost" {
		t.Fatalf("archive after sweep = %+v", refs)
	}

	// Same conversation again (archive landed, kill before the draft was
	// removed): must not duplicate.
	writeDraft(t, ws, "bbbb", Snapshot{Version: 1, Workspace: ws, Title: "crash-lost",
		SavedAt: time.Now(), Messages: storeMsg("DRAFT-RECOVERED")})
	if n, err := SweepDrafts(ws); err != nil || n != 0 {
		t.Fatalf("dedupe sweep = (%d, %v), want (0, nil)", n, err)
	}
	refs, _ = ListSnapshots(ws)
	if len(refs) != 1 {
		t.Fatalf("duplicate archive entry: %+v", refs)
	}

	// Corrupt draft: kept for the next startup, error surfaced.
	dir, err := draftsDir(ws)
	if err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "cccc.json")
	if err := os.MkdirAll(dir, 0o755); err != nil { // the sweep removes the empty dir
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := SweepDrafts(ws); err == nil {
		t.Fatal("corrupt draft: want an error, got nil")
	}
	if _, err := os.Stat(bad); err != nil {
		t.Fatalf("corrupt draft was deleted: %v", err)
	}
}

// TestDraftTouchCoalesces: Touch returns immediately (it runs on the hub's
// consumer goroutine) and the last state wins; Close waits for the write, so
// the file is complete once Close returns.
func TestDraftTouchCoalesces(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))
	ws := tempDir(t)
	eng := NewEngine(DefaultConfig(), nil)
	sess := eng.NewSession(nil)
	t.Cleanup(sess.Close)
	sess.Restore(Snapshot{Version: 1, Workspace: ws, Title: "touched",
		Messages: storeMsg("TOUCH-LAST")})

	path, err := DraftPath(ws, "touch")
	if err != nil {
		t.Fatal(err)
	}
	d := sess.NewDraft(path)
	for i := 0; i < 20; i++ {
		d.Touch()
	}
	d.Close()
	d.Touch() // after Close: no-op, must not panic or write

	snap, err := LoadSnapshotFrom(path)
	if err != nil || snap == nil {
		t.Fatalf("draft unreadable after Close: %v", err)
	}
	if snap.Title != "touched" || len(snap.Messages) != 1 {
		t.Fatalf("draft = %+v", snap)
	}
}

// TestDraftPathIsPerSession: different live sessions never share a slot,
// and the slot sits below the archive directory (so ListSnapshots' scan
// never sees a live session twice).
func TestDraftPathIsPerSession(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))
	ws := tempDir(t)
	a, err := DraftPath(ws, "aaaa")
	if err != nil {
		t.Fatal(err)
	}
	b, err := DraftPath(ws, "bbbb")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("two sessions share a draft slot: %q", a)
	}
	latest, err := SnapshotPath(ws)
	if err != nil {
		t.Fatal(err)
	}
	if dir := filepath.Dir(a); dir == filepath.Dir(latest) {
		t.Fatalf("drafts land in the archive dir: %q", dir)
	}
}
