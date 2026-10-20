package server

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	lyonv1 "lyon-agent/gen/go/lyon/v1"
)

// TestConnectRollbackSession: two prompts stack two checkpoints; rolling
// back to the newest rewinds to "after prompt one", the ring is
// re-synthesized to match (prompt two's echo is gone from from_seq=-1
// replay), and the returned checkpoint names the state restored.
func TestConnectRollbackSession(t *testing.T) {
	client := newTranscriptFixture(t, time.Millisecond)
	ctx := context.Background()

	cres, err := client.CreateSession(ctx, connect.NewRequest(&lyonv1.CreateSessionRequest{}))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sid := cres.Msg.Session.Id

	for _, prompt := range []string{"第一问", "第二问"} {
		if _, err := client.SubmitPrompt(ctx, connect.NewRequest(&lyonv1.SubmitPromptRequest{
			SessionId: sid, Prompt: prompt,
		})); err != nil {
			t.Fatalf("SubmitPrompt %q: %v", prompt, err)
		}
		collectPromptEvents(t, client, sid) // drain through prompt_end
	}

	// Every prompt captures its PRE-state, labeled with the prompt it
	// would undo: prompt two's checkpoint = "before 第二问" (= after 第一问).
	cres2, err := client.ListCheckpoints(ctx, connect.NewRequest(&lyonv1.ListCheckpointsRequest{SessionId: sid}))
	if err != nil {
		t.Fatalf("ListCheckpoints: %v", err)
	}
	cps := cres2.Msg.Checkpoints
	// The first prompt runs on empty history (nothing to undo to, no
	// capture); the second captures "after prompt one" — one checkpoint.
	if len(cps) != 1 {
		t.Fatalf("checkpoints = %d, want 1 (first prompt has no pre-state)", len(cps))
	}
	if cps[0].Kind != "prompt" || cps[0].Messages != 2 || cps[0].Label != "第二问" {
		t.Fatalf("checkpoint = %+v, want the before-第二问 capture (2 msgs)", cps[0])
	}

	rb, err := client.RollbackSession(ctx, connect.NewRequest(&lyonv1.RollbackSessionRequest{
		SessionId: sid, Id: 0, // 0 = newest
	}))
	if err != nil {
		t.Fatalf("RollbackSession: %v", err)
	}
	if rb.Msg.Checkpoint == nil || rb.Msg.Checkpoint.Id != cps[0].Id {
		t.Fatalf("rollback returned %+v, want checkpoint %d", rb.Msg.Checkpoint, cps[0].Id)
	}

	// The rewound ring replays prompt one's transcript only — the second
	// prompt's echo and its completion must be gone.
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
	var last int64
	for stream.Receive() {
		ev := stream.Msg().GetEvent()
		if ev == nil {
			continue
		}
		kinds = append(kinds, ev.TypeName)
		texts = append(texts, ev.Text)
		last = ev.Seq
		if len(kinds) == 2 { // user_message + one whole-text delta — the full synth batch of round one
			break
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(kinds) != 2 || kinds[0] != "user_message" || kinds[1] != "text_delta" {
		t.Fatalf("replay kinds = %v, want [user_message text_delta]", kinds)
	}
	if texts[0] != "第一问" {
		t.Fatalf("replay first text = %q, want 第一问 (the surviving prompt)", texts[0])
	}
	for _, tx := range texts {
		if tx == "第二问" {
			t.Fatal("rolled-back prompt still present in replay")
		}
	}
	// seq keeps ascending across the reset — the ring just holds new entries.
	if last <= 0 {
		t.Fatalf("last seq = %d", last)
	}
}

// TestConnectSaveCheckpoint: an explicit save pins a labeled manual undo
// point — FailedPrecondition while the history is empty, then kind
// "manual" with the caller's label once a prompt has run, listed
// newest-first among the automatic captures.
func TestConnectSaveCheckpoint(t *testing.T) {
	client := newTranscriptFixture(t, time.Millisecond)
	ctx := context.Background()
	cres, err := client.CreateSession(ctx, connect.NewRequest(&lyonv1.CreateSessionRequest{}))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sid := cres.Msg.Session.Id

	// Empty history: nothing to roll back to.
	_, err = client.SaveCheckpoint(ctx, connect.NewRequest(&lyonv1.SaveCheckpointRequest{
		SessionId: sid, Label: "too early",
	}))
	if got := connectCode(err); got != connect.CodeFailedPrecondition {
		t.Fatalf("save on empty history code = %v, want FailedPrecondition (err=%v)", got, err)
	}

	if _, err := client.SubmitPrompt(ctx, connect.NewRequest(&lyonv1.SubmitPromptRequest{
		SessionId: sid, Prompt: "hi",
	})); err != nil {
		t.Fatalf("SubmitPrompt: %v", err)
	}
	collectPromptEvents(t, client, sid)

	save, err := client.SaveCheckpoint(ctx, connect.NewRequest(&lyonv1.SaveCheckpointRequest{
		SessionId: sid, Label: "before risky stretch",
	}))
	if err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	cp := save.Msg.Checkpoint
	if cp == nil || cp.Kind != "manual" || cp.Label != "before risky stretch" || cp.Messages != 2 {
		t.Fatalf("saved checkpoint = %+v, want kind=manual, label %q, 2 msgs", cp, "before risky stretch")
	}

	lres, err := client.ListCheckpoints(ctx, connect.NewRequest(&lyonv1.ListCheckpointsRequest{SessionId: sid}))
	if err != nil {
		t.Fatalf("ListCheckpoints: %v", err)
	}
	if len(lres.Msg.Checkpoints) == 0 || lres.Msg.Checkpoints[0].Id != cp.Id {
		t.Fatalf("list = %+v, want the manual save as the newest entry", lres.Msg.Checkpoints)
	}
}

// TestRollbackUnknownCheckpoint: an id that never existed (or was
// discarded) fails NotFound, and an empty stack says so.
func TestRollbackUnknownCheckpoint(t *testing.T) {
	client := newTranscriptFixture(t, time.Millisecond)
	ctx := context.Background()
	cres, err := client.CreateSession(ctx, connect.NewRequest(&lyonv1.CreateSessionRequest{}))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sid := cres.Msg.Session.Id

	// No prompts → no checkpoints → NotFound with a hint.
	_, err = client.RollbackSession(ctx, connect.NewRequest(&lyonv1.RollbackSessionRequest{SessionId: sid, Id: 0}))
	if got := connectCode(err); got != connect.CodeNotFound {
		t.Fatalf("empty-stack code = %v, want NotFound (err=%v)", got, err)
	}

	if _, err := client.SubmitPrompt(ctx, connect.NewRequest(&lyonv1.SubmitPromptRequest{
		SessionId: sid, Prompt: "hi",
	})); err != nil {
		t.Fatalf("SubmitPrompt: %v", err)
	}
	collectPromptEvents(t, client, sid)

	_, err = client.RollbackSession(ctx, connect.NewRequest(&lyonv1.RollbackSessionRequest{SessionId: sid, Id: 424242}))
	if got := connectCode(err); got != connect.CodeNotFound {
		t.Fatalf("unknown-id code = %v, want NotFound (err=%v)", got, err)
	}
}
