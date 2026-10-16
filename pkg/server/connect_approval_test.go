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

// newApprovalFixture builds a lyond with -approve semantics: the engine
// cfg carries the bridge's ApproveFunc and SetApproval wires the store, so
// a NeedsApproval tool parks until the ApprovalService client decides.
func newApprovalFixture(t *testing.T, script []mockapi.Turn) (lyonv1connect.SessionServiceClient, lyonv1connect.ApprovalServiceClient) {
	t.Helper()
	mock := mockapi.New(mockapi.Config{Script: script, Latency: time.Millisecond})
	t.Cleanup(mock.Close)
	t.Setenv("ANTHROPIC_BASE_URL", mock.URL())
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "approval-test-token")

	appr := NewApprovalBridge()
	cfg := agent.DefaultConfig()
	cfg.Approve = appr.ApproveFunc()
	eng := agent.NewEngine(cfg, nil)
	s := New(eng, NewStore(0, 0))
	s.SetApproval(appr)
	s.SetObserver(tools.NewMetrics())
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return lyonv1connect.NewSessionServiceClient(http.DefaultClient, ts.URL),
		lyonv1connect.NewApprovalServiceClient(http.DefaultClient, ts.URL)
}

// startApprovalWatch opens WatchPending in the background and splits the
// stream: arrivals carry a tool name, resolves are the empty-tool_name
// removal signal. Either channel failing (stream error) is surfaced as a
// closed channel.
func startApprovalWatch(t *testing.T, client lyonv1connect.ApprovalServiceClient) (arrivals, resolves <-chan *lyonv1.WatchPendingResponse) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stream, err := client.WatchPending(ctx, connect.NewRequest(&lyonv1.WatchPendingRequest{}))
	if err != nil {
		t.Fatalf("WatchPending: %v", err)
	}
	a := make(chan *lyonv1.WatchPendingResponse, 16)
	r := make(chan *lyonv1.WatchPendingResponse, 16)
	go func() {
		defer close(a)
		defer close(r)
		for stream.Receive() {
			ev := stream.Msg()
			switch {
			case ev.ToolName == "" && ev.ExecId == 0:
				// Header-flush no-op (stream open marker) — not a resolve.
			case ev.ToolName == "":
				r <- ev
			default:
				a <- ev
			}
		}
		// Err after a clean cancel/close is expected; a real stream
		// failure closes the channels and the tests' receives time out.
	}()
	return a, r
}

func waitPending(t *testing.T, ch <-chan *lyonv1.WatchPendingResponse, what string) *lyonv1.WatchPendingResponse {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatalf("approval stream ended while waiting for %s", what)
		}
		return ev
	case <-time.After(15 * time.Second):
		t.Fatalf("timeout waiting for %s", what)
		return nil
	}
}

// TestConnectApprovalFlow: a scripted run_shell parks on the bridge, the
// watcher sees it with seat+input, Decide(approve) unblocks the tool, the
// second API turn lands, and the transcript closes with a successful
// exec — the whole Phase B loop over the wire.
func TestConnectApprovalFlow(t *testing.T) {
	client, apprClient := newApprovalFixture(t, []mockapi.Turn{
		{Tool: &mockapi.ToolCall{Name: "run_shell", Input: `{"command":"echo approve-me"}`}},
		{Text: []string{"all done"}},
	})
	ctx := context.Background()
	cres, err := client.CreateSession(ctx, connect.NewRequest(&lyonv1.CreateSessionRequest{}))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sid := cres.Msg.Session.Id
	arrivals, resolves := startApprovalWatch(t, apprClient)

	if _, err := client.SubmitPrompt(ctx, connect.NewRequest(&lyonv1.SubmitPromptRequest{
		SessionId: sid, Prompt: "please run it",
	})); err != nil {
		t.Fatalf("SubmitPrompt: %v", err)
	}

	pend := waitPending(t, arrivals, "parked run_shell")
	if pend.SessionId != sid || pend.ToolName != "run_shell" {
		t.Fatalf("pending = seat %q tool %q, want seat %q run_shell", pend.SessionId, pend.ToolName, sid)
	}
	if !strings.Contains(pend.InputJson, "approve-me") {
		t.Fatalf("pending input = %q, want the scripted command", pend.InputJson)
	}
	if pend.RequestedAtMs == 0 || pend.ExecId == 0 {
		t.Fatalf("pending key fields empty: exec=%d at=%d", pend.ExecId, pend.RequestedAtMs)
	}

	if _, err := apprClient.Decide(ctx, connect.NewRequest(&lyonv1.DecideRequest{
		SessionId: pend.SessionId, ExecId: pend.ExecId, Approve: true,
	})); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	// The watchers must see the card go (empty tool_name = resolve).
	res := waitPending(t, resolves, "resolve after approve")
	if res.SessionId != sid || res.ExecId != pend.ExecId {
		t.Fatalf("resolve = %+v for exec %d", res, pend.ExecId)
	}
	// Double-decide is gone: the key left the park table with the answer.
	_, err = apprClient.Decide(ctx, connect.NewRequest(&lyonv1.DecideRequest{
		SessionId: pend.SessionId, ExecId: pend.ExecId, Approve: true,
	}))
	if got := connectCode(err); got != connect.CodeNotFound {
		t.Fatalf("second Decide code = %v, want NotFound (err=%v)", got, err)
	}

	evs := collectPromptEvents(t, client, sid)
	var sawExec bool
	var text string
	for _, ev := range evs {
		switch ev.TypeName {
		case "tool_exec_end":
			if ev.ExecId != pend.ExecId {
				t.Fatalf("exec_end for unknown exec %d", ev.ExecId)
			}
			if ev.IsError {
				t.Fatalf("approved exec failed: %s", ev.Error)
			}
			sawExec = true
		case "text_delta":
			text += ev.Text
		}
	}
	if !sawExec {
		t.Fatal("no tool_exec_end in the transcript")
	}
	if !strings.Contains(text, "all done") {
		t.Fatalf("second-turn text = %q", text)
	}
}

// TestConnectApprovalDeny: Decide(approve=false) surfaces to the model as
// an error tool result ("user declined"), and no remember cookie is set —
// the next ask parks again.
func TestConnectApprovalDeny(t *testing.T) {
	client, apprClient := newApprovalFixture(t, []mockapi.Turn{
		{Tool: &mockapi.ToolCall{Name: "run_shell", Input: `{"command":"echo nope"}`}},
		{Text: []string{"understood"}},
	})
	ctx := context.Background()
	cres, err := client.CreateSession(ctx, connect.NewRequest(&lyonv1.CreateSessionRequest{}))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sid := cres.Msg.Session.Id
	arrivals, _ := startApprovalWatch(t, apprClient)

	if _, err := client.SubmitPrompt(ctx, connect.NewRequest(&lyonv1.SubmitPromptRequest{
		SessionId: sid, Prompt: "run it",
	})); err != nil {
		t.Fatalf("SubmitPrompt: %v", err)
	}
	pend := waitPending(t, arrivals, "parked run_shell")
	if _, err := apprClient.Decide(ctx, connect.NewRequest(&lyonv1.DecideRequest{
		SessionId: pend.SessionId, ExecId: pend.ExecId, Approve: false,
	})); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	evs := collectPromptEvents(t, client, sid)
	var denied bool
	for _, ev := range evs {
		if ev.TypeName == "tool_exec_end" && ev.IsError && strings.Contains(ev.Error, "declined") {
			denied = true
		}
	}
	if !denied {
		t.Fatal("denial did not reach the transcript as a declined exec")
	}
}

// TestApprovalRememberVouches: Decide(approve, remember) vouches for the
// tool on this seat only — the second scripted call runs without a new
// arrival (TUI allowAll semantics; other tools are unaffected).
func TestApprovalRememberVouches(t *testing.T) {
	client, apprClient := newApprovalFixture(t, []mockapi.Turn{
		{Tool: &mockapi.ToolCall{Name: "run_shell", Input: `{"command":"echo one"}`}},
		{Tool: &mockapi.ToolCall{Name: "run_shell", Input: `{"command":"echo two"}`}},
		{Text: []string{"done"}},
	})
	ctx := context.Background()
	cres, err := client.CreateSession(ctx, connect.NewRequest(&lyonv1.CreateSessionRequest{}))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sid := cres.Msg.Session.Id
	arrivals, _ := startApprovalWatch(t, apprClient)

	if _, err := client.SubmitPrompt(ctx, connect.NewRequest(&lyonv1.SubmitPromptRequest{
		SessionId: sid, Prompt: "twice",
	})); err != nil {
		t.Fatalf("SubmitPrompt: %v", err)
	}
	pend := waitPending(t, arrivals, "first run_shell")
	if _, err := apprClient.Decide(ctx, connect.NewRequest(&lyonv1.DecideRequest{
		SessionId: pend.SessionId, ExecId: pend.ExecId, Approve: true, Remember: true,
	})); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	evs := collectPromptEvents(t, client, sid)
	var text string
	var execs int
	for _, ev := range evs {
		if ev.TypeName == "tool_exec_end" {
			if ev.IsError {
				t.Fatalf("exec %d failed: %s", ev.ExecId, ev.Error)
			}
			execs++
		}
		text += ev.Text
	}
	if execs != 2 {
		t.Fatalf("execs = %d, want 2 (second must run vouched)", execs)
	}
	if !strings.Contains(text, "done") {
		t.Fatalf("final text = %q", text)
	}
	// Only the first call ever reached a watcher.
	deadline := time.After(300 * time.Millisecond)
	late := false
	for !late {
		select {
		case ev, ok := <-arrivals:
			if !ok {
				late = true
			} else if ev.ExecId != pend.ExecId {
				t.Fatalf("vouched tool parked again: exec %d", ev.ExecId)
			}
		case <-deadline:
			late = true
		}
	}
}

// TestApprovalFailClosedOnInterrupt: interrupting a parked approval cancels
// the tool context — the bridge denies, resolves the card, and the seat
// becomes undecidable (NotFound).
func TestApprovalFailClosedOnInterrupt(t *testing.T) {
	client, apprClient := newApprovalFixture(t, []mockapi.Turn{
		{Tool: &mockapi.ToolCall{Name: "run_shell", Input: `{"command":"echo park"}`}},
		{Text: []string{"never"}},
	})
	ctx := context.Background()
	cres, err := client.CreateSession(ctx, connect.NewRequest(&lyonv1.CreateSessionRequest{}))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sid := cres.Msg.Session.Id
	arrivals, resolves := startApprovalWatch(t, apprClient)

	if _, err := client.SubmitPrompt(ctx, connect.NewRequest(&lyonv1.SubmitPromptRequest{
		SessionId: sid, Prompt: "park forever",
	})); err != nil {
		t.Fatalf("SubmitPrompt: %v", err)
	}
	pend := waitPending(t, arrivals, "parked run_shell")

	if _, err := client.Interrupt(ctx, connect.NewRequest(&lyonv1.InterruptRequest{
		SessionId: sid,
	})); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	// Cancellation path resolves the card exactly like a decision does.
	res := waitPending(t, resolves, "resolve after interrupt")
	if res.ExecId != pend.ExecId {
		t.Fatalf("resolve exec %d, want %d", res.ExecId, pend.ExecId)
	}
	_, err = apprClient.Decide(ctx, connect.NewRequest(&lyonv1.DecideRequest{
		SessionId: pend.SessionId, ExecId: pend.ExecId, Approve: true,
	}))
	if got := connectCode(err); got != connect.CodeNotFound {
		t.Fatalf("late Decide code = %v, want NotFound (err=%v)", got, err)
	}
	// The run must have ended: its closing event is in the ring.
	for _, ev := range collectPromptEvents(t, client, sid) {
		if ev.TypeName == "prompt_end" {
			return
		}
	}
	t.Fatal("no prompt_end after interrupt")
}

// TestApprovalConcurrentParks: unlike TUI's single overlay slot (apprMu),
// two seats asking at once both surface — a human answering one card must
// not stall the other session (concurrency is a product requirement).
func TestApprovalConcurrentParks(t *testing.T) {
	// The script feeds API calls in arrival order across BOTH sessions:
	// the first two calls must each emit a tool_use (so both seats park),
	// then every later call answers with plain text until prompt_end.
	client, apprClient := newApprovalFixture(t, []mockapi.Turn{
		{Tool: &mockapi.ToolCall{Name: "run_shell", Input: `{"command":"echo first"}`}},
		{Tool: &mockapi.ToolCall{Name: "run_shell", Input: `{"command":"echo second"}`}},
		{Text: []string{"one done"}},
	})
	ctx := context.Background()
	arrivals, _ := startApprovalWatch(t, apprClient)

	mk := func() string {
		t.Helper()
		res, err := client.CreateSession(ctx, connect.NewRequest(&lyonv1.CreateSessionRequest{}))
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		sid := res.Msg.Session.Id
		if _, err := client.SubmitPrompt(ctx, connect.NewRequest(&lyonv1.SubmitPromptRequest{
			SessionId: sid, Prompt: "go",
		})); err != nil {
			t.Fatalf("SubmitPrompt: %v", err)
		}
		return sid
	}
	s1, s2 := mk(), mk()

	// Both parked: two arrivals with distinct seats, in any order.
	first := waitPending(t, arrivals, "first park")
	second := waitPending(t, arrivals, "second park")
	if first.SessionId == second.SessionId {
		t.Fatalf("both parks on seat %q — the second session never surfaced", first.SessionId)
	}
	bySid := map[string]*lyonv1.WatchPendingResponse{first.SessionId: first, second.SessionId: second}

	// Answer out of arrival order: the second seat decides first, proving
	// neither park holds the other hostage.
	for _, sid := range []string{second.SessionId, first.SessionId} {
		pend := bySid[sid]
		if _, err := apprClient.Decide(ctx, connect.NewRequest(&lyonv1.DecideRequest{
			SessionId: pend.SessionId, ExecId: pend.ExecId, Approve: true,
		})); err != nil {
			t.Fatalf("Decide seat %s: %v", sid, err)
		}
	}
	for _, sid := range []string{s1, s2} {
		for _, ev := range collectPromptEvents(t, client, sid) {
			if ev.TypeName == "prompt_end" {
				break
			}
		}
	}
}
