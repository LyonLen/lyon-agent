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
	t.Setenv("LYON_HOME", t.TempDir())

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

// TestDraftSweepRecoversCrash: the restart path. SweepDrafts stands in for
// the process that never got to run CloseAll: the draft becomes an archive
// entry (visible to ListSnapshots → web "saved") and the draft file goes.
func TestDraftSweepRecoversCrash(t *testing.T) {
	client, _, ws := newDraftFixture(t)
	sid := runOnePrompt(t, client, "survive the crash")
	waitDraft(t, ws, sid)

	n, err := agent.SweepDrafts(ws)
	if err != nil {
		t.Fatalf("SweepDrafts: %v", err)
	}
	if n != 1 {
		t.Fatalf("recovered %d drafts, want 1", n)
	}
	refs, err := agent.ListSnapshots(ws)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(refs) != 1 || refs[0].Messages < 2 {
		t.Fatalf("ListSnapshots after sweep = %+v, want the recovered conversation", refs)
	}
	if _, err := os.Stat(mustDraftPath(t, ws, sid)); !os.IsNotExist(err) {
		t.Fatalf("draft file survived the sweep: %v", err)
	}

	// A second sweep (nothing left) must be a quiet no-op — the next
	// lyond start runs it unconditionally.
	if n, err := agent.SweepDrafts(ws); err != nil || n != 0 {
		t.Fatalf("second sweep = (%d, %v), want (0, nil)", n, err)
	}
}

// TestDraftRemovedOnDelete: the clean exit path already archives the final
// state, so the draft is redundant and must be deleted with the session —
// otherwise the next startup would recover a conversation that is already
// saved (deduped, but pointless I/O), or worse a stale subset of it.
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
	if len(refs) != 1 || refs[0].Messages < 2 {
		t.Fatalf("archive after Delete = %+v, want the conversation saved once", refs)
	}
}

// TestDraftDirShape pins where drafts live: inside the workspace's archive
// directory (so the snapshot store has one root) but in a subdirectory the
// archive scan ignores.
func TestDraftDirShape(t *testing.T) {
	home := t.TempDir()
	t.Setenv("LYON_HOME", home)
	ws := t.TempDir()
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
