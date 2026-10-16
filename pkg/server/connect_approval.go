package server

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	lyonv1 "lyon-agent/gen/go/lyon/v1"
	"lyon-agent/gen/go/lyon/v1/lyonv1connect"
)

// approvalService is the v1 stub of lyon.v1.ApprovalService. lyond never
// sets cfg.Approve (cmd/lyond/main.go), so every NeedsApproval tool
// auto-approves and no pending queue exists to watch or decide — both
// RPCs answer Unimplemented until -approve grows a tools.ApproveFunc
// that parks the approvee (proto approval.proto TODO(wire): return false
// on timeout, matching TUI behavior).
type approvalService struct{}

var _ lyonv1connect.ApprovalServiceHandler = (*approvalService)(nil)

// errApprovalStub is shared across the stub's RPCs: connect errors are
// immutable once built, and every caller sees the same story.
var errApprovalStub = connect.NewError(connect.CodeUnimplemented,
	errors.New("approval not wired yet — lyond has no -approve flag, tools auto-approve (approval.proto TODO(wire))"))

func (s *approvalService) WatchPending(context.Context, *connect.Request[lyonv1.WatchPendingRequest], *connect.ServerStream[lyonv1.WatchPendingResponse]) error {
	return errApprovalStub
}

func (s *approvalService) Decide(context.Context, *connect.Request[lyonv1.DecideRequest]) (*connect.Response[lyonv1.DecideResponse], error) {
	return nil, errApprovalStub
}
