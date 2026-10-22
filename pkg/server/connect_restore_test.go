package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/anthropics/anthropic-sdk-go"

	lyonv1 "lyon-agent/gen/go/lyon/v1"
	"lyon-agent/pkg/agent"
)

// writeTestSnapshot drops snap into $LYON_HOME/sessions/name.json and
// returns the absolute path — the shape ListSnapshots would hand out. It
// uses the home already in effect (the fixture's), because restore validates
// the path against the running server's root: a second temp dir here would
// make every path look like a traversal.
func writeTestSnapshot(t *testing.T, name string, snap agent.Snapshot) string {
	t.Helper()
	home := os.Getenv("LYON_HOME")
	if home == "" {
		t.Fatalf("writeTestSnapshot needs $LYON_HOME (build the fixture first)")
	}
	dir := filepath.Join(home, "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir sessions: %v", err)
	}
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	return p
}

// restoreMessages is a small but complete conversation: prompt, answer,
// one parallel-ish tool exchange with result text.
func restoreMessages() []anthropic.BetaMessageParam {
	return []anthropic.BetaMessageParam{
		anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("第一条问题")),
		{Role: anthropic.BetaMessageParamRoleAssistant, Content: []anthropic.BetaContentBlockParamUnion{
			{OfText: &anthropic.BetaTextBlockParam{Text: "回答**内容**"}},
		}},
		{Role: anthropic.BetaMessageParamRoleAssistant, Content: []anthropic.BetaContentBlockParamUnion{
			{OfToolUse: &anthropic.BetaToolUseBlockParam{
				ID: "toolu_1", Name: "run_shell",
				Input: map[string]any{"command": "ls"},
			}},
		}},
		anthropic.NewBetaUserMessage(anthropic.BetaContentBlockParamUnion{
			OfToolResult: &anthropic.BetaToolResultBlockParam{
				ToolUseID: "toolu_1",
				Content: []anthropic.BetaToolResultBlockParamContentUnion{
					{OfText: &anthropic.BetaTextBlockParam{Text: "file.go"}},
				},
			},
		}),
	}
}

// TestConnectRestoreSession: a stored snapshot restores into a live
// session whose WatchEvents(from_seq=-1) replays the full transcript
// (user_message → text → tool card lifecycle) and then keeps accepting
// prompts like any fresh session — the Phase A "点不进来" loop closed.
func TestConnectRestoreSession(t *testing.T) {
	client := newTranscriptFixture(t, time.Millisecond)
	path := writeTestSnapshot(t, "hist.json", agent.Snapshot{
		Version:   1,
		Workspace: "/w",
		Model:     "m",
		Title:     "恢复的历史",
		SavedAt:   time.Now(),
		Messages:  restoreMessages(),
	})
	ctx := context.Background()

	res, err := client.RestoreSession(ctx, connect.NewRequest(&lyonv1.RestoreSessionRequest{Path: path}))
	if err != nil {
		t.Fatalf("RestoreSession: %v", err)
	}
	if res.Msg.RestoredMessages != int32(len(restoreMessages())) {
		t.Fatalf("restored_messages = %d, want %d", res.Msg.RestoredMessages, len(restoreMessages()))
	}
	sid := res.Msg.Session.Id
	if res.Msg.Session.Title != "恢复的历史" {
		t.Fatalf("title = %q, want restored title", res.Msg.Session.Title)
	}

	// Replay: contiguous seqs, transcript order, exec settling the card.
	rctx, rcancel := context.WithTimeout(ctx, 10*time.Second)
	defer rcancel()
	stream, err := client.WatchEvents(rctx, connect.NewRequest(&lyonv1.WatchEventsRequest{
		SessionId: sid, FromSeq: -1,
	}))
	if err != nil {
		t.Fatalf("WatchEvents: %v", err)
	}
	var kinds []string
	var texts []string
	var restoredSeq int64
	for stream.Receive() {
		ev := stream.Msg().GetEvent()
		if ev == nil {
			continue
		}
		kinds = append(kinds, ev.TypeName)
		texts = append(texts, ev.Text)
		restoredSeq = ev.Seq
		if ev.TypeName == "tool_exec_end" {
			break // last synthesized event of this history
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream: %v", err)
	}
	wantKinds := []string{
		"user_message", "text_delta",
		"tool_use_start", "tool_exec_start", "tool_exec_output", "tool_exec_end",
	}
	if len(kinds) != len(wantKinds) {
		t.Fatalf("replay kinds = %v, want %v", kinds, wantKinds)
	}
	for i, k := range wantKinds {
		if kinds[i] != k {
			t.Fatalf("replay kinds = %v, want %v", kinds, wantKinds)
		}
	}
	if texts[0] != "第一条问题" {
		t.Fatalf("first text = %q", texts[0])
	}
	// seqs must have run 1..n through the replay (rcancel stops the live
	// tail; only the injected batch is asserted).
	if restoredSeq != int64(len(wantKinds)) {
		t.Fatalf("last replay seq = %d, want %d", restoredSeq, len(wantKinds))
	}

	// The restored session is a first-class citizen: a new prompt runs on
	// top of the seeded history and its echo stamps after the replay.
	if _, err := client.SubmitPrompt(ctx, connect.NewRequest(&lyonv1.SubmitPromptRequest{
		SessionId: sid, Prompt: "继续",
	})); err != nil {
		t.Fatalf("SubmitPrompt after restore: %v", err)
	}
	var last int64
	var echo bool
	for _, ev := range collectPromptEvents(t, client, sid) {
		last = ev.Seq
		if ev.TypeName == "user_message" && ev.Text == "继续" {
			echo = true
		}
	}
	if !echo {
		t.Fatal("post-restore prompt echo missing from replay")
	}
	if last <= int64(len(wantKinds)) {
		t.Fatalf("live seq %d did not advance past restored batch %d", last, len(wantKinds))
	}
}

// TestConnectRestorePathValidation: containment, missing, and corrupt
// inputs each fail with their documented code before anything outside
// $LYON_HOME/sessions is read.
func TestConnectRestorePathValidation(t *testing.T) {
	client := newTranscriptFixture(t, time.Millisecond)
	// The fixture pinned $LYON_HOME; the paths below must live under the
	// root the running server validates against.
	home := os.Getenv("LYON_HOME")
	sessions := filepath.Join(home, "sessions")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessions, "corrupt.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A file OUTSIDE the sessions root must be rejected even though it
	// sits inside $LYON_HOME.
	if err := os.WriteFile(filepath.Join(home, "elsewhere.json"), []byte(`{"Version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	for _, tc := range []struct {
		name string
		path string
		code connect.Code
	}{
		{"traversal", filepath.Join(home, "sessions", "..", "..", "x.json"), connect.CodeInvalidArgument},
		{"outside-root", filepath.Join(home, "elsewhere.json"), connect.CodeInvalidArgument},
		{"empty", "", connect.CodeInvalidArgument},
		{"missing", filepath.Join(sessions, "nope.json"), connect.CodeNotFound},
		{"corrupt", filepath.Join(sessions, "corrupt.json"), connect.CodeDataLoss},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.RestoreSession(ctx, connect.NewRequest(&lyonv1.RestoreSessionRequest{Path: tc.path}))
			if got := connectCode(err); got != tc.code {
				t.Fatalf("code = %v, want %v (err=%v)", got, tc.code, err)
			}
		})
	}
}

// TestStoreTeardownKeepsSeats: the store's two teardown paths now differ on
// purpose. Delete destroys (no archive: an archived delete comes back as a
// SAVED row), while shutdown parks the seat on disk under its own id so the
// next process lists and adopts it. Neither writes into the snapshot archive.
func TestStoreTeardownKeepsSeats(t *testing.T) {
	scratchHome(t)
	eng := agent.NewEngine(agent.DefaultConfig(), nil)
	ws := eng.Config().Workspace
	st := NewStore(0, 0)

	seeded := func(title, prompt string) *agent.Session {
		sess := eng.NewSession(nil)
		sess.Restore(agent.Snapshot{
			Version: 1, Workspace: ws, Title: title, SavedAt: time.Now(),
			Messages: []anthropic.BetaMessageParam{
				anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(prompt)),
			},
		})
		return sess
	}

	// Path 1: Delete — the conversation is gone for good.
	s1 := seeded("delete-exits", "delete prompt")
	id1, err := st.Create(s1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !st.Delete(id1) {
		t.Fatal("Delete = false")
	}
	if _, parked := st.Parked(id1); parked {
		t.Fatal("deleted seat is still parked")
	}
	if _, err := os.Stat(mustDraftPath(t, ws, id1)); !os.IsNotExist(err) {
		t.Fatalf("draft survived delete: %v", err)
	}

	// Path 2: CloseAll (lyond shutdown) — parked under the same id.
	s2 := seeded("shutdown-exits", "shutdown prompt")
	id2, err := st.Create(s2)
	if err != nil {
		t.Fatalf("Create #2: %v", err)
	}
	st.CloseAll()
	snap, parked := st.Parked(id2)
	if !parked {
		t.Fatalf("shutdown dropped seat %q instead of parking it", id2)
	}
	if snap.Snap.Title != "shutdown-exits" {
		t.Fatalf("parked title = %q", snap.Snap.Title)
	}
	if _, err := os.Stat(snap.Path); err != nil {
		t.Fatalf("parked dump missing: %v", err)
	}

	// The snapshot store stays empty either way: seats are not SAVED rows.
	refs, err := agent.ListSnapshots(ws)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("teardown wrote archive entries = %+v; every restart would add a SAVED row", refs)
	}
}
