package server

import (
	"context"
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

// newTranscriptFixture is newConnectFixture with a slower mock gateway so
// a second submit can observe the session still busy (Phase C's echo
// pre-check must reject without leaving a phantom prompt row).
func newTranscriptFixture(t *testing.T, latency time.Duration) lyonv1connect.SessionServiceClient {
	t.Helper()
	mock := mockapi.New(mockapi.Config{
		Script:  []mockapi.Turn{{Text: []string{"He", "llo"}}},
		Latency: latency,
	})
	t.Cleanup(mock.Close)
	t.Setenv("ANTHROPIC_BASE_URL", mock.URL())
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "transcript-test-token")

	eng := agent.NewEngine(agent.DefaultConfig(), nil)
	s := New(eng, NewStore(0, 0))
	s.SetObserver(tools.NewMetrics())
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return lyonv1connect.NewSessionServiceClient(http.DefaultClient, ts.URL)
}

// collectPromptEvents replays the ring and returns events through the
// prompt_end that closes the run.
func collectPromptEvents(t *testing.T, client lyonv1connect.SessionServiceClient, sessionID string) []*lyonv1.SessionEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := client.WatchEvents(ctx, connect.NewRequest(&lyonv1.WatchEventsRequest{
		SessionId: sessionID,
		FromSeq:   -1,
	}))
	if err != nil {
		t.Fatalf("WatchEvents: %v", err)
	}
	var evs []*lyonv1.SessionEvent
	for stream.Receive() {
		ev := stream.Msg().GetEvent()
		if ev == nil {
			continue
		}
		evs = append(evs, ev)
		if ev.TypeName == "prompt_end" {
			break
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("WatchEvents stream: %v", err)
	}
	if len(evs) == 0 {
		t.Fatal("no events before prompt_end")
	}
	return evs
}

// TestConnectPromptEcho: SubmitPrompt injects the prompt as a
// user_message hub event before the driver stamps anything — the replay
// must open with it (type UNSPECIFIED, type_name authoritative), seqs
// must run 1..n unbroken, and the mock's text deltas must follow.
func TestConnectPromptEcho(t *testing.T) {
	client := newTranscriptFixture(t, time.Millisecond)
	ctx := context.Background()

	cres, err := client.CreateSession(ctx, connect.NewRequest(&lyonv1.CreateSessionRequest{}))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sid := cres.Msg.Session.Id

	if _, err := client.SubmitPrompt(ctx, connect.NewRequest(&lyonv1.SubmitPromptRequest{
		SessionId: sid,
		Prompt:    "hello lyond",
	})); err != nil {
		t.Fatalf("SubmitPrompt: %v", err)
	}

	evs := collectPromptEvents(t, client, sid)
	first := evs[0]
	if first.TypeName != "user_message" || first.Text != "hello lyond" {
		t.Fatalf("first event = %s/%q, want user_message/%q", first.TypeName, first.Text, "hello lyond")
	}
	if first.Type != lyonv1.EventType_EVENT_TYPE_UNSPECIFIED {
		t.Fatalf("echo type = %v, want UNSPECIFIED (type_name is the channel)", first.Type)
	}
	var text strings.Builder
	for i, ev := range evs {
		if want := int64(i + 1); ev.Seq != want {
			t.Fatalf("evs[%d].Seq = %d, want %d (contiguous from 1)", i, ev.Seq, want)
		}
		if ev.TypeName == "text_delta" {
			text.WriteString(ev.Text)
		}
	}
	if text.String() != "Hello" {
		t.Fatalf("assembled text = %q, want %q", text.String(), "Hello")
	}
}

// TestConnectEchoRejectLeavesNoPhantom: a submit rejected while busy
// must not stamp a user_message — the pre-check fires before injection.
func TestConnectEchoRejectLeavesNoPhantom(t *testing.T) {
	client := newTranscriptFixture(t, 400*time.Millisecond)
	ctx := context.Background()

	cres, err := client.CreateSession(ctx, connect.NewRequest(&lyonv1.CreateSessionRequest{}))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sid := cres.Msg.Session.Id

	if _, err := client.SubmitPrompt(ctx, connect.NewRequest(&lyonv1.SubmitPromptRequest{
		SessionId: sid,
		Prompt:    "first",
	})); err != nil {
		t.Fatalf("SubmitPrompt #1: %v", err)
	}
	_, err = client.SubmitPrompt(ctx, connect.NewRequest(&lyonv1.SubmitPromptRequest{
		SessionId: sid,
		Prompt:    "second",
	}))
	if got := connectCode(err); got != connect.CodeFailedPrecondition {
		t.Fatalf("SubmitPrompt #2 while busy code = %v, want FailedPrecondition (err=%v)", got, err)
	}
	// Cut the slow mock run short so the drain below finishes fast; the
	// rejected submit is already proven by the code check.
	client.Interrupt(ctx, connect.NewRequest(&lyonv1.InterruptRequest{SessionId: sid}))

	echoes := 0
	for _, ev := range collectPromptEvents(t, client, sid) {
		if ev.TypeName == "user_message" {
			echoes++
		}
	}
	if echoes != 1 {
		t.Fatalf("user_message count = %d, want 1 (rejected submit must not inject)", echoes)
	}
}

// TestHubInjectSeqAndAttrs: synthetic events stamp through the same
// locked path as live ones (seq ascending = ring ascending), and attrs
// ride the forward-compat sink with invalid UTF-8 sanitized on the way in.
func TestHubInjectSeqAndAttrs(t *testing.T) {
	h := newHub(nil) // no run(): inject is self-contained
	h.inject(agent.Event{Type: evUserMessage, Text: "echo"})
	h.inject(agent.Event{
		Type:   agent.EventMemoryInjected,
		Detail: "2 bytes of hot memory",
		Attrs:  map[string]string{"memory": "hi \xff"},
	})

	sub, replay, err := h.subscribe(-1)
	if err != nil {
		t.Fatalf("subscribe(-1): %v", err)
	}
	defer h.unsubscribe(sub)
	if len(replay) != 2 {
		t.Fatalf("replay len = %d, want 2", len(replay))
	}
	if replay[0].seq != 1 || replay[1].seq != 2 {
		t.Fatalf("seqs = %d,%d, want 1,2", replay[0].seq, replay[1].seq)
	}
	if replay[0].ev.Type != evUserMessage || replay[0].ev.Text != "echo" {
		t.Fatalf("first injected event = %s/%q", replay[0].ev.Type, replay[0].ev.Text)
	}
	if got := replay[1].ev.Attrs["memory"]; got != "hi �" {
		t.Fatalf("attrs[memory] = %q, want sanitized %q", got, "hi �")
	}
}
