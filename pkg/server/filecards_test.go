package server

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"lyon-agent/pkg/agent"
)

// newFileCardRepo is newGitFixture's lighter twin: a scratch git repo with
// no Server behind it — file cards only need a workspace and a hub.
func newFileCardRepo(t *testing.T) (string, func(args ...string)) {
	t.Helper()
	repo := t.TempDir()
	t.Cleanup(func() { settleRemoveAll(repo) })
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", args[0], err, out)
		}
	}
	run("init")
	run("config", "user.email", "test@lyon.local")
	run("config", "user.name", "lyon-test")
	return repo, run
}

// waitFileBaseline polls until beginFileCards' async capture has stored a
// snapshot (the capture is a real git status — never assume its latency).
func waitFileBaseline(t *testing.T, h *hub) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.baselineMu.Lock()
		ok := h.baselineOK
		h.baselineMu.Unlock()
		if ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("file-card baseline never captured")
}

// findEvent returns the first stamped event of typ from the replay ring.
func findEvent(h *hub, typ agent.EventType) *agent.Event {
	for _, it := range h.dump() {
		if it.ev.Type == typ {
			ev := it.ev
			return &ev
		}
	}
	return nil
}

func waitPromptEnd(t *testing.T, h *hub) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if findEvent(h, agent.EventPromptEnd) != nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("prompt_end never stamped")
}

func waitFilesChanged(t *testing.T, h *hub) agent.Event {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ev := findEvent(h, evFilesChanged); ev != nil {
			return *ev
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("files_changed never injected")
	return agent.Event{}
}

// TestDiffFileCards pins the delta rule: new paths in, XY-letter changes
// in, mtime-only moves in (an already-dirty file touched again — the
// screenshot's untracked deliverable), untouched rows out, and base-only
// paths NOT swept in (porcelain lists deletions itself; a path that left
// the dirty set was committed).
func TestDiffFileCards(t *testing.T) {
	tm := time.Now()
	base := map[string]fileSnap{
		"already-dirty.md":       {xy: "??", mtime: tm},
		"same-state.txt":         {xy: " M", mtime: tm},
		"left-the-dirty-set.txt": {xy: " M", mtime: tm},
	}
	cur := map[string]fileSnap{
		"already-dirty.md":     {xy: "??", mtime: tm.Add(time.Second)}, // touched again, letter unchanged
		"same-state.txt":       {xy: " M", mtime: tm},                   // untouched
		"brand-new.pptx":       {xy: "??", mtime: tm},                   // new since baseline
		"clean-now-modified.md": {xy: " M", mtime: tm},                  // wasn't dirty, now is (absent from base)
	}
	got := diffFileCards(base, cur)

	want := []struct {
		path, kind string
		staged     bool
	}{
		{"already-dirty.md", "untracked", false},
		{"brand-new.pptx", "untracked", false},
		{"clean-now-modified.md", "modified", false},
	}
	if len(got) != len(want) {
		t.Fatalf("delta = %+v, want %d rows", got, len(want))
	}
	for i, w := range want {
		if got[i].Path != w.path || got[i].Kind != w.kind || got[i].Staged != w.staged {
			t.Fatalf("row %d = %+v, want %+v", i, got[i], w)
		}
	}
}

// TestFileCardsFlow: the store wiring end to end — baseline at submit,
// the run's footprint on the tree, prompt_end through the hub's tap, one
// files_changed event carrying both touched files (mtime move + brand-new)
// with its seq ABOVE the prompt_end it belongs to (the tap runs after the
// push, so the card always lands under its reply).
func TestFileCardsFlow(t *testing.T) {
	scratchHome(t)
	repo, _ := newFileCardRepo(t)

	// The screenshot's shape: an untracked deliverable that already exists
	// when the run starts — the baseline sees it as ??, the run edits it
	// (letter unchanged, mtime moves) and also creates a new file.
	deliverable := filepath.Join(repo, "deliverable.md")
	if err := os.WriteFile(deliverable, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	events := make(chan agent.Event)
	h := newHub(events)
	// Store's onPromptEnd, verbatim shape (store.go): the tap only schedules.
	h.onPromptEnd = func(agent.Event) { h.scheduleFileCards(repo) }
	go h.run()
	defer close(events)

	h.beginFileCards(repo)
	waitFileBaseline(t, h)

	if err := os.WriteFile(deliverable, []byte("v2 — edited by the run\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "new-report.pptx"),
		[]byte("pptx-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	events <- agent.Event{Type: agent.EventPromptEnd}
	ev := waitFilesChanged(t, h)

	var cards []fileCard
	if err := json.Unmarshal([]byte(ev.Attrs["files"]), &cards); err != nil {
		t.Fatalf("attrs.files JSON: %v\n%s", err, ev.Attrs["files"])
	}
	if len(cards) != 2 {
		t.Fatalf("cards = %+v, want exactly the run's two files", cards)
	}
	// Path-sorted: deliverable.md before new-report.pptx.
	if cards[0].Path != "deliverable.md" || cards[1].Path != "new-report.pptx" {
		t.Fatalf("cards = %+v, want [deliverable.md new-report.pptx]", cards)
	}
	for _, c := range cards {
		if c.Kind != "untracked" || c.XY != "??" {
			t.Fatalf("card %+v: want an untracked ?? row", c)
		}
	}

	// Ordering: the card's seq is above its prompt_end's — a card rendered
	// above the reply it belongs to would read as belonging to an earlier run.
	var endSeq, cardSeq int64
	for _, it := range h.dump() {
		switch it.ev.Type {
		case agent.EventPromptEnd:
			endSeq = it.seq
		case evFilesChanged:
			cardSeq = it.seq
		}
	}
	if endSeq == 0 || cardSeq <= endSeq {
		t.Fatalf("files_changed seq %d not above prompt_end seq %d", cardSeq, endSeq)
	}
}

// TestFileCardsSkipWithoutBaseline: a seat that never got a beginFileCards
// (adopted mid-run — hubOf on a parked seat) must stay quiet. Diffing
// against nothing would card every dirty file in the workspace.
func TestFileCardsSkipWithoutBaseline(t *testing.T) {
	scratchHome(t)
	repo, _ := newFileCardRepo(t)

	events := make(chan agent.Event)
	h := newHub(events)
	h.onPromptEnd = func(agent.Event) { h.scheduleFileCards(repo) }
	go h.run()
	defer close(events)

	events <- agent.Event{Type: agent.EventPromptEnd}
	waitPromptEnd(t, h)
	// The skip branch is synchronous inside the tap; the settle only gives
	// a wrongfully spawned diff a chance to surface anyway.
	time.Sleep(150 * time.Millisecond)
	if ev := findEvent(h, evFilesChanged); ev != nil {
		t.Fatalf("files_changed without a baseline: %+v", ev)
	}
}

// TestFileCardsNotARepo: a non-repo workspace captures an empty-but-ok
// baseline (isNotARepo degrades like the files panel) — the diff then has
// nothing to say and must not inject.
func TestFileCardsNotARepo(t *testing.T) {
	scratchHome(t)
	dir := t.TempDir() // plain directory — no .git anywhere above it

	events := make(chan agent.Event)
	h := newHub(events)
	h.onPromptEnd = func(agent.Event) { h.scheduleFileCards(dir) }
	go h.run()
	defer close(events)

	h.beginFileCards(dir)
	waitFileBaseline(t, h) // empty baseline still counts as captured

	if err := os.WriteFile(filepath.Join(dir, "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	events <- agent.Event{Type: agent.EventPromptEnd}
	waitPromptEnd(t, h)
	time.Sleep(150 * time.Millisecond) // diff goroutine: not-a-repo again → empty delta
	if ev := findEvent(h, evFilesChanged); ev != nil {
		t.Fatalf("files_changed fired in a non-repo: %+v", ev)
	}
}
