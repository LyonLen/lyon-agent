package server

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"

	lyonv1 "lyon-agent/gen/go/lyon/v1"
	"lyon-agent/gen/go/lyon/v1/lyonv1connect"
)

// approvalService is lyon.v1.ApprovalService. Without -approve the bridge
// is nil and both RPCs keep the Phase 5 Unimplemented contract; with one,
// WatchPending streams the parked asks and Decide answers them (proto
// approval.proto — parks until decided or the run context dies, matching
// TUI's fail-closed behavior).
type approvalService struct {
	s *Server
}

var _ lyonv1connect.ApprovalServiceHandler = (*approvalService)(nil)

// errApprovalStub is shared across the not-wired RPCs: connect errors are
// immutable once built, and every caller sees the same story.
var errApprovalStub = connect.NewError(connect.CodeUnimplemented,
	errors.New("approval is off — start lyond with -approve (tools auto-approve without it)"))

// WatchPending streams pending approvals: the current set first (a
// reconnecting client converges), then live arrivals and resolves (an
// empty tool_name is the removal signal — approval.proto). Each parked
// approval is one message; a lane that cannot keep up is dropped by the
// bridge and this stream simply ends, letting the client re-snapshot.
func (s *approvalService) WatchPending(ctx context.Context, req *connect.Request[lyonv1.WatchPendingRequest], stream *connect.ServerStream[lyonv1.WatchPendingResponse]) error {
	b := s.s.appr
	if b == nil {
		return errApprovalStub
	}
	id, ch := b.subscribe()
	defer b.unsubscribe(id)
	// Flush the streaming response headers with one no-op message (all
	// fields empty: no seat, no exec — every client treats it as a
	// nothing-to-drop resolve). Without it, a connect-Go client blocks in
	// its constructor waiting for response headers whenever the park set
	// is momentarily empty, and nothing would ever unblock it: the handler
	// only writes when it Sends.
	if err := stream.Send(&lyonv1.WatchPendingResponse{}); err != nil {
		return err
	}
	for _, p := range b.snapshot() {
		if err := stream.Send(p); err != nil {
			return err
		}
	}
	// Chrome keepalive (opt-in via x-lyon-keepalive): same held-open quirk
	// as WatchEvents (see that handler) — the open burst lands, then this
	// stream falls silent while the bridge waits, and Chrome never
	// completes the first read until the stream shows ~1s of continued
	// flow. The web dialog would never render and the parked tool hung
	// its run forever (#72/#73). The beat is the same exec-0 no-op every
	// client already folds into its ignore branch.
	var ka <-chan time.Time
	if req.Header().Get("x-lyon-keepalive") == "1" {
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		ka = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ka:
			if err := stream.Send(&lyonv1.WatchPendingResponse{}); err != nil {
				return err
			}
		case ev, ok := <-ch:
			if !ok {
				return nil // slow-kick: end cleanly, the client re-snapshots
			}
			if err := stream.Send(ev); err != nil {
				return err
			}
		}
	}
}

// Decide answers one parked approval: the bridge unblocks the approvee,
// the tool proceeds (or its result becomes "user declined"), and every
// watcher drops the card.
func (s *approvalService) Decide(_ context.Context, req *connect.Request[lyonv1.DecideRequest]) (*connect.Response[lyonv1.DecideResponse], error) {
	b := s.s.appr
	if b == nil {
		return nil, errApprovalStub
	}
	msg := req.Msg
	if err := b.decide(msg.SessionId, uint64(msg.ExecId), msg.Approve, msg.Remember, msg.Action); err != nil {
		if errors.Is(err, ErrApprovalGone) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&lyonv1.DecideResponse{
		SessionId: msg.SessionId,
		ExecId:    msg.ExecId,
		Approve:   msg.Approve,
	}), nil
}
