package server

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	lyonv1 "lyon-agent/gen/go/lyon/v1"
	"lyon-agent/gen/go/lyon/v1/lyonv1connect"
)

// planService is the v1 stub of lyon.v1.PlanService — every method
// answers Unimplemented until #15 (plan horizontal tree, node-level
// approval/edit, concurrent step growth, live output, guidance
// injection) lands. Message shapes are frozen in plan.proto so field
// numbers never churn; only the behavior is missing.
type planService struct{}

var _ lyonv1connect.PlanServiceHandler = (*planService)(nil)

// errPlanStub is shared across the stub's RPCs (see errApprovalStub).
var errPlanStub = connect.NewError(connect.CodeUnimplemented,
	errors.New("plan service is a v1 stub — plan tree lands with #15 (plan.proto TODO(#15))"))

func (s *planService) GetPlan(context.Context, *connect.Request[lyonv1.GetPlanRequest]) (*connect.Response[lyonv1.GetPlanResponse], error) {
	return nil, errPlanStub
}

func (s *planService) WatchPlan(context.Context, *connect.Request[lyonv1.WatchPlanRequest], *connect.ServerStream[lyonv1.WatchPlanResponse]) error {
	return errPlanStub
}

func (s *planService) UpdateNode(context.Context, *connect.Request[lyonv1.UpdateNodeRequest]) (*connect.Response[lyonv1.UpdateNodeResponse], error) {
	return nil, errPlanStub
}

func (s *planService) ApproveNode(context.Context, *connect.Request[lyonv1.ApproveNodeRequest]) (*connect.Response[lyonv1.ApproveNodeResponse], error) {
	return nil, errPlanStub
}

func (s *planService) ExecutePlan(context.Context, *connect.Request[lyonv1.ExecutePlanRequest]) (*connect.Response[lyonv1.ExecutePlanResponse], error) {
	return nil, errPlanStub
}

func (s *planService) GuideNode(context.Context, *connect.Request[lyonv1.GuideNodeRequest]) (*connect.Response[lyonv1.GuideNodeResponse], error) {
	return nil, errPlanStub
}
