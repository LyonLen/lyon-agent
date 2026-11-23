package server

import (
	"context"
	"os"
	"path/filepath"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	lyonv1 "lyon-agent/gen/go/lyon/v1"
	"lyon-agent/gen/go/lyon/v1/lyonv1connect"
	"lyon-agent/pkg/agent"
)

// dashboardService is the Connect implementation of lyon.v1.DashboardService:
// read-only panels over $LYON_HOME on-disk data (saved sessions) plus the
// process aggregates /metrics already computes. Run dumps and the audit
// tail are read by the agent itself (pkg/agent/dumpread.go), not over the
// wire. Mounted in New() like its siblings, so the bearer wrapper covers
// every RPC here too. (GetOverview — the evolution panel's
// routing/leaderboard half — went out with the evolution system.)
type dashboardService struct {
	s *Server
}

var _ lyonv1connect.DashboardServiceHandler = (*dashboardService)(nil)

// dashHome resolves $LYON_HOME for the on-disk panels with the same rule
// as pkg/agent's lyonHome: env override, else ~/.lyon.
func dashHome() (string, error) {
	if h := os.Getenv("LYON_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".lyon"), nil
}

// ListSnapshots lists the workspace's saved sessions (the /resume
// store) — same call the TUI's history panel makes.
//
// Snapshots whose conversation is already a seat are hidden: that row is
// already in the live list, and offering it again as "saved" is how one
// conversation used to appear twice — once live, once as an archive copy of
// itself.
func (s *dashboardService) ListSnapshots(_ context.Context, _ *connect.Request[lyonv1.ListSnapshotsRequest]) (*connect.Response[lyonv1.ListSnapshotsResponse], error) {
	refs, err := agent.ListSnapshots(s.s.eng.Config().Workspace)
	if err != nil {
		return nil, fileOSErr(err, "sessions")
	}
	seats := s.s.store.SeatIDs()
	out := &lyonv1.ListSnapshotsResponse{Snapshots: make([]*lyonv1.SnapshotRef, 0, len(refs))}
	for _, r := range refs {
		if r.ID != "" && seats[r.ID] {
			continue
		}
		out.Snapshots = append(out.Snapshots, &lyonv1.SnapshotRef{
			Title:    r.Title,
			Messages: int32(r.Messages),
			SavedAt:  timestamppb.New(r.SavedAt),
			Path:     r.Path,
		})
	}
	return connect.NewResponse(out), nil
}
