package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	lyonv1 "lyon-agent/gen/go/lyon/v1"
	"lyon-agent/gen/go/lyon/v1/lyonv1connect"

	"lyon-agent/internal/mockapi"
	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/tools"
)

// newDraftFixture serves a live server on its own $LYON_HOME and returns the
// session client, the store (for teardown-path assertions) and the workspace.
func newDraftFixture(t *testing.T) (lyonv1connect.SessionServiceClient, *Store, string) {
	t.Helper()
	mock := mockapi.New(mockapi.Config{
		Script:  []mockapi.Turn{{Text: []string{"draft-", "ok"}}},
		Latency: time.Millisecond,
	})
	t.Cleanup(mock.Close)
	t.Setenv("ANTHROPIC_BASE_URL", mock.URL())
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "draft-test-token")
	scratchHome(t)

	eng := agent.NewEngine(agent.DefaultConfig(), nil)
	st := NewStore(0, 0)
	t.Cleanup(st.CloseAll)
	s := New(eng, st)
	s.SetObserver(tools.NewMetrics())
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return lyonv1connect.NewSessionServiceClient(http.DefaultClient, ts.URL), st, eng.Config().Workspace
}

// runOnePrompt creates a session and drives one prompt through prompt_end.
func runOnePrompt(t *testing.T, client lyonv1connect.SessionServiceClient, prompt string) string {
	t.Helper()
	ctx := context.Background()
	cres, err := client.CreateSession(ctx, connect.NewRequest(&lyonv1.CreateSessionRequest{}))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sid := cres.Msg.Session.Id
	if _, err := client.SubmitPrompt(ctx, connect.NewRequest(&lyonv1.SubmitPromptRequest{
		SessionId: sid, Prompt: prompt,
	})); err != nil {
		t.Fatalf("SubmitPrompt: %v", err)
	}
	collectPromptEvents(t, client, sid)
	return sid
}

// waitDraft polls the session's draft slot until it holds messages.
func waitDraft(t *testing.T, ws, id string) agent.Snapshot {
	t.Helper()
	path, err := agent.DraftPath(ws, id)
	if err != nil {
		t.Fatalf("DraftPath: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		snap, err := agent.LoadSnapshotFrom(path)
		if err != nil {
			t.Fatalf("draft unreadable: %v", err)
		}
		if snap != nil {
			return *snap
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no draft at %s after prompt_end", path)
	return agent.Snapshot{}
}

// TestDraftMirrorsPromptEnd: #45 — a live session writes its conversation to
// the draft slot as soon as a prompt finishes, so a hard kill (which runs no
// teardown at all) still leaves the conversation on disk. The draft must stay
// out of ListSnapshots while the session is live: it would otherwise show up
// twice — once as a live row, once as a saved one.
func TestDraftMirrorsPromptEnd(t *testing.T) {
	client, _, ws := newDraftFixture(t)
	sid := runOnePrompt(t, client, "draft me before the crash")

	snap := waitDraft(t, ws, sid)
	if len(snap.Messages) < 2 {
		t.Fatalf("draft has %d messages, want the prompt + answer", len(snap.Messages))
	}
	if snap.FirstPrompt != "draft me before the crash" {
		t.Fatalf("draft FirstPrompt = %q", snap.FirstPrompt)
	}
	refs, err := agent.ListSnapshots(ws)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	for _, r := range refs {
		if r.Path == mustDraftPath(t, ws, sid) {
			t.Fatal("live draft leaked into ListSnapshots — the session would list twice")
		}
	}
	if len(refs) != 0 {
		t.Fatalf("ListSnapshots = %v, want empty while the session is live", refs)
	}
}

// TestDraftSurvivesRestart: the restart path. A process that ends — cleanly
// or not — leaves the conversation in its seat's draft, and the next lyond
// indexes that dump (LoadDrafts) instead of sweeping it into an archive. The
// seat comes back under the same id, the list shows one row, and the first
// RPC to it reads the conversation back from disk (acquire) rather than
// minting a duplicate or 404ing.
func TestDraftSurvivesRestart(t *testing.T) {
	client, st, ws := newDraftFixture(t)
	sid := runOnePrompt(t, client, "survive the crash")
	live := waitDraft(t, ws, sid)

	st.CloseAll() // the process goes away; the dump stays

	// A fresh process: new engine, new store, same $LYON_HOME.
	eng2 := agent.NewEngine(agent.DefaultConfig(), nil)
	st2 := NewStore(0, 0)
	t.Cleanup(st2.CloseAll)
	n, err := st2.LoadDrafts(ws)
	if err != nil {
		t.Fatalf("LoadDrafts: %v", err)
	}
	if n != 1 {
		t.Fatalf("indexed %d seats, want 1", n)
	}
	if st2.Len() != 0 {
		t.Fatalf("LoadDrafts adopted %d sessions; startup must only index", st2.Len())
	}
	refs := st2.List()
	if len(refs) != 1 || refs[0].ID != sid {
		t.Fatalf("List after restart = %+v, want one seat %q", refs, sid)
	}
	if refs[0].Session != nil {
		t.Fatal("restart row is live; a re-indexed seat must be disk-backed")
	}

	// Nothing was archived: the rows the web shows as SAVED stay empty, so
	// the same conversation cannot appear twice.
	snaps, err := agent.ListSnapshots(ws)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(snaps) != 0 {
		t.Fatalf("restart archived conversations: %+v", snaps)
	}

	// First touch adopts it back under the same id, with its history.
	s2 := New(eng2, st2)
	s2.SetObserver(tools.NewMetrics())
	ts := httptest.NewServer(s2.Handler())
	t.Cleanup(ts.Close)
	client2 := lyonv1connect.NewSessionServiceClient(http.DefaultClient, ts.URL)

	ctx := context.Background()
	sres, err := client2.GetSession(ctx, connect.NewRequest(&lyonv1.GetSessionRequest{SessionId: sid}))
	if err != nil {
		t.Fatalf("GetSession after restart: %v", err)
	}
	if sres.Msg.Session.Title == "" {
		t.Fatalf("adopted seat lost its title: %+v", sres.Msg.Session)
	}
	hres, err := client2.GetHistory(ctx, connect.NewRequest(&lyonv1.GetHistoryRequest{SessionId: sid}))
	if err != nil {
		t.Fatalf("GetHistory after restart: %v", err)
	}
	var sawPrompt bool
	for _, ev := range hres.Msg.Events {
		if ev.TypeName == "user_message" && ev.Text == "survive the crash" {
			sawPrompt = true
		}
	}
	if !sawPrompt {
		t.Fatalf("adopted history does not replay the prompt: %+v", hres.Msg.Events)
	}
	if st2.Len() != 1 {
		t.Fatalf("adopted seats live = %d, want 1", st2.Len())
	}
	// Adoption is not a rewrite: the same file, now the live seat's mirror.
	if now := waitDraft(t, ws, sid); len(now.Messages) < len(live.Messages) {
		t.Fatalf("adopted draft shrank: %d < %d messages", len(now.Messages), len(live.Messages))
	}
}

// TestDraftRemovedOnDelete: Delete is the one path that destroys. The draft
// goes with the seat and nothing is archived — an archived delete comes back
// as a SAVED row, which is exactly the "deleted it twice and it's still
// there" bug.
func TestDraftRemovedOnDelete(t *testing.T) {
	client, st, ws := newDraftFixture(t)
	sid := runOnePrompt(t, client, "deleted cleanly")
	waitDraft(t, ws, sid)

	if !st.Delete(sid) {
		t.Fatal("Delete = false")
	}
	if _, err := os.Stat(mustDraftPath(t, ws, sid)); !os.IsNotExist(err) {
		t.Fatalf("draft file survived Delete: %v", err)
	}
	refs, err := agent.ListSnapshots(ws)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("archive after Delete = %+v, want nothing (delete must not resurrect)", refs)
	}
	// And the seat is gone from the store, not merely unloaded.
	if _, parked := st.Parked(sid); parked {
		t.Fatal("deleted seat is still parked")
	}
}

// TestDraftDirShape pins where drafts live: inside the workspace's archive
// directory (so the snapshot store has one root) but in a subdirectory the
// archive scan ignores.
func TestDraftDirShape(t *testing.T) {
	scratchHome(t)
	ws := scratchHome(t)
	path, err := agent.DraftPath(ws, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	latest, err := agent.SnapshotPath(ws)
	if err != nil {
		t.Fatal(err)
	}
	archiveDir := strings.TrimSuffix(latest, ".json") // sessions/<hash>
	if want := filepath.Join(archiveDir, "drafts", "abc123.json"); path != want {
		t.Fatalf("draft path = %q, want %q", path, want)
	}
	if filepath.Dir(path) == archiveDir {
		t.Fatalf("drafts dir collides with the archive dir %q", archiveDir)
	}
}

func mustDraftPath(t *testing.T, ws, id string) string {
	t.Helper()
	p, err := agent.DraftPath(ws, id)
	if err != nil {
		t.Fatalf("DraftPath: %v", err)
	}
	return p
}
