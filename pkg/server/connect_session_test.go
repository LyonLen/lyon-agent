package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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

// newConnectFixture builds a live lyond-shaped server against the mock
// gateway and returns a connected SessionService client.
func newConnectFixture(t *testing.T) lyonv1connect.SessionServiceClient {
	t.Helper()
	mock := mockapi.New(mockapi.Config{
		Script:  []mockapi.Turn{{Text: []string{"He", "llo"}}},
		Latency: time.Millisecond,
	})
	t.Cleanup(mock.Close)
	t.Setenv("ANTHROPIC_BASE_URL", mock.URL())
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "connect-test-token")

	eng := agent.NewEngine(agent.DefaultConfig(), nil)
	s := New(eng, NewStore(0, 0))
	s.SetObserver(tools.NewMetrics())
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return lyonv1connect.NewSessionServiceClient(http.DefaultClient, ts.URL)
}

func connectCode(err error) connect.Code {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Code()
	}
	return connect.CodeUnknown
}

// TestConnectSessionLifecycle drives the full Phase 1 surface over the
// wire: create → watch → enqueue-rejection → submit → prompt_end event
// stream (contiguous seq, enum + type_name agreement) → stats → mode →
// list → delete → NotFound → watcher EOF.
func TestConnectSessionLifecycle(t *testing.T) {
	client := newConnectFixture(t)
	ctx := context.Background()

	cres, err := client.CreateSession(ctx, connect.NewRequest(&lyonv1.CreateSessionRequest{}))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sess := cres.Msg.Session
	if sess.Id == "" || sess.Busy || sess.Mode != "default" || sess.CreatedAt == nil {
		t.Fatalf("fresh session = %+v", sess)
	}

	// Watch BEFORE submit (from_seq=-1: replay everything, currently
	// nothing) — must then see the whole prompt live, message_start first.
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	watchDone := make(chan error, 1)
	events := make(chan *lyonv1.SessionEvent, 256)
	go func() {
		stream, err := client.WatchEvents(wctx, connect.NewRequest(&lyonv1.WatchEventsRequest{
			SessionId: sess.Id,
			FromSeq:   -1,
		}))
		if err != nil {
			watchDone <- err
			return
		}
		for stream.Receive() {
			events <- stream.Msg().Event
		}
		watchDone <- stream.Err()
	}()

	// Reconnect semantics: from_seq beyond the ring fails with
	// FailedPrecondition (client re-anchors with -1 and retries).
	bad, err := client.WatchEvents(ctx, connect.NewRequest(&lyonv1.WatchEventsRequest{
		SessionId: sess.Id,
		FromSeq:   99,
	}))
	if err == nil {
		for bad.Receive() {
		}
		err = bad.Err()
	}
	if got := connectCode(err); got != connect.CodeFailedPrecondition {
		t.Fatalf("WatchEvents(from_seq=99) code = %v, want FailedPrecondition (err=%v)", got, err)
	}

	// Enqueue on an idle session → FailedPrecondition ("use Submit").
	_, err = client.EnqueuePrompt(ctx, connect.NewRequest(&lyonv1.EnqueuePromptRequest{
		SessionId: sess.Id, Prompt: "parked",
	}))
	if got := connectCode(err); got != connect.CodeFailedPrecondition {
		t.Fatalf("EnqueuePrompt idle code = %v, want FailedPrecondition (err=%v)", got, err)
	}

	sres, err := client.SubmitPrompt(ctx, connect.NewRequest(&lyonv1.SubmitPromptRequest{
		SessionId: sess.Id, Prompt: "connect lifecycle: say hello.",
	}))
	if err != nil {
		t.Fatalf("SubmitPrompt: %v", err)
	}
	if !sres.Msg.Started {
		t.Fatal("SubmitPrompt.started = false")
	}

	// Collect until prompt_end: contiguous seq from 1, contract-first types.
	var got []*lyonv1.SessionEvent
	deadline := time.After(30 * time.Second)
loop:
	for {
		select {
		case ev := <-events:
			got = append(got, ev)
			if ev.Type == lyonv1.EventType_EVENT_TYPE_PROMPT_END {
				break loop
			}
		case <-deadline:
			t.Fatalf("timed out waiting for prompt_end; got %d events", len(got))
		}
	}
	if len(got) < 2 {
		t.Fatalf("only %d events, want a full prompt stream", len(got))
	}
	// Seq starts at 1 and is contiguous; the stream carries message_start
	// (memory_injected may legitimately precede it — the hot memory block
	// rides into the first message) and ends at prompt_end, which the loop
	// break above already pinned.
	if got[0].Seq != 1 {
		t.Fatalf("first event seq = %d, want 1", got[0].Seq)
	}
	foundStart := false
	for i, ev := range got {
		if ev.Type == lyonv1.EventType_EVENT_TYPE_MESSAGE_START {
			foundStart = true
			if ev.TypeName != string(agent.EventMessageStart) {
				t.Fatalf("message_start type_name = %q", ev.TypeName)
			}
		}
		if ev.Type == lyonv1.EventType_EVENT_TYPE_UNSPECIFIED && ev.TypeName == "" {
			t.Fatalf("event %d has neither enum nor type_name", i)
		}
		if i > 0 && ev.Seq != got[i-1].Seq+1 {
			t.Fatalf("seq discontinuity at %d: %d -> %d", i, got[i-1].Seq, ev.Seq)
		}
	}
	if !foundStart {
		t.Fatalf("no message_start in stream of %d events", len(got))
	}
	if last := got[len(got)-1]; last.Type != lyonv1.EventType_EVENT_TYPE_PROMPT_END || last.Seq != int64(len(got)) {
		t.Fatalf("last event = seq %d type %s, want seq %d prompt_end", last.Seq, last.Type, len(got))
	}

	// Stats: the turn landed with usage accounted.
	st, err := client.GetStats(ctx, connect.NewRequest(&lyonv1.GetStatsRequest{SessionId: sess.Id}))
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	// First turn on a cold cache bills input as cache_creation on the mock
	// gateway — assert token accounting, not a specific bucket.
	if len(st.Msg.Turns) != 1 || st.Msg.Turns[0].Usage == nil ||
		st.Msg.Turns[0].Usage.Input+st.Msg.Turns[0].Usage.CacheRead+st.Msg.Turns[0].Usage.CacheCreation <= 0 {
		t.Fatalf("GetStats = %+v, want one turn with accounted tokens", st.Msg.Turns)
	}

	// Mode round-trip; unknown mode → InvalidArgument, state unchanged.
	mres, err := client.SetMode(ctx, connect.NewRequest(&lyonv1.SetModeRequest{SessionId: sess.Id, Mode: "plan"}))
	if err != nil || mres.Msg.Mode != "plan" {
		t.Fatalf("SetMode(plan) = %v err=%v", mres, err)
	}
	_, err = client.SetMode(ctx, connect.NewRequest(&lyonv1.SetModeRequest{SessionId: sess.Id, Mode: "nope"}))
	if got := connectCode(err); got != connect.CodeInvalidArgument {
		t.Fatalf("SetMode(nope) code = %v, want InvalidArgument", got)
	}
	gs, err := client.GetSession(ctx, connect.NewRequest(&lyonv1.GetSessionRequest{SessionId: sess.Id}))
	if err != nil || gs.Msg.Session.Mode != "plan" || gs.Msg.Session.Busy || gs.Msg.Session.Turns != 1 {
		t.Fatalf("GetSession after modes = %+v err=%v", gs.Msg.Session, err)
	}

	// List sees exactly this one.
	lres, err := client.ListSessions(ctx, connect.NewRequest(&lyonv1.ListSessionsRequest{}))
	if err != nil || len(lres.Msg.Sessions) != 1 || lres.Msg.Sessions[0].Id != sess.Id {
		t.Fatalf("ListSessions = %+v err=%v", lres.Msg.Sessions, err)
	}

	// Delete closes the session; the watcher must see a clean EOF and
	// GetSession flips to NotFound.
	if _, err := client.DeleteSession(ctx, connect.NewRequest(&lyonv1.DeleteSessionRequest{SessionId: sess.Id})); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	_, err = client.GetSession(ctx, connect.NewRequest(&lyonv1.GetSessionRequest{SessionId: sess.Id}))
	if got := connectCode(err); got != connect.CodeNotFound {
		t.Fatalf("GetSession after delete code = %v, want NotFound", got)
	}
	select {
	case err := <-watchDone:
		if err != nil {
			t.Fatalf("watch stream ended with error (want clean EOF): %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch stream did not end after session close")
	}
}

// TestConnectAuthRequired pins the token wrapper over the Connect routes:
// no token → 401; with token → the RPC layer answers (bad body is fine,
// it proves auth passed).
func TestConnectAuthRequired(t *testing.T) {
	eng := agent.NewEngine(agent.DefaultConfig(), nil)
	s := New(eng, NewStore(0, 0))
	s.SetToken("sekrit")
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+lyonv1connect.SessionServiceCreateSessionProcedure,
		"application/connect+proto", strings.NewReader(""))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodPost, ts.URL+lyonv1connect.SessionServiceCreateSessionProcedure,
		strings.NewReader(""))
	req.Header.Set("Content-Type", "application/connect+proto")
	req.Header.Set("Authorization", "Bearer sekrit")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("authed post: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode == http.StatusUnauthorized {
		t.Fatal("valid token still rejected with 401")
	}
}
