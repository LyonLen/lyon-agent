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

// newApprovalFixture builds a lyond with -approve semantics: the engine
// cfg carries the bridge's ApproveFunc and SetApproval wires the store, so
// a NeedsApproval tool parks until the ApprovalService client decides.
func newApprovalFixture(t *testing.T, script []mockapi.Turn) (lyonv1connect.SessionServiceClient, lyonv1connect.ApprovalServiceClient) {
	t.Helper()
	// Always pin a private LYON_HOME, not just an empty one: TestMain
	// already sets a package-wide scratch home, so env-sniffing would
	// leave every fixture sharing one ledger — Decide.remember from an
	// earlier test would auto-approve later tests' tools before they
	// ever park. Tests that need to observe the home read it back from
	// the env after this call.
	scratchHome(t)
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
	// Close the store's sessions BEFORE the TempDir cleanup runs (LIFO): a
	// session's Close waits out its pending run-dump writes, and without
	// that the async dump writer races RemoveAll of the scratch home —
	// stable "directory is not empty" failures on a slow disk.
	t.Cleanup(s.store.CloseAll)
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

// TestApprovalRememberPersists pins the proto Decide.remember contract:
// a remembered vouch lands in $LYON_HOME/approvals.json for the seat it
// was decided on, a fresh bridge over the same home honors that seat (the
// restart case) and only that seat, and a corrupt ledger degrades to empty
// instead of blocking construction. policy.json — the operator's security
// gate — must stay untouched.
func TestApprovalRememberPersists(t *testing.T) {
	client, apprClient := newApprovalFixture(t, []mockapi.Turn{
		{Tool: &mockapi.ToolCall{Name: "run_shell", Input: `{"command":"echo one"}`}},
		{Text: []string{"done"}},
	})
	home := os.Getenv("LYON_HOME") // the fixture pinned a scratch home for us
	// The regression this pins: the OLD bridge wrote its vouches straight
	// into policy.json as {"run_shell":true}, which made run_shell skip the
	// card forever — in every seat, across restarts. Seed exactly that
	// leftover (plus a real operator gate beside it): the tool must still
	// park, and the file must come out byte-identical.
	gate := []byte(`{"allow_tools":["run_shell"],"run_shell":true}`)
	if err := os.WriteFile(filepath.Join(home, "policy.json"), gate, 0o644); err != nil {
		t.Fatalf("seed policy.json: %v", err)
	}
	ctx := context.Background()
	cres, err := client.CreateSession(ctx, connect.NewRequest(&lyonv1.CreateSessionRequest{}))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sid := cres.Msg.Session.Id
	arrivals, _ := startApprovalWatch(t, apprClient)
	if _, err := client.SubmitPrompt(ctx, connect.NewRequest(&lyonv1.SubmitPromptRequest{
		SessionId: sid, Prompt: "once",
	})); err != nil {
		t.Fatalf("SubmitPrompt: %v", err)
	}
	pend := waitPending(t, arrivals, "first run_shell")
	if _, err := apprClient.Decide(ctx, connect.NewRequest(&lyonv1.DecideRequest{
		SessionId: pend.SessionId, ExecId: pend.ExecId, Approve: true, Remember: true,
	})); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	collectPromptEvents(t, client, sid)

	// The vouch hit its own ledger, scoped to the seat.
	raw, err := os.ReadFile(filepath.Join(home, "approvals.json"))
	if err != nil {
		t.Fatalf("approvals.json not written: %v", err)
	}
	if !strings.Contains(string(raw), sid) || !strings.Contains(string(raw), "run_shell") {
		t.Fatalf("approvals.json = %s, want the seat+run_shell vouch", raw)
	}
	got, err := os.ReadFile(filepath.Join(home, "policy.json"))
	if err != nil {
		t.Fatalf("policy.json disappeared: %v", err)
	}
	if string(got) != string(gate) {
		t.Fatalf("policy.json was rewritten: %s", got)
	}

	// A fresh bridge (the restart case) honors the ledger for THAT seat.
	b2 := NewApprovalBridge()
	if !b2.remembered(sid, "run_shell") {
		t.Fatal("fresh bridge did not honor the persisted run_shell vouch")
	}
	// A vouch is a decision about one run: it must not leak to another
	// session, nor cover a different tool.
	if b2.remembered("some-other-session", "run_shell") {
		t.Fatal("vouch leaked to another seat")
	}
	if b2.remembered(sid, "write_file") {
		t.Fatal("fresh bridge vouched an unrelated tool")
	}

	// Corrupt ledger: construction still succeeds, ledger empty, and the
	// next grant overwrites the damage.
	if err := os.WriteFile(filepath.Join(home, "approvals.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("corrupt the ledger: %v", err)
	}
	b3 := NewApprovalBridge()
	if b3.remembered(sid, "run_shell") {
		t.Fatal("corrupt ledger silently granted a tool")
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

// newModeBlockFixture builds a lyond whose plan gate parks on the bridge
// (cfg.ModeBlock) while approval stays unwired — the mode card in
// isolation, so a vouched write does not then park a second approval card.
func newModeBlockFixture(t *testing.T, script []mockapi.Turn) (lyonv1connect.SessionServiceClient, lyonv1connect.ApprovalServiceClient) {
	t.Helper()
	scratchHome(t)
	mock := mockapi.New(mockapi.Config{Script: script, Latency: time.Millisecond})
	t.Cleanup(mock.Close)
	t.Setenv("ANTHROPIC_BASE_URL", mock.URL())
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "mode-block-test-token")

	appr := NewApprovalBridge()
	cfg := agent.DefaultConfig()
	cfg.Workspace = t.TempDir() // write_file lands here; the vouch keys off it
	cfg.ModeBlock = appr.ModeBlockFunc()
	eng := agent.NewEngine(cfg, nil)
	s := New(eng, NewStore(0, 0))
	s.SetApproval(appr)
	s.SetObserver(tools.NewMetrics())
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(s.store.CloseAll)
	return lyonv1connect.NewSessionServiceClient(http.DefaultClient, ts.URL),
		lyonv1connect.NewApprovalServiceClient(http.DefaultClient, ts.URL)
}

// TestConnectModeBlockAllowPath: a plan-mode write_file parks with
// mode_block set (not a NeedsApproval ask), Decide(ALLOW_PATH) vouches
// that file for the session — the write executes without ever reaching
// an approval card — and remember is ignored (policy answer, not consent:
// no approvals.json entry).
func TestConnectModeBlockAllowPath(t *testing.T) {
	client, apprClient := newModeBlockFixture(t, []mockapi.Turn{
		{Tool: &mockapi.ToolCall{Name: "write_file", Input: `{"path":"notes.md","content":"vouched"}`}},
		{Text: []string{"wrote it"}},
	})
	ctx := context.Background()
	cres, err := client.CreateSession(ctx, connect.NewRequest(&lyonv1.CreateSessionRequest{
		Mode: "plan",
	}))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sid := cres.Msg.Session.Id
	arrivals, resolves := startApprovalWatch(t, apprClient)

	if _, err := client.SubmitPrompt(ctx, connect.NewRequest(&lyonv1.SubmitPromptRequest{
		SessionId: sid, Prompt: "take a note",
	})); err != nil {
		t.Fatalf("SubmitPrompt: %v", err)
	}

	pend := waitPending(t, arrivals, "parked write_file")
	if !pend.ModeBlock {
		t.Fatalf("pending = %+v, want mode_block=true", pend)
	}
	if pend.ToolName != "write_file" || !strings.Contains(pend.InputJson, "notes.md") {
		t.Fatalf("pending tool/input = %q %q", pend.ToolName, pend.InputJson)
	}

	if _, err := apprClient.Decide(ctx, connect.NewRequest(&lyonv1.DecideRequest{
		SessionId: pend.SessionId, ExecId: pend.ExecId, Approve: true, Remember: true,
		Action: lyonv1.ModeAction_MODE_ACTION_ALLOW_PATH,
	})); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	res := waitPending(t, resolves, "resolve after allow-path")
	if res.ExecId != pend.ExecId {
		t.Fatalf("resolve exec = %d, want %d", res.ExecId, pend.ExecId)
	}

	evs := collectPromptEvents(t, client, sid)
	var sawExec, sawText bool
	for _, ev := range evs {
		switch ev.TypeName {
		case "tool_exec_end":
			if ev.ExecId != pend.ExecId {
				continue
			}
			if ev.IsError {
				t.Fatalf("vouched write failed: %s", ev.Error)
			}
			sawExec = true
		case "text_delta":
			sawText = sawText || strings.Contains(ev.Text, "wrote it")
		}
	}
	if !sawExec {
		t.Fatal("no successful tool_exec_end after allow-path")
	}
	if !sawText {
		t.Fatal("second turn never landed")
	}

	// remember must not write the consent ledger: a vouch-by-file is not
	// an approvals.json tool grant.
	if raw, err := os.ReadFile(filepath.Join(os.Getenv("LYON_HOME"), "approvals.json")); err == nil {
		t.Fatalf("mode decide wrote approvals.json: %s", raw)
	}
}

// TestConnectModeBlockDeny: Decide(DENY) reproduces the plan hard block
// as the tool result — the model sees "blocked by plan mode" and the run
// continues to its next turn.
func TestConnectModeBlockDeny(t *testing.T) {
	client, apprClient := newModeBlockFixture(t, []mockapi.Turn{
		{Tool: &mockapi.ToolCall{Name: "write_file", Input: `{"path":"nope.md","content":"x"}`}},
		{Text: []string{"understood"}},
	})
	ctx := context.Background()
	cres, err := client.CreateSession(ctx, connect.NewRequest(&lyonv1.CreateSessionRequest{
		Mode: "plan",
	}))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sid := cres.Msg.Session.Id
	arrivals, _ := startApprovalWatch(t, apprClient)

	if _, err := client.SubmitPrompt(ctx, connect.NewRequest(&lyonv1.SubmitPromptRequest{
		SessionId: sid, Prompt: "write it",
	})); err != nil {
		t.Fatalf("SubmitPrompt: %v", err)
	}
	pend := waitPending(t, arrivals, "parked write_file")
	if !pend.ModeBlock {
		t.Fatalf("pending = %+v, want mode_block=true", pend)
	}
	if _, err := apprClient.Decide(ctx, connect.NewRequest(&lyonv1.DecideRequest{
		SessionId: pend.SessionId, ExecId: pend.ExecId,
		Action: lyonv1.ModeAction_MODE_ACTION_DENY,
	})); err != nil {
		t.Fatalf("Decide: %v", err)
	}

	var blocked bool
	for _, ev := range collectPromptEvents(t, client, sid) {
		if ev.TypeName == "tool_exec_end" && ev.ExecId == pend.ExecId && ev.IsError &&
			strings.Contains(ev.Error, "plan mode") {
			blocked = true
		}
	}
	if !blocked {
		t.Fatal("deny did not surface as the plan-mode block")
	}
}

// TestModeDecisionMapping pins the Decide.action wire contract,
// including old-client compat (UNSPECIFIED + approve → default, deny →
// deny) — approval parks ignore action entirely (see decide).
func TestModeDecisionMapping(t *testing.T) {
	cases := []struct {
		name    string
		action  lyonv1.ModeAction
		approve bool
		want    agent.ModeBlockDecision
	}{
		{"allow_path", lyonv1.ModeAction_MODE_ACTION_ALLOW_PATH, false, agent.ModeBlockAllowPath},
		{"default", lyonv1.ModeAction_MODE_ACTION_MODE_DEFAULT, false, agent.ModeBlockDefault},
		{"bypass", lyonv1.ModeAction_MODE_ACTION_MODE_BYPASS, false, agent.ModeBlockBypass},
		{"deny", lyonv1.ModeAction_MODE_ACTION_DENY, true, agent.ModeBlockDeny},
		{"compat approve", lyonv1.ModeAction_MODE_ACTION_UNSPECIFIED, true, agent.ModeBlockDefault},
		{"compat deny", lyonv1.ModeAction_MODE_ACTION_UNSPECIFIED, false, agent.ModeBlockDeny},
	}
	for _, c := range cases {
		if got := modeDecision(c.action, c.approve); got != c.want {
			t.Errorf("%s: modeDecision = %d, want %d", c.name, got, c.want)
		}
	}
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

// TestConnectApprovalKeepalive: with x-lyon-keepalive set, a quiet
// WatchPending stream keeps beating exec-0 no-ops every 100ms — the opt-in
// contract from WatchEvents, needed because Chrome does not complete the
// first read on a held-open application/connect+json chunked response
// until the stream shows ~1s of continued flow. Without beats the web
// dialog never renders and a parked approval hangs its run forever
// (#72/#73). Without the header the stream must stay silent after the
// open marker (curl/TUI/tests don't opt in).
func TestConnectApprovalKeepalive(t *testing.T) {
	_, apprClient := newApprovalFixture(t, nil)
	t.Run("beats with header", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		req := connect.NewRequest(&lyonv1.WatchPendingRequest{})
		req.Header().Set("x-lyon-keepalive", "1")
		stream, err := apprClient.WatchPending(ctx, req)
		if err != nil {
			t.Fatalf("WatchPending: %v", err)
		}
		// Opening marker is instant, beats follow every 100ms: three
		// no-op frames should land in well under a beat budget. A lone
		// flush cannot satisfy Chrome's ~1s-of-flow condition.
		start := time.Now()
		for frames := 0; frames < 3; frames++ {
			if !stream.Receive() {
				t.Fatalf("stream ended after %d frames: %v", frames, stream.Err())
			}
			ev := stream.Msg()
			if ev.ToolName != "" || ev.ExecId != 0 {
				t.Fatalf("non-noop frame on a quiet stream: %+v", ev)
			}
		}
		if d := time.Since(start); d > 900*time.Millisecond {
			t.Fatalf("3 no-op frames took %v — keepalive beats missing or too slow", d)
		}
	})
	t.Run("silent without header", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		stream, err := apprClient.WatchPending(ctx, connect.NewRequest(&lyonv1.WatchPendingRequest{}))
		if err != nil {
			t.Fatalf("WatchPending: %v", err)
		}
		if !stream.Receive() {
			t.Fatalf("open marker missing: %v", stream.Err())
		}
		if ev := stream.Msg(); ev.ToolName != "" || ev.ExecId != 0 {
			t.Fatalf("first frame should be the exec-0 flush: %+v", ev)
		}
		got := make(chan *lyonv1.WatchPendingResponse, 1)
		go func() {
			if stream.Receive() {
				got <- stream.Msg()
			} else {
				close(got)
			}
		}()
		select {
		case ev, ok := <-got:
			if !ok {
				t.Fatalf("stream ended while waiting for silence: %v", stream.Err())
			}
			t.Fatalf("headerless stream must stay silent, got: %+v", ev)
		case <-time.After(500 * time.Millisecond):
			// silence as contracted
		}
	})
}
