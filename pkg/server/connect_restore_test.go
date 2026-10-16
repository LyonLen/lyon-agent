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
// returns the absolute path — the shape ListSnapshots would hand out.
func writeTestSnapshot(t *testing.T, name string, snap agent.Snapshot) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("LYON_HOME", home)
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
	path := writeTestSnapshot(t, "hist.json", agent.Snapshot{
		Version:   1,
		Workspace: "/w",
		Model:     "m",
		Title:     "恢复的历史",
		SavedAt:   time.Now(),
		Messages:  restoreMessages(),
	})
	client := newTranscriptFixture(t, time.Millisecond)
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
	home := t.TempDir()
	t.Setenv("LYON_HOME", home)
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

	client := newTranscriptFixture(t, time.Millisecond)
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

// TestStoreArchiveOnExit: every store teardown path — explicit delete and
// shutdown — archives the conversation first, so nothing is lost from the
// snapshot store when a session leaves memory (Phase A data-loss fix).
func TestStoreArchiveOnExit(t *testing.T) {
	t.Setenv("LYON_HOME", t.TempDir())
	eng := agent.NewEngine(agent.DefaultConfig(), nil)
	ws := eng.Config().Workspace
	st := NewStore(0, 0)

	seeded := func(title, prompt string) *agent.Session {
		sess := eng.NewSession(nil)
		sess.Restore(agent.Snapshot{
			Version: 1, Workspace: ws, Title: title, SavedAt: time.Now(),
			// Distinct histories: ArchiveIfNew dedupes by message bytes
			// (storedElsewhere), so identical messages would collapse the
			// two cases into one archive entry.
			Messages: []anthropic.BetaMessageParam{
				anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(prompt)),
			},
		})
		return sess
	}

	// Path 1: Delete.
	s1 := seeded("delete-exits", "delete prompt")
	id1, err := st.Create(s1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !st.Delete(id1) {
		t.Fatal("Delete = false")
	}

	// Path 2: CloseAll (lyond shutdown).
	s2 := seeded("shutdown-exits", "shutdown prompt")
	if _, err := st.Create(s2); err != nil {
		t.Fatalf("Create #2: %v", err)
	}
	st.CloseAll()

	refs, err := agent.ListSnapshots(ws)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	titles := map[string]bool{}
	for _, r := range refs {
		titles[r.Title] = true
	}
	for _, want := range []string{"delete-exits", "shutdown-exits"} {
		if !titles[want] {
			t.Fatalf("archived titles = %v, missing %q", titles, want)
		}
	}
}
