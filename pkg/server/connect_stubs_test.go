package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	lyonv1 "lyon-agent/gen/go/lyon/v1"
	"lyon-agent/gen/go/lyon/v1/lyonv1connect"
	"lyon-agent/pkg/agent"
)

// newStubFixture serves a real server for the approval/plan stubs — no
// prompts, no disk reads; the RPCs answer before touching anything.
func newStubFixture(t *testing.T) (lyonv1connect.ApprovalServiceClient, lyonv1connect.PlanServiceClient) {
	t.Helper()
	scratchHome(t)
	eng := agent.NewEngine(agent.DefaultConfig(), nil)
	s := New(eng, NewStore(0, 0))
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return lyonv1connect.NewApprovalServiceClient(http.DefaultClient, ts.URL),
		lyonv1connect.NewPlanServiceClient(http.DefaultClient, ts.URL)
}

// wantUnimplemented asserts err is a connect Unimplemented error — the
// one stable contract these stubs make.
func wantUnimplemented(t *testing.T, err error) {
	t.Helper()
	if got := connect.CodeOf(err); got != connect.CodeUnimplemented {
		t.Fatalf("want connect code Unimplemented, got %v: %v", got, err)
	}
}

// streamErr drains a server stream that should fail immediately; the
// error may surface either at construction or from Err() after the
// Receive loop, depending on when the handler's error reaches the wire.
func streamErr[T any](stream *connect.ServerStreamForClient[T], err error) error {
	if err != nil {
		return err
	}
	for stream.Receive() {
	}
	return stream.Err()
}

// TestConnectStubs: approval and plan are mounted but answer
// Unimplemented on every method — mounted (not 404) so the web client
// can probe them and degrade through errText.
func TestConnectStubs(t *testing.T) {
	appr, plan := newStubFixture(t)
	ctx := context.Background()

	t.Run("approval/Decide", func(t *testing.T) {
		_, err := appr.Decide(ctx, connect.NewRequest(&lyonv1.DecideRequest{}))
		wantUnimplemented(t, err)
	})
	t.Run("approval/WatchPending", func(t *testing.T) {
		st, err := appr.WatchPending(ctx, connect.NewRequest(&lyonv1.WatchPendingRequest{}))
		wantUnimplemented(t, streamErr(st, err))
	})

	unaryPlan := []struct {
		name string
		call func() error
	}{
		{"plan/GetPlan", func() error {
			_, err := plan.GetPlan(ctx, connect.NewRequest(&lyonv1.GetPlanRequest{}))
			return err
		}},
		{"plan/UpdateNode", func() error {
			_, err := plan.UpdateNode(ctx, connect.NewRequest(&lyonv1.UpdateNodeRequest{}))
			return err
		}},
		{"plan/ApproveNode", func() error {
			_, err := plan.ApproveNode(ctx, connect.NewRequest(&lyonv1.ApproveNodeRequest{}))
			return err
		}},
		{"plan/ExecutePlan", func() error {
			_, err := plan.ExecutePlan(ctx, connect.NewRequest(&lyonv1.ExecutePlanRequest{}))
			return err
		}},
		{"plan/GuideNode", func() error {
			_, err := plan.GuideNode(ctx, connect.NewRequest(&lyonv1.GuideNodeRequest{}))
			return err
		}},
	}
	for _, tc := range unaryPlan {
		t.Run(tc.name, func(t *testing.T) { wantUnimplemented(t, tc.call()) })
	}
	t.Run("plan/WatchPlan", func(t *testing.T) {
		st, err := plan.WatchPlan(ctx, connect.NewRequest(&lyonv1.WatchPlanRequest{}))
		wantUnimplemented(t, streamErr(st, err))
	})
}
