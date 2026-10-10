package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"lyon-agent/internal/mockapi"
)

func storeMsg(text string) []anthropic.BetaMessageParam {
	return []anthropic.BetaMessageParam{{
		Role:    anthropic.BetaMessageParamRoleUser,
		Content: []anthropic.BetaContentBlockParamUnion{{OfText: &anthropic.BetaTextBlockParam{Text: text}}},
	}}
}

// TestSnapshotPathHonorsLYON_HOME: $LYON_HOME relocates the whole store —
// the save lands under the override, never in the real ~/.lyon.
func TestSnapshotPathHonorsLYON_HOME(t *testing.T) {
	root := tempDir(t)
	t.Setenv("LYON_HOME", root)
	ws := tempDir(t)
	p, err := SnapshotPath(ws)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p, root) {
		t.Fatalf("path %q not under LYON_HOME %q", p, root)
	}
	if err := SaveSnapshot(Snapshot{Version: 1, Workspace: ws, SavedAt: time.Now(),
		Messages: storeMsg("HOME-OVERRIDE")}); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSnapshot(ws)
	if err != nil || got == nil {
		t.Fatalf("override store unreadable: %v", err)
	}
}

// TestSaveRotatesIntoArchive: every distinct conversation survives a save —
// the previous latest rotates into the archive instead of being overwritten,
// an identical re-save is a no-op (no duplicates), and ListSnapshots returns
// the whole store newest-first so /sessions + /resume N can reach back.
func TestSaveRotatesIntoArchive(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t)) // keep the store out of ~/.lyon
	ws := tempDir(t)
	a := Snapshot{Version: 1, Workspace: ws, SavedAt: time.Date(2026, 10, 10, 14, 0, 0, 0, time.Local),
		Messages: storeMsg("SESSION-ALPHA")}
	b := Snapshot{Version: 1, Workspace: ws, SavedAt: time.Date(2026, 10, 10, 15, 0, 0, 0, time.Local),
		Messages: storeMsg("SESSION-BETA")}

	if err := SaveSnapshot(a); err != nil {
		t.Fatal(err)
	}
	if err := SaveSnapshot(b); err != nil {
		t.Fatal(err)
	}

	latest, err := LoadSnapshot(ws)
	if err != nil {
		t.Fatal(err)
	}
	if latest == nil || !sameHistory(latest.Messages, b.Messages) {
		t.Fatal("latest slot should hold the newest save (B)")
	}

	refs, err := ListSnapshots(ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("store has %d entries, want 2 (B latest + A archived)", len(refs))
	}
	if refs[0].SavedAt.Hour() != 15 || refs[1].SavedAt.Hour() != 14 {
		t.Fatalf("order not newest-first: %v then %v", refs[0].SavedAt, refs[1].SavedAt)
	}

	back, err := LoadSnapshotFrom(refs[1].Path)
	if err != nil || back == nil {
		t.Fatalf("archive entry unreadable: %v", err)
	}
	if !sameHistory(back.Messages, a.Messages) {
		t.Fatal("archived entry lost SESSION-ALPHA")
	}

	// Re-saving an identical conversation (resume + quit) is a no-op:
	// no duplicate, latest slot untouched.
	if err := SaveSnapshot(b); err != nil {
		t.Fatal(err)
	}
	refs, err = ListSnapshots(ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("identical re-save grew the store to %d entries", len(refs))
	}
}

// TestArchiveKeepsLatestSlot: /new's archive path files the conversation
// beside the latest slot — the slot itself never changes until a real save.
func TestArchiveKeepsLatestSlot(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))
	ws := tempDir(t)
	a := Snapshot{Version: 1, Workspace: ws, SavedAt: time.Date(2026, 10, 10, 14, 0, 0, 0, time.Local),
		Messages: storeMsg("SESSION-ALPHA")}
	other := Snapshot{Version: 1, Workspace: ws, SavedAt: time.Date(2026, 10, 10, 16, 0, 0, 0, time.Local),
		Messages: storeMsg("SESSION-GAMMA")}
	if err := SaveSnapshot(a); err != nil {
		t.Fatal(err)
	}
	if err := ArchiveSnapshot(other); err != nil {
		t.Fatal(err)
	}
	latest, err := LoadSnapshot(ws)
	if err != nil || latest == nil {
		t.Fatalf("latest slot gone: %v", err)
	}
	if !sameHistory(latest.Messages, a.Messages) {
		t.Fatal("ArchiveSnapshot must not touch the latest slot")
	}
	refs, err := ListSnapshots(ws)
	if err != nil || len(refs) != 2 {
		t.Fatalf("store: %d entries, err=%v — want 2", len(refs), err)
	}
	if refs[0].SavedAt.Hour() != 16 {
		t.Fatalf("archived 16:00 should sort first, got %v", refs[0].SavedAt)
	}
}

// TestArchiveIfNewDeduplicates: /new archives the live conversation once —
// a second /new without edits must not file the same history again.
func TestArchiveIfNewDeduplicates(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))
	newCompactMock(t, []mockapi.Turn{
		{Text: []string{"ARCHIVE-MARKER: live conversation"}},
		{Text: []string{"after reset"}},
	})
	cfg := DefaultConfig()
	cfg.CompactThreshold = -1
	cfg.Workspace = tempDir(t)
	eng := NewEngine(cfg, nil)
	sess := eng.NewSession(nil)
	defer sess.Close()

	if err := sess.Submit("ARCHIVE-MARKER: live conversation"); err != nil {
		t.Fatal(err)
	}
	if pe, _ := waitPromptEndCollect(t, sess); pe.Err != "" {
		t.Fatalf("prompt failed: %s", pe.Err)
	}

	archived, err := sess.ArchiveIfNew()
	if err != nil || !archived {
		t.Fatalf("first ArchiveIfNew = %v, %v — want true, nil", archived, err)
	}
	// The archive does not fill the latest slot.
	if latest, err := LoadSnapshot(cfg.Workspace); err != nil || latest != nil {
		t.Fatalf("latest slot should stay empty, got %v / %v", latest, err)
	}
	// Same live history again → already stored → no duplicate.
	archived, err = sess.ArchiveIfNew()
	if err != nil || archived {
		t.Fatalf("second ArchiveIfNew = %v, %v — want false, nil", archived, err)
	}
	refs, err := ListSnapshots(cfg.Workspace)
	if err != nil || len(refs) != 1 {
		t.Fatalf("store: %d entries, err=%v — want exactly 1", len(refs), err)
	}
	// The archived entry carries the live history.
	got, err := LoadSnapshotFrom(refs[0].Path)
	if err != nil || got == nil {
		t.Fatalf("archived unreadable: %v", err)
	}
	joined := ""
	for _, m := range got.Messages {
		for _, blk := range m.Content {
			if blk.OfText != nil {
				joined += blk.OfText.Text
			}
		}
	}
	if !strings.Contains(joined, "ARCHIVE-MARKER") {
		t.Fatalf("archive lost the conversation: %.300s", joined)
	}
}
