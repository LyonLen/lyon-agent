package server

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	"google.golang.org/protobuf/types/known/timestamppb"

	lyonv1 "lyon-agent/gen/go/lyon/v1"
	"lyon-agent/pkg/agent"
)

// ListCheckpoints is the undo stack view (TUI /rollback parity): newest
// first, each entry one prompt boundary or pre-compaction capture.
func (c *sessionService) ListCheckpoints(_ context.Context, req *connect.Request[lyonv1.ListCheckpointsRequest]) (*connect.Response[lyonv1.ListCheckpointsResponse], error) {
	sess, err := c.s.store.Get(req.Msg.SessionId)
	if err != nil {
		return nil, rpcErr(err)
	}
	cps := sess.Checkpoints()
	out := make([]*lyonv1.Checkpoint, 0, len(cps))
	for _, cp := range cps {
		out = append(out, protoCheckpoint(cp))
	}
	return connect.NewResponse(&lyonv1.ListCheckpointsResponse{Checkpoints: out}), nil
}

// RollbackSession rewinds the conversation to one checkpoint. The session
// blocks on its history lock while a prompt is in flight (same semantic as
// TUI deferring /rollback to prompt_end — histMu makes it wait instead of
// corrupting a live drive), then the ring is re-synthesized from the
// post-rollback history: reconnect WatchEvents with from_seq=-1 to see the
// rewound transcript. Turn stats and spend are kept; only the
// conversation rewinds (pkg/agent/rollback.go).
func (c *sessionService) RollbackSession(_ context.Context, req *connect.Request[lyonv1.RollbackSessionRequest]) (*connect.Response[lyonv1.RollbackSessionResponse], error) {
	sid := req.Msg.SessionId
	sess, err := c.s.store.Get(sid)
	if err != nil {
		return nil, rpcErr(err)
	}
	h, err := c.s.store.Hub(sid)
	if err != nil {
		return nil, rpcErr(err)
	}
	id := int(req.Msg.Id)
	if id == 0 {
		// 0 = the newest checkpoint (Checkpoints is newest-first).
		cps := sess.Checkpoints()
		if len(cps) == 0 {
			return nil, connect.NewError(connect.CodeNotFound,
				errors.New("rollback: no checkpoints yet — every prompt captures one before it runs"))
		}
		id = cps[0].ID
	}
	cp, err := sess.Rollback(id)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	// The history was rewritten under the ring — replay must narrate the
	// rewound conversation, not the discarded one (resetAndInject).
	snap := sess.Export()
	h.resetAndInject(synthSnapshotEvents(snap.Messages))
	return connect.NewResponse(&lyonv1.RollbackSessionResponse{Checkpoint: protoCheckpoint(cp)}), nil
}

// SaveCheckpoint pins an explicit undo point (TUI /rollback save parity):
// the caller's label lands on the checkpoint so the undo stack stays
// readable among the automatic prompt-boundary captures. Shares histMu
// with the prompt path — a save during a live drive waits, never races.
func (c *sessionService) SaveCheckpoint(_ context.Context, req *connect.Request[lyonv1.SaveCheckpointRequest]) (*connect.Response[lyonv1.SaveCheckpointResponse], error) {
	sess, err := c.s.store.Get(req.Msg.SessionId)
	if err != nil {
		return nil, rpcErr(err)
	}
	cp, err := sess.Checkpoint(req.Msg.Label)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	return connect.NewResponse(&lyonv1.SaveCheckpointResponse{Checkpoint: protoCheckpoint(cp)}), nil
}

func protoCheckpoint(cp agent.CheckpointInfo) *lyonv1.Checkpoint {
	return &lyonv1.Checkpoint{
		Id:       int32(cp.ID),
		Kind:     cp.Kind,
		Label:    cp.Label,
		Messages: int32(cp.Messages),
		At:       timestamppb.New(cp.At),
	}
}
