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

// TestListDraftsIndexesSeats: a draft left by a hard-killed lyond is not
// swept or archived — it is a seat the next process indexes and re-adopts.
// The id comes from the snapshot, so a hand-renamed file still lists under
// the seat it carries; a truncated file is skipped rather than reported as a
// conversation.
func TestListDraftsIndexesSeats(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))
	ws := tempDir(t)

	path := writeDraft(t, ws, "aaaa", Snapshot{Version: 1, ID: "seat-a", Workspace: ws, Title: "crash-lost",
		SavedAt: time.Now(), Messages: storeMsg("DRAFT-RECOVERED")})
	// A file renamed by hand still names the seat it carries.
	writeDraft(t, ws, "renamed", Snapshot{Version: 1, ID: "seat-b", Workspace: ws,
		SavedAt: time.Now(), Messages: storeMsg("DRAFT-RENAMED")})
	// Pre-seat build: no ID, so the filename is the seat.
	writeDraft(t, ws, "oldschool", Snapshot{Version: 1, Workspace: ws,
		SavedAt: time.Now(), Messages: storeMsg("DRAFT-OLD")})

	dir, err := draftsDir(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	refs, err := ListDrafts(ws)
	if err != nil {
		t.Fatalf("ListDrafts: %v", err)
	}
	got := map[string]string{} // id → title
	for _, r := range refs {
		got[r.ID] = r.Snap.Title
	}
	want := map[string]string{"seat-a": "crash-lost", "seat-b": "", "oldschool": ""}
	if len(got) != len(want) {
		t.Fatalf("ListDrafts ids = %v, want %v", got, want)
	}
	for id, title := range want {
		if title == "" {
			if _, ok := got[id]; !ok {
				t.Fatalf("ListDrafts missing %q: %v", id, got)
			}
			continue
		}
		if got[id] != title {
			t.Fatalf("ListDrafts[%q] title = %q, want %q", id, got[id], title)
		}
	}

	// Nothing was archived or deleted: the dump is the record.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("draft file removed by listing: %v", err)
	}
	snaps, err := ListSnapshots(ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 0 {
		t.Fatalf("ListDrafts archived conversations into the snapshot store: %+v", snaps)
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
