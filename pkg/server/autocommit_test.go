package server

// Auto-commit + /undo tests (blueprint §四): the side ref's invariants —
// chain, message format, HEAD/branch/index untouched, foreign WIP kept
// out — and undo's restore/delete walk with its NotFound floor. All
// fixtures are scratch repos; the real checkout is never touched.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	lyonv1 "lyon-agent/gen/go/lyon/v1"
	"lyon-agent/gen/go/lyon/v1/lyonv1connect"

	"lyon-agent/pkg/agent"
)

// newAutoCommitRepo is newFileCardRepo plus a seed commit — the side-ref
// chain needs a HEAD to hang its first parent on (an unborn HEAD skips
// the commit by design).
func newAutoCommitRepo(t *testing.T) (string, func(args ...string)) {
	t.Helper()
	repo, run := newFileCardRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "seed.md"), []byte("seed v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "seed.md")
	run("commit", "-m", "seed")
	return repo, run
}

// gitTry runs git in repo and returns raw output + error (for calls whose
// failure is itself the assertion — cat-file of an absent blob).
func gitTry(repo string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// gitOut runs git in repo and fails the test on error; output is raw
// (blob content keeps its exact bytes — trim at the call site).
func gitOut(t *testing.T, repo string, args ...string) string {
	t.Helper()
	out, err := gitTry(repo, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// waitAutoRef polls until the session's side ref exists (the commit runs
// after files_changed in the same goroutine — never assume its latency).
func waitAutoRef(t *testing.T, repo, sid string) string {
	t.Helper()
	ref := "refs/lyon/auto/" + sid
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if out, err := gitTry(repo, "rev-parse", "--verify", "--quiet", ref); err == nil {
			if sha := strings.TrimSpace(out); sha != "" {
				return sha
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("side ref %s never appeared", ref)
	return ""
}

func findSeq(h *hub, typ agent.EventType) int64 {
	for _, it := range h.dump() {
		if it.ev.Type == typ {
			return it.seq
		}
	}
	return 0
}

// writeRepoFile is the run-footprint shorthand: a workspace write between
// baseline and prompt_end.
func writeRepoFile(t *testing.T, repo, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestAutoCommitSideRef: a run that touched files records itself on
// refs/lyon/auto/<sid> — message in the lyon(<sid8>): <summary> format,
// first link's parent = the HEAD it started from, and HEAD / the branch /
// the user's index all untouched (the run's changes are still unstaged
// worktree rows after the commit).
func TestAutoCommitSideRef(t *testing.T) {
	scratchHome(t)
	repo, _ := newAutoCommitRepo(t)
	sid := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	headBefore := strings.TrimSpace(gitOut(t, repo, "rev-parse", "HEAD"))
	branchBefore := strings.TrimSpace(gitOut(t, repo, "symbolic-ref", "HEAD"))

	events := make(chan agent.Event)
	h := newHub(events)
	h.onPromptEnd = func(agent.Event) { h.scheduleFileCards(repo, "make the deck v2") }
	h.sid, h.ws = sid, repo
	go h.run()
	defer close(events)

	h.beginFileCards(repo)
	waitFileBaseline(t, h)

	// The run's footprint: modify the tracked seed, create a new file.
	writeRepoFile(t, repo, "seed.md", "seed v2 — run edit\n")
	writeRepoFile(t, repo, "new-report.pptx", "pptx-bytes")

	events <- agent.Event{Type: agent.EventPromptEnd}
	waitFilesChanged(t, h)
	waitAutoRef(t, repo, sid)

	// Message format (BACKLOG): lyon(<sid[:8]>): <prompt 摘要>.
	subj := strings.TrimSpace(gitOut(t, repo, "log", "-1", "--format=%s",
		"refs/lyon/auto/"+sid))
	if want := "lyon(aaaaaaaa): make the deck v2"; subj != want {
		t.Fatalf("commit subject = %q, want %q", subj, want)
	}

	// HEAD and the branch never moved; the first chain link's parent is
	// the HEAD the run started from.
	if head := strings.TrimSpace(gitOut(t, repo, "rev-parse", "HEAD")); head != headBefore {
		t.Fatalf("HEAD moved: %s → %s", headBefore, head)
	}
	if branch := strings.TrimSpace(gitOut(t, repo, "symbolic-ref", "HEAD")); branch != branchBefore {
		t.Fatalf("branch moved: %s → %s", branchBefore, branch)
	}
	if parent := strings.TrimSpace(gitOut(t, repo, "rev-parse", "refs/lyon/auto/"+sid+"^")); parent != headBefore {
		t.Fatalf("chain root parent = %s, want HEAD %s", parent, headBefore)
	}

	// The user's index is untouched: both run changes are still unstaged
	// worktree rows, nothing staged.
	status := strings.ReplaceAll(gitOut(t, repo, "status", "--porcelain"), "\r\n", "\n")
	for _, row := range strings.Split(status, "\n") {
		if row == "" {
			continue
		}
		if row[0] != ' ' && row[0] != '?' {
			t.Fatalf("auto-commit staged into the user's index: %q", row)
		}
	}
	for _, want := range []string{" M seed.md\n", "?? new-report.pptx\n"} {
		if !strings.Contains(status, want) {
			t.Fatalf("status missing %q:\n%s", want, status)
		}
	}

	// The chain's tree carries the run's content (worktree state at
	// commit time), not the pre-run blob.
	if blob := gitOut(t, repo, "cat-file", "-p", "refs/lyon/auto/"+sid+":seed.md"); blob != "seed v2 — run edit\n" {
		t.Fatalf("tree seed.md = %q, want the run's content", blob)
	}
}

// TestAutoCommitSkipsCleanRun: a run with no file changes produces no
// card and no side-ref commit — no empty undo points on the chain.
func TestAutoCommitSkipsCleanRun(t *testing.T) {
	scratchHome(t)
	repo, _ := newAutoCommitRepo(t)
	sid := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	events := make(chan agent.Event)
	h := newHub(events)
	h.onPromptEnd = func(agent.Event) { h.scheduleFileCards(repo, "just chatting") }
	h.sid, h.ws = sid, repo
	go h.run()
	defer close(events)

	h.beginFileCards(repo)
	waitFileBaseline(t, h)

	// Run footprint: nothing — the tree is as it was at baseline.
	events <- agent.Event{Type: agent.EventPromptEnd}
	waitPromptEnd(t, h)

	// Poll: the goroutine had every chance to (wrongly) commit.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if out, err := gitTry(repo, "rev-parse", "--verify", "--quiet",
			"refs/lyon/auto/"+sid); err == nil && strings.TrimSpace(out) != "" {
			t.Fatalf("clean run must not create a side ref: %s", out)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if findEvent(h, evFilesChanged) != nil {
		t.Fatal("clean run must not inject files_changed")
	}
}

// TestAutoCommitNotARepo: a non-repo workspace degrades completely —
// both captures are successful EMPTY baselines (captureFileBaseline's
// isNotARepo contract), so no delta, no card, no commit attempt, and no
// error event ever lands in the ring. The git layer's side of the same
// coin: writeAutoCommit on a plain directory fails with the benign
// not-a-repo error the caller swallows.
func TestAutoCommitNotARepo(t *testing.T) {
	scratchHome(t)
	dir := t.TempDir()
	sid := "cccccccccccccccccccccccccccccccc"

	events := make(chan agent.Event)
	h := newHub(events)
	h.onPromptEnd = func(agent.Event) { h.scheduleFileCards(dir, "do the thing") }
	h.sid, h.ws = sid, dir
	go h.run()
	defer close(events)

	h.beginFileCards(dir)
	waitFileBaseline(t, h)
	writeRepoFile(t, dir, "deliverable.txt", "made outside any repo")

	events <- agent.Event{Type: agent.EventPromptEnd}
	waitPromptEnd(t, h)
	time.Sleep(150 * time.Millisecond) // diff goroutine: not-a-repo again → empty delta

	var types []string
	for _, it := range h.dump() {
		types = append(types, string(it.ev.Type))
	}
	if len(types) != 1 || types[0] != string(agent.EventPromptEnd) {
		t.Fatalf("ring = %v, want exactly [prompt_end] (no card, no error)", types)
	}

	// The commit layer degrades with the same benign error.
	err := writeAutoCommit(context.Background(), dir, sid, "anything",
		[]string{"deliverable.txt"})
	if err == nil || !isNotARepo(err) {
		t.Fatalf("writeAutoCommit on a plain dir = %v, want the benign not-a-repo error", err)
	}
}

// TestAutoCommitExcludesForeignWIP: user WIP that existed BEFORE the
// baseline never enters the auto-commit's tree — only the run's paths do.
// The WIP itself stays in the worktree, still dirty, still uncommitted.
func TestAutoCommitExcludesForeignWIP(t *testing.T) {
	scratchHome(t)
	repo, _ := newAutoCommitRepo(t)
	sid := "dddddddddddddddddddddddddddddddd"

	// User WIP before the run even starts: a dirty tracked file and a
	// stray untracked note.
	writeRepoFile(t, repo, "seed.md", "user wip — not the run's doing\n")
	writeRepoFile(t, repo, "wip.txt", "user notes\n")

	events := make(chan agent.Event)
	h := newHub(events)
	h.onPromptEnd = func(agent.Event) { h.scheduleFileCards(repo, "run one") }
	h.sid, h.ws = sid, repo
	go h.run()
	defer close(events)

	h.beginFileCards(repo)
	waitFileBaseline(t, h)

	// The run's footprint: ONE new file, nothing else.
	writeRepoFile(t, repo, "run-output.md", "run output\n")

	events <- agent.Event{Type: agent.EventPromptEnd}
	ev := waitFilesChanged(t, h)

	// The delta (and therefore the commit's path set) is run-output.md only.
	var cards []fileCard
	if err := json.Unmarshal([]byte(ev.Attrs["files"]), &cards); err != nil {
		t.Fatalf("attrs.files JSON: %v", err)
	}
	if len(cards) != 1 || cards[0].Path != "run-output.md" {
		t.Fatalf("cards = %+v, want only run-output.md (pre-baseline WIP is not this run's delta)", cards)
	}
	waitAutoRef(t, repo, sid)

	ref := "refs/lyon/auto/" + sid
	// The tree has the run's file…
	if blob := gitOut(t, repo, "cat-file", "-p", ref+":run-output.md"); blob != "run output\n" {
		t.Fatalf("tree run-output.md = %q", blob)
	}
	// …seed.md at the PARENT's content (user WIP not swept in)…
	if blob := gitOut(t, repo, "cat-file", "-p", ref+":seed.md"); blob != "seed v1\n" {
		t.Fatalf("tree seed.md = %q, want the committed seed (user WIP excluded)", blob)
	}
	// …and wip.txt absent entirely.
	if _, err := gitTry(repo, "cat-file", "-e", ref+":wip.txt"); err == nil {
		t.Fatal("untracked user WIP wip.txt leaked into the auto-commit tree")
	}

	// The WIP itself is still in the worktree, untouched and uncommitted.
	status := strings.ReplaceAll(gitOut(t, repo, "status", "--porcelain"), "\r\n", "\n")
	for _, want := range []string{" M seed.md\n", "?? wip.txt\n", "?? run-output.md\n"} {
		if !strings.Contains(status, want) {
			t.Fatalf("status missing %q after commit:\n%s", want, status)
		}
	}
}

// TestUndoLastRun: the chain walk — two runs record two links; each undo
// restores that run's paths (modified → parent content, created →
// removed), steps the ref onto the parent while the parent is still an
// auto-commit, deletes the ref at the chain's root, and the next call is
// NotFound. The user's index stays out of it throughout; after the full
// walk the worktree matches HEAD again.
func TestUndoLastRun(t *testing.T) {
	scratchHome(t)
	repo, run := newAutoCommitRepo(t)
	sid := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	ctx := context.Background()

	// A second tracked file, committed — the chain will hang off it.
	writeRepoFile(t, repo, "other.md", "other v1\n")
	run("add", "other.md")
	run("commit", "-m", "other")

	// Run 1: edit seed.md, create created.md.
	writeRepoFile(t, repo, "seed.md", "v2 — run one\n")
	writeRepoFile(t, repo, "created.md", "fresh\n")
	if err := writeAutoCommit(ctx, repo, sid, "run one: edit seed, create created",
		[]string{"seed.md", "created.md"}); err != nil {
		t.Fatalf("run 1 auto-commit: %v", err)
	}

	// Run 2: edit other.md.
	writeRepoFile(t, repo, "other.md", "v2 — run two\n")
	if err := writeAutoCommit(ctx, repo, sid, "run two: edit other",
		[]string{"other.md"}); err != nil {
		t.Fatalf("run 2 auto-commit: %v", err)
	}

	ref := "refs/lyon/auto/" + sid
	run2 := strings.TrimSpace(gitOut(t, repo, "rev-parse", ref))
	run1 := strings.TrimSpace(gitOut(t, repo, "rev-parse", ref+"^"))

	// Undo #1: only run 2's path reverts; the ref steps onto run 1.
	paths, refAfter, err := undoLastRun(ctx, repo, sid)
	if err != nil {
		t.Fatalf("undo #1: %v", err)
	}
	if len(paths) != 1 || paths[0] != "other.md" {
		t.Fatalf("undo #1 restored %v, want [other.md]", paths)
	}
	if got := readWorktree(t, repo, "other.md"); got != "other v1\n" {
		t.Fatalf("other.md after undo #1 = %q", got)
	}
	if strings.TrimSpace(gitOut(t, repo, "rev-parse", ref)) != run1 {
		t.Fatalf("ref after undo #1 = %s, want run 1 %s",
			strings.TrimSpace(gitOut(t, repo, "rev-parse", ref)), run1)
	}
	if refAfter == "" || refAfter == run2 {
		t.Fatalf("response ref = %q, want run 1's short sha", refAfter)
	}

	// Undo #2: run 1's paths — modified restored, created removed — and
	// the chain root (parent = the user's "other" commit) deletes the ref.
	paths, refAfter, err = undoLastRun(ctx, repo, sid)
	if err != nil {
		t.Fatalf("undo #2: %v", err)
	}
	sort.Strings(paths)
	if len(paths) != 2 || paths[0] != "created.md" || paths[1] != "seed.md" {
		t.Fatalf("undo #2 restored %v, want [created.md seed.md]", paths)
	}
	if got := readWorktree(t, repo, "seed.md"); got != "seed v1\n" {
		t.Fatalf("seed.md after undo #2 = %q", got)
	}
	if _, err := os.Stat(filepath.Join(repo, "created.md")); !os.IsNotExist(err) {
		t.Fatalf("created.md after undo #2: stat err = %v, want removed", err)
	}
	if refAfter != "" {
		t.Fatalf("response ref = %q, want \"\" (chain exhausted, ref deleted)", refAfter)
	}
	if out, err := gitTry(repo, "rev-parse", "--verify", "--quiet", ref); err == nil && strings.TrimSpace(out) != "" {
		t.Fatalf("ref still resolves after the chain root: %s", out)
	}

	// Worktree matches HEAD again — the full walk left no residue (and
	// the index was never in play).
	if status := strings.TrimSpace(gitOut(t, repo, "status", "--porcelain")); status != "" {
		t.Fatalf("status after full undo = %q, want clean", status)
	}

	// Undo #3: nothing left.
	_, _, err = undoLastRun(ctx, repo, sid)
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("undo #3 code = %v, want NotFound (err = %v)", connect.CodeOf(err), err)
	}
}

// TestUndoLastRunRPC: the handler over the real wire — a live session's
// side ref restores through Store.Get/Hub, the response carries the flash
// list, and a second call falls to NotFound. (No prompt runs: the session
// is empty, so CloseAll's distill pass stays silent too.)
func TestUndoLastRunRPC(t *testing.T) {
	scratchHome(t)
	repo, _ := newAutoCommitRepo(t)

	cfg := agent.DefaultConfig()
	cfg.Workspace = repo
	st := NewStore(0, 0)
	s := New(agent.NewEngine(cfg, nil), st)
	t.Cleanup(st.CloseAll)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	client := lyonv1connect.NewSessionServiceClient(http.DefaultClient, ts.URL)
	ctx := context.Background()

	cres, err := client.CreateSession(ctx, connect.NewRequest(&lyonv1.CreateSessionRequest{}))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sid := cres.Msg.Session.Id

	// A run's footprint, recorded on this session's side ref directly.
	writeRepoFile(t, repo, "seed.md", "v2 — rpc run\n")
	if err := writeAutoCommit(ctx, repo, sid, "rpc run", []string{"seed.md"}); err != nil {
		t.Fatalf("auto-commit: %v", err)
	}

	res, err := client.UndoLastRun(ctx, connect.NewRequest(&lyonv1.UndoLastRunRequest{SessionId: sid}))
	if err != nil {
		t.Fatalf("UndoLastRun: %v", err)
	}
	if got := res.Msg.RestoredPaths; len(got) != 1 || got[0] != "seed.md" {
		t.Fatalf("restored = %v, want [seed.md]", got)
	}
	if got := readWorktree(t, repo, "seed.md"); got != "seed v1\n" {
		t.Fatalf("seed.md after RPC undo = %q", got)
	}
	if res.Msg.Ref != "" {
		t.Fatalf("ref = %q, want \"\" (chain exhausted)", res.Msg.Ref)
	}

	// Second call: nothing left to undo.
	_, err = client.UndoLastRun(ctx, connect.NewRequest(&lyonv1.UndoLastRunRequest{SessionId: sid}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("second call code = %v, want NotFound (err = %v)", connect.CodeOf(err), err)
	}
}

// TestAutoCommitTapNonBlocking: the tap only SCHEDULES. A marker event
// pushed right after prompt_end stamps BEFORE files_changed (which needs
// a full git status pass first) — if the tap ever ran git inline, the
// card would stamp first and this ordering would invert.
func TestAutoCommitTapNonBlocking(t *testing.T) {
	scratchHome(t)
	repo, _ := newAutoCommitRepo(t)
	sid := "ffffffffffffffffffffffffffffffff"

	events := make(chan agent.Event)
	h := newHub(events)
	h.onPromptEnd = func(agent.Event) { h.scheduleFileCards(repo, "prompt for the marker test") }
	h.sid, h.ws = sid, repo
	go h.run()
	defer close(events)

	h.beginFileCards(repo)
	waitFileBaseline(t, h)
	writeRepoFile(t, repo, "seed.md", "v2 — marker run\n") // non-empty delta

	events <- agent.Event{Type: agent.EventPromptEnd}
	events <- agent.Event{Type: agent.EventType("tap_marker")}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		marker, card := findSeq(h, agent.EventType("tap_marker")), findSeq(h, evFilesChanged)
		if marker != 0 && card != 0 {
			if marker >= card {
				t.Fatalf("marker seq %d not before files_changed seq %d — the tap blocked on git", marker, card)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("marker or files_changed never stamped")
}

// readWorktree returns a file's content under repo, EOL-normalized to
// LF: core.autocrlf=true makes git restore materialize CRLF on this box,
// while the blobs (and the assertions) speak LF.
func readWorktree(t *testing.T, repo, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repo, name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}
