package server

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	lyonv1 "lyon-agent/gen/go/lyon/v1"
	"lyon-agent/gen/go/lyon/v1/lyonv1connect"
	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/models"
)

// sessionService is the Connect implementation of lyon.v1.SessionService,
// layered directly on the same Store the legacy JSON+SSE routes use. It
// mounts on the Store's mux via New(), which Handler() wraps in the
// bearer-token check — every RPC inherits auth with zero per-route work.
type sessionService struct {
	s *Server
}

var _ lyonv1connect.SessionServiceHandler = (*sessionService)(nil)

// rpcErr maps domain errors onto Connect codes (web plan Phase 1 table).
func rpcErr(err error) error {
	if err == nil {
		return nil
	}
	code := connect.CodeInternal
	switch {
	case errors.Is(err, ErrNotFound):
		code = connect.CodeNotFound
	case errors.Is(err, ErrRingGap):
		code = connect.CodeFailedPrecondition
	case errors.Is(err, agent.ErrPromptInProgress):
		code = connect.CodeFailedPrecondition
	case errors.Is(err, agent.ErrNoPromptRunning):
		code = connect.CodeFailedPrecondition
	case errors.Is(err, agent.ErrSessionClosed):
		// The session died under the caller (reaped or deleted mid-call).
		code = connect.CodeUnavailable
	case errors.Is(err, ErrFull):
		code = connect.CodeResourceExhausted
	}
	return connect.NewError(code, err)
}

func (c *sessionService) CreateSession(_ context.Context, req *connect.Request[lyonv1.CreateSessionRequest]) (*connect.Response[lyonv1.CreateSessionResponse], error) {
	msg := req.Msg
	// v1: the engine owns one model per process; per-prompt pins ride
	// SubmitPrompt.model instead. Reject loudly rather than silently
	// ignoring a pin the caller asked for.
	if msg.Model != "" && msg.Model != c.s.eng.Config().Model {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("per-session model is not supported; set model per prompt (SubmitPrompt.model)"))
	}
	sess := c.s.eng.NewSession(c.s.obs)
	if msg.Mode != "" {
		if _, err := sess.SetMode(agent.Mode(msg.Mode)); err != nil {
			sess.Close()
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
	}
	id, err := c.s.store.Create(sess)
	if err != nil {
		sess.Close()
		return nil, rpcErr(err)
	}
	ref, err := c.s.store.Ref(id)
	if err != nil {
		// Just created; only possible if a reap raced the clock — the
		// session is gone either way.
		return nil, rpcErr(err)
	}
	return connect.NewResponse(&lyonv1.CreateSessionResponse{Session: protoSession(c.s, ref)}), nil
}

func (c *sessionService) ListSessions(_ context.Context, _ *connect.Request[lyonv1.ListSessionsRequest]) (*connect.Response[lyonv1.ListSessionsResponse], error) {
	refs := c.s.store.List() // most recently active first
	out := make([]*lyonv1.Session, 0, len(refs))
	for _, ref := range refs {
		out = append(out, protoSession(c.s, ref))
	}
	return connect.NewResponse(&lyonv1.ListSessionsResponse{Sessions: out}), nil
}

func (c *sessionService) GetSession(_ context.Context, req *connect.Request[lyonv1.GetSessionRequest]) (*connect.Response[lyonv1.GetSessionResponse], error) {
	ref, err := c.s.store.Ref(req.Msg.SessionId)
	if err != nil {
		return nil, rpcErr(err)
	}
	return connect.NewResponse(&lyonv1.GetSessionResponse{Session: protoSession(c.s, ref)}), nil
}

// GetHistory is the unary twin of WatchEvents(from_seq=-1) — the web
// transcript's initial render rides this batch instead of the stream
// (a proxied streaming response can sit un-read past any watchdog; a
// content-length response arrives like every other unary call). Same
// data, same order: hub ring snapshot through the shared protoEvent.
// last_seq anchors the client's live WatchEvents so the tail starts
// exactly where the batch ended; 0 (empty ring) tells it to fall back
// to -1 and let the full replay cover the snapshot-to-subscribe gap.
func (c *sessionService) GetHistory(_ context.Context, req *connect.Request[lyonv1.GetHistoryRequest]) (*connect.Response[lyonv1.GetHistoryResponse], error) {
	h, err := c.s.store.Hub(req.Msg.SessionId)
	if err != nil {
		return nil, rpcErr(err)
	}
	items := h.dump()
	evs := make([]*lyonv1.SessionEvent, 0, len(items))
	var last int64
	for _, it := range items {
		se := protoEvent(it)
		last = se.Seq
		evs = append(evs, se)
	}
	return connect.NewResponse(&lyonv1.GetHistoryResponse{
		Events:  evs,
		LastSeq: last,
	}), nil
}

func (c *sessionService) DeleteSession(_ context.Context, req *connect.Request[lyonv1.DeleteSessionRequest]) (*connect.Response[lyonv1.DeleteSessionResponse], error) {
	if !c.s.store.Delete(req.Msg.SessionId) {
		return nil, rpcErr(ErrNotFound)
	}
	return connect.NewResponse(&lyonv1.DeleteSessionResponse{}), nil
}

// evUserMessage is a server-synthesized transcript event: the agent has
// no user-message event type (renderers echo prompts from local state),
// so the web layer injects the prompt into the hub itself — type stays
// UNSPECIFIED on the wire and type_name carries it, the event.proto
// forward-compat channel. TUI never sees these (it consumes agent
// events directly, not through the hub).
const evUserMessage = agent.EventType("user_message")

func (c *sessionService) SubmitPrompt(_ context.Context, req *connect.Request[lyonv1.SubmitPromptRequest]) (*connect.Response[lyonv1.SubmitPromptResponse], error) {
	msg := req.Msg
	if strings.TrimSpace(msg.Prompt) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("prompt is required"))
	}
	sess, err := c.s.store.Get(msg.SessionId)
	if err != nil {
		return nil, rpcErr(err)
	}
	// Events flow to watchers through the hub ring — a watcher that
	// attaches late replays with from_seq, nothing is lost by this unary
	// ack not streaming anything.
	//
	// The echo must stamp BEFORE SubmitOn hands the prompt to the driver,
	// or the transcript renders the reply above its question; and it must
	// not stamp when the submit will be rejected, or the ring keeps a
	// phantom prompt. Busy() is the same gate SubmitOn's CAS takes, so
	// pre-checking closes the common race (concurrent double-submit is
	// the residual, and the UI already serializes sends).
	if sess.Busy() {
		return nil, rpcErr(agent.ErrPromptInProgress)
	}
	if h, herr := c.s.store.Hub(msg.SessionId); herr == nil {
		h.inject(agent.Event{Type: evUserMessage, Text: msg.Prompt})
	}
	if err := sess.SubmitOn(msg.Prompt, msg.Model); err != nil {
		return nil, rpcErr(err)
	}
	return connect.NewResponse(&lyonv1.SubmitPromptResponse{Started: true}), nil
}

func (c *sessionService) EnqueuePrompt(_ context.Context, req *connect.Request[lyonv1.EnqueuePromptRequest]) (*connect.Response[lyonv1.EnqueuePromptResponse], error) {
	msg := req.Msg
	if strings.TrimSpace(msg.Prompt) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("prompt is required"))
	}
	sess, err := c.s.store.Get(msg.SessionId)
	if err != nil {
		return nil, rpcErr(err)
	}
	// ErrNoPromptRunning (idle session) maps to FailedPrecondition — the
	// caller should fall back to SubmitPrompt.
	if err := sess.Enqueue(msg.Prompt); err != nil {
		return nil, rpcErr(err)
	}
	return connect.NewResponse(&lyonv1.EnqueuePromptResponse{QueueLen: int32(sess.QueueLen())}), nil
}

func (c *sessionService) Interrupt(_ context.Context, req *connect.Request[lyonv1.InterruptRequest]) (*connect.Response[lyonv1.InterruptResponse], error) {
	sess, err := c.s.store.Get(req.Msg.SessionId)
	if err != nil {
		return nil, rpcErr(err)
	}
	return connect.NewResponse(&lyonv1.InterruptResponse{Interrupted: sess.Interrupt()}), nil
}

func (c *sessionService) GetStats(_ context.Context, req *connect.Request[lyonv1.GetStatsRequest]) (*connect.Response[lyonv1.GetStatsResponse], error) {
	sess, err := c.s.store.Get(req.Msg.SessionId)
	if err != nil {
		return nil, rpcErr(err)
	}
	stats := sess.Stats()
	turns := make([]*lyonv1.TurnStat, 0, len(stats))
	for _, t := range stats {
		turns = append(turns, &lyonv1.TurnStat{
			Turn:       int32(t.Turn),
			StopReason: t.StopReason,
			Usage:      protoUsage(t.Usage),
			DurationMs: t.Duration.Milliseconds(),
			At:         timestamppb.New(t.At),
		})
	}
	return connect.NewResponse(&lyonv1.GetStatsResponse{Turns: turns}), nil
}

func (c *sessionService) SetMode(_ context.Context, req *connect.Request[lyonv1.SetModeRequest]) (*connect.Response[lyonv1.SetModeResponse], error) {
	msg := req.Msg
	if msg.Mode == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("mode is required (default|plan|bypass)"))
	}
	sess, err := c.s.store.Get(msg.SessionId)
	if err != nil {
		return nil, rpcErr(err)
	}
	if _, err := sess.SetMode(agent.Mode(msg.Mode)); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&lyonv1.SetModeResponse{Mode: string(sess.Mode())}), nil
}

func (c *sessionService) WatchEvents(ctx context.Context, req *connect.Request[lyonv1.WatchEventsRequest], stream *connect.ServerStream[lyonv1.WatchEventsResponse]) error {
	msg := req.Msg
	h, err := c.s.store.Hub(msg.SessionId)
	if err != nil {
		return rpcErr(err)
	}
	sub, replay, err := h.subscribe(msg.FromSeq)
	if err != nil {
		return rpcErr(err)
	}
	defer h.unsubscribe(sub)

	send := func(it hubItem) error {
		return stream.Send(&lyonv1.WatchEventsResponse{Event: protoEvent(it)})
	}
	for _, it := range replay {
		if err := send(it); err != nil {
			return err
		}
	}
	for {
		select {
		case <-ctx.Done():
			return nil // client went away; teardown is the framework's
		case it, ok := <-sub.ch:
			if !ok {
				if sub.kicked {
					// Fell behind hubSubBuffer: reconnect and replay from
					// a recent seq (the ring still holds it).
					return connect.NewError(connect.CodeResourceExhausted,
						errors.New("event subscriber fell behind; reconnect and replay from a recent seq"))
				}
				return nil // session closed — clean EOF
			}
			if err := send(it); err != nil {
				return err
			}
		}
	}
}

// protoSession renders a store record as the wire Session. Workspace and
// model are engine-level in v1 (one engine, one model per process).
func protoSession(s *Server, ref StoreRef) *lyonv1.Session {
	sess := ref.Session
	return &lyonv1.Session{
		Id:         ref.ID,
		Workspace:  s.eng.Config().Workspace,
		Model:      s.eng.Config().Model,
		Title:      sess.Title(),
		Mode:       string(sess.Mode()),
		Busy:       sess.Busy(),
		QueueLen:   int32(sess.QueueLen()),
		Turns:      int32(len(sess.Stats())),
		CreatedAt:  timestamppb.New(ref.Created),
		LastActive: timestamppb.New(ref.LastActive),
		// windowFor (cmd/lyon/usage.go) twin: knowledge window when the
		// model is on file, the configured fallback otherwise — the ctx
		// gauge's denominator.
		ContextWindow: windowForModel(s.eng.Config()),
	}
}

// windowForModel resolves the context-window denominator for the gauge.
func windowForModel(cfg agent.Config) int64 {
	if info := models.Lookup(cfg.Model); info != nil && info.Context > 0 {
		return info.Context
	}
	if cfg.ContextWindow > 0 {
		return cfg.ContextWindow
	}
	return 0
}

func protoUsage(u agent.Usage) *lyonv1.Usage {
	return &lyonv1.Usage{
		Input:         u.Input,
		Output:        u.Output,
		CacheRead:     u.CacheRead,
		CacheCreation: u.CacheCreation,
	}
}

// protoEvent maps a stamped hub item onto the wire. type_name always
// carries the raw EventType string, so a client can still render a future
// type the binary doesn't know (type stays UNSPECIFIED — protojson-safe).
func protoEvent(it hubItem) *lyonv1.SessionEvent {
	se := &lyonv1.SessionEvent{
		Seq:        it.seq,
		Type:       protoEventType(it.ev.Type),
		TypeName:   string(it.ev.Type),
		Text:       it.ev.Text,
		Thinking:   it.ev.Thinking,
		ToolId:     it.ev.ToolID,
		ExecId:     int64(it.ev.ExecID),
		ToolName:   it.ev.ToolName,
		InputJson:  string(it.ev.Input),
		Server:     it.ev.Server,
		Sub:        it.ev.Sub,
		SubId:      it.ev.SubID,
		DurationMs: it.ev.DurationMS,
		IsError:    it.ev.IsError,
		Error:      it.ev.Err,
		Turn:       int32(it.ev.Turn),
		StopReason: it.ev.StopReason,
		Iterations: int32(it.ev.Iterations),
		Detail:     it.ev.Detail,
	}
	if len(it.ev.Attrs) > 0 {
		se.Attrs = it.ev.Attrs
	}
	if it.ev.Usage != nil {
		se.Usage = protoUsage(*it.ev.Usage)
	}
	if !it.ev.TS.IsZero() {
		se.Ts = timestamppb.New(it.ev.TS)
	}
	return se
}

func protoEventType(t agent.EventType) lyonv1.EventType {
	switch t {
	case agent.EventMessageStart:
		return lyonv1.EventType_EVENT_TYPE_MESSAGE_START
	case agent.EventTextDelta:
		return lyonv1.EventType_EVENT_TYPE_TEXT_DELTA
	case agent.EventThinkingDelta:
		return lyonv1.EventType_EVENT_TYPE_THINKING_DELTA
	case agent.EventToolUseStart:
		return lyonv1.EventType_EVENT_TYPE_TOOL_USE_START
	case agent.EventToolExecStart:
		return lyonv1.EventType_EVENT_TYPE_TOOL_EXEC_START
	case agent.EventToolExecOutput:
		return lyonv1.EventType_EVENT_TYPE_TOOL_EXEC_OUTPUT
	case agent.EventToolExecEnd:
		return lyonv1.EventType_EVENT_TYPE_TOOL_EXEC_END
	case agent.EventServerToolResult:
		return lyonv1.EventType_EVENT_TYPE_SERVER_TOOL_RESULT
	case agent.EventCompaction:
		return lyonv1.EventType_EVENT_TYPE_COMPACTION
	case agent.EventSubagentStart:
		return lyonv1.EventType_EVENT_TYPE_SUBAGENT_START
	case agent.EventSubagentEnd:
		return lyonv1.EventType_EVENT_TYPE_SUBAGENT_END
	case agent.EventMemoryInjected:
		return lyonv1.EventType_EVENT_TYPE_MEMORY_INJECTED
	case agent.EventQueueInjected:
		return lyonv1.EventType_EVENT_TYPE_QUEUE_INJECTED
	case agent.EventTurnEnd:
		return lyonv1.EventType_EVENT_TYPE_TURN_END
	case agent.EventPromptEnd:
		return lyonv1.EventType_EVENT_TYPE_PROMPT_END
	case agent.EventError:
		return lyonv1.EventType_EVENT_TYPE_ERROR
	default:
		return lyonv1.EventType_EVENT_TYPE_UNSPECIFIED
	}
}
