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
