package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"

	lyonv1 "lyon-agent/gen/go/lyon/v1"
	"lyon-agent/gen/go/lyon/v1/lyonv1connect"
	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/tools"
)

// newDashboardFixture serves a real server on a scratch LYON_HOME — no
// prompts run, so no mock gateway is needed; the panels only read disk
// and in-memory aggregates.
func newDashboardFixture(t *testing.T) (lyonv1connect.DashboardServiceClient, string) {
	t.Helper()
	home := scratchHome(t)
	eng := agent.NewEngine(agent.DefaultConfig(), nil)
	s := New(eng, NewStore(0, 0))
	s.SetObserver(tools.NewMetrics())
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return lyonv1connect.NewDashboardServiceClient(http.DefaultClient, ts.URL), home
}

// TestConnectDashboardOverview: aggregates wire up, the leaderboard keeps
// its full class order even on an empty board, and routing reflects a
// seeded table.
func TestConnectDashboardOverview(t *testing.T) {
	client, home := newDashboardFixture(t)
	ctx := context.Background()

	// Seed routing.json the way routing.Table persists it.
	routingDoc := map[string]any{"entries": map[string]any{
		"coding": map[string]any{
			"class": "coding", "agent": "routed-agent", "model": "m-x",
			"confidence": 0.8, "samples": 4, "mean_score": 0.75,
			"updated": "2026-10-16T08:00:00Z",
		},
	}}
	data, _ := json.Marshal(routingDoc)
	if err := os.WriteFile(filepath.Join(home, "routing.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := client.GetOverview(ctx, connect.NewRequest(&lyonv1.GetOverviewRequest{}))
	if err != nil {
		t.Fatalf("GetOverview: %v", err)
	}
	msg := res.Msg
	if msg.Sessions != 0 || msg.CacheShare != 0 || msg.UptimeSeconds < 0 || msg.Model == "" {
		t.Fatalf("overview = %+v", msg)
	}
	if len(msg.Routing) != 1 || msg.Routing[0].Class != "coding" ||
		msg.Routing[0].Agent != "routed-agent" || msg.Routing[0].Samples != 4 {
		t.Fatalf("routing = %+v", msg.Routing)
	}
	// Empty ledger still renders every class row (Views' stable-order
	// contract — same rows the TUI panel shows).
	if len(msg.Leaderboard) != len(dashClassOrder) {
		t.Fatalf("leaderboard rows = %d, want %d", len(msg.Leaderboard), len(dashClassOrder))
	}
	if msg.Leaderboard[0].Class != "coding" {
		t.Fatalf("first row = %q, want coding", msg.Leaderboard[0].Class)
	}
}

// TestConnectDashboardSnapshots: fresh store lists empty; a seeded
// snapshot round-trips its row (title/messages/saved_at/path).
func TestConnectDashboardSnapshots(t *testing.T) {
	client, _ := newDashboardFixture(t)
	ctx := context.Background()

	none, err := client.ListSnapshots(ctx, connect.NewRequest(&lyonv1.ListSnapshotsRequest{}))
	if err != nil || len(none.Msg.Snapshots) != 0 {
		t.Fatalf("fresh home ListSnapshots = %+v, err %v", none.Msg.Snapshots, err)
	}

	workspace := agent.NewEngine(agent.DefaultConfig(), nil).Config().Workspace
	path, err := agent.SnapshotPath(workspace)
	if err != nil {
		t.Fatal(err)
	}
	snap := map[string]any{
		"Version":   1,
		"Workspace": workspace,
		"Model":     "m-dash",
		"Title":     "seeded chat",
		"SavedAt":   "2026-10-16T10:00:00Z",
		"Messages":  []map[string]any{{"role": "user"}, {"role": "assistant"}},
	}
	data, _ := json.Marshal(snap)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := client.ListSnapshots(ctx, connect.NewRequest(&lyonv1.ListSnapshotsRequest{}))
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(got.Msg.Snapshots) != 1 {
		t.Fatalf("snapshots = %+v", got.Msg.Snapshots)
	}
	s := got.Msg.Snapshots[0]
	if s.Title != "seeded chat" || s.Messages != 2 || s.SavedAt == nil || s.Path != path {
		t.Fatalf("snapshot = %+v", s)
	}
}
