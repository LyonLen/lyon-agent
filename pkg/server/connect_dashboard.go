package server

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	lyonv1 "lyon-agent/gen/go/lyon/v1"
	"lyon-agent/gen/go/lyon/v1/lyonv1connect"
	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/eval"
	"lyon-agent/pkg/routing"
)

// dashboardService is the Connect implementation of lyon.v1.DashboardService:
// read-only panels over $LYON_HOME on-disk data (saved sessions, routing
// table, leaderboard) plus the process aggregates /metrics already
// computes. Run dumps and the audit tail are read by the agent itself
// (pkg/agent/dumpread.go), not over the wire. Mounted in New() like its
// siblings, so the bearer wrapper covers every RPC here too.
type dashboardService struct {
	s *Server
}

var _ lyonv1connect.DashboardServiceHandler = (*dashboardService)(nil)

// dashClassOrder mirrors cmd/lyon's evoClassOrder — the evolution panel's
// row order (routing's classes, most common first). package main cannot be
// imported from here, so the list is copied; Views appends any extra class
// the ledger grows, sorted, so a rename lands at the bottom instead of
// vanishing.
var dashClassOrder = []string{
	string(routing.ClassCoding),
	string(routing.ClassFix),
	string(routing.ClassReview),
	string(routing.ClassResearch),
	string(routing.ClassWriting),
	string(routing.ClassDesign),
	string(routing.ClassData),
	string(routing.ClassGeneral),
}

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

// GetOverview answers "how is this process doing" in one shot: live
// session rollup (shared with /metrics — one aggregation, two
// transports), the routing network, and the leaderboard. Disk reads are
// per-call on purpose: routing/leaderboard are small files and re-reading
// means the panel never serves a stale cache.
func (s *dashboardService) GetOverview(_ context.Context, _ *connect.Request[lyonv1.GetOverviewRequest]) (*connect.Response[lyonv1.GetOverviewResponse], error) {
	agg := s.s.usageRollup()
	out := &lyonv1.GetOverviewResponse{
		Sessions:      int32(agg.Sessions),
		UptimeSeconds: int64(time.Since(s.s.start).Seconds()),
		Model:         s.s.eng.Config().Model,
		CacheShare:    agg.CacheShare,
	}
	for _, e := range routing.Open().All() {
		out.Routing = append(out.Routing, &lyonv1.RoutingEntry{
			Class:      e.Class,
			Agent:      e.Agent,
			Model:      e.Model,
			Confidence: e.Confidence,
			Samples:    int32(e.Samples),
			MeanScore:  e.MeanScore,
			Updated:    timestamppb.New(e.Updated),
		})
	}
	for _, v := range eval.OpenLeaderboard().Views(dashClassOrder) {
		out.Leaderboard = append(out.Leaderboard, &lyonv1.LeaderboardView{
			Class:      v.Class,
			OnDuty:     v.OnDuty,
			Rounds:     int32(v.Rounds),
			ExamFails:  int32(v.ExamFails),
			Cards:      int32(v.Cards),
			Mean:       v.Mean,
			LastAgent:  v.LastAgent,
			LastTotal:  v.LastTotal,
			LastPassed: v.LastPassed,
			LastSource: v.LastSource,
		})
	}
	return connect.NewResponse(out), nil
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
