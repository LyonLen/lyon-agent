package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

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

// seedDump writes one dump document in exactly the on-disk shape dump.go
// produces, under today's dumps/ directory (so day="" finds it), and
// returns the run id.
func seedDump(t *testing.T, home string) string {
	t.Helper()
	now := time.Now()
	runID := now.Format("20060102") + "-120000-abcdef012345"
	dir := filepath.Join(home, "dumps", now.Format("2006-01-02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{
		"schema_version": 1,
		"run_id":         runID,
		"workspace":      "/ws",
		"model":          "m-dash",
		"prompt":         "make it work",
		"first_prompt":   "make it work",
		"started_at":     time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC),
		"ended_at":       time.Date(2026, 10, 16, 12, 0, 3, 0, time.UTC),
		"duration_ms":    int64(3000),
		"iterations":     2,
		"stop_reason":    "end_turn",
		"prompt_err":     "",
		"usage":          map[string]any{"input": 3, "output": 4, "cache_read": 5, "cache_creation": 6},
		"events": []map[string]any{
			{"seq": 1, "type": "message_start"},
			{"seq": 2, "type": "tool_use_start", "tool_name": "read_file", "tool_id": "t1", "input": map[string]any{"path": "a.txt"}},
			{"seq": 3, "type": "prompt_end", "turn": 1, "stop_reason": "end_turn"},
		},
		"history": "final history",
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "120000-abcdef012345.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return runID
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

// TestConnectDashboardRuns: empty day → [], seeded day → header row,
// GetRun → full replay payload, and selector hygiene (bad day/limit →
// InvalidArgument, unknown run → NotFound, malformed id →
// InvalidArgument — never a path).
func TestConnectDashboardRuns(t *testing.T) {
	client, home := newDashboardFixture(t)
	ctx := context.Background()

	empty, err := client.ListRuns(ctx, connect.NewRequest(&lyonv1.ListRunsRequest{}))
	if err != nil || len(empty.Msg.Runs) != 0 {
		t.Fatalf("fresh home ListRuns = %+v, err %v", empty.Msg.Runs, err)
	}

	runID := seedDump(t, home)

	list, err := client.ListRuns(ctx, connect.NewRequest(&lyonv1.ListRunsRequest{}))
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(list.Msg.Runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(list.Msg.Runs))
	}
	r := list.Msg.Runs[0]
	if r.RunId != runID || r.Prompt != "make it work" || r.DurationMs != 3000 ||
		r.Model != "m-dash" || r.Failed || r.StartedAt == nil {
		t.Fatalf("run ref = %+v", r)
	}

	run, err := client.GetRun(ctx, connect.NewRequest(&lyonv1.GetRunRequest{RunId: runID}))
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	d := run.Msg
	if d.History != "final history" || d.Iterations != 2 || d.StopReason != "end_turn" ||
		d.Usage.Input != 3 || d.Usage.CacheRead != 5 || d.SubUsage != nil {
		t.Fatalf("run doc = %+v", d)
	}
	if len(d.Events) != 3 || d.Events[0].Seq != 1 || d.Events[1].ToolName != "read_file" ||
		d.Events[2].Type != "prompt_end" {
		t.Fatalf("events = %+v", d.Events)
	}
	var input map[string]any
	if err := json.Unmarshal([]byte(d.Events[1].InputJson), &input); err != nil ||
		input["path"] != "a.txt" {
		t.Fatalf("input_json = %q, err %v", d.Events[1].InputJson, err)
	}

	for _, tc := range []struct {
		name string
		req  *lyonv1.ListRunsRequest
	}{
		{"bad day", &lyonv1.ListRunsRequest{Day: "../.."}},
		{"bad limit", &lyonv1.ListRunsRequest{Limit: -1}},
	} {
		_, err := client.ListRuns(ctx, connect.NewRequest(tc.req))
		if connectCode(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: code = %v, want InvalidArgument (err %v)", tc.name, connectCode(err), err)
		}
	}
	_, err = client.GetRun(ctx, connect.NewRequest(&lyonv1.GetRunRequest{RunId: "../../../etc/passwd"}))
	if connectCode(err) != connect.CodeInvalidArgument {
		t.Errorf("traversal id: code = %v, want InvalidArgument (err %v)", connectCode(err), err)
	}
	_, err = client.GetRun(ctx, connect.NewRequest(&lyonv1.GetRunRequest{
		RunId: time.Now().Format("20060102") + "-120000-ffffffffffff",
	}))
	if connectCode(err) != connect.CodeNotFound {
		t.Errorf("missing run: code = %v, want NotFound (err %v)", connectCode(err), err)
	}
}

// TestConnectDashboardAudit: tail semantics over the JSONL file —
// newest last, torn lines skipped, a day that never wrote is empty (not
// NotFound), and the same selector hygiene as runs.
func TestConnectDashboardAudit(t *testing.T) {
	client, home := newDashboardFixture(t)
	ctx := context.Background()

	none, err := client.ListAudit(ctx, connect.NewRequest(&lyonv1.ListAuditRequest{}))
	if err != nil || len(none.Msg.Records) != 0 {
		t.Fatalf("fresh home ListAudit = %+v, err %v", none.Msg.Records, err)
	}

	day := time.Now().Format("2006-01-02")
	if err := os.MkdirAll(filepath.Join(home, "audit"), 0o755); err != nil {
		t.Fatal(err)
	}
	lines := ""
	for i, ev := range []string{"prompt_start", "approve_request", "NOT-JSON", "security_block"} {
		if ev == "NOT-JSON" {
			lines += "{torn\n"
			continue
		}
		lines += fmt.Sprintf(`{"time":"2026-10-16T10:0%d:00Z","event":%q,"session":"s1","tool":"bash","is_error":true,"error":"boom","duration_ms":42}`+"\n", i, ev)
	}
	if err := os.WriteFile(filepath.Join(home, "audit", day+".jsonl"), []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}

	full, err := client.ListAudit(ctx, connect.NewRequest(&lyonv1.ListAuditRequest{}))
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(full.Msg.Records) != 3 {
		t.Fatalf("records = %d, want 3 (torn line skipped)", len(full.Msg.Records))
	}
	last := full.Msg.Records[2]
	if last.Event != "security_block" || !last.IsError || last.Err != "boom" ||
		last.DurationMs != 42 || last.Tool != "bash" {
		t.Fatalf("last record = %+v", last)
	}

	two, err := client.ListAudit(ctx, connect.NewRequest(&lyonv1.ListAuditRequest{Tail: 2}))
	if err != nil || len(two.Msg.Records) != 2 {
		t.Fatalf("tail=2 → %d records, err %v", len(two.Msg.Records), err)
	}
	if two.Msg.Records[0].Event != "approve_request" || two.Msg.Records[1].Event != "security_block" {
		t.Fatalf("tail slice = %+v", two.Msg.Records)
	}

	_, err = client.ListAudit(ctx, connect.NewRequest(&lyonv1.ListAuditRequest{Day: "nope"}))
	if connectCode(err) != connect.CodeInvalidArgument {
		t.Errorf("bad day: code = %v, want InvalidArgument", connectCode(err))
	}
	_, err = client.ListAudit(ctx, connect.NewRequest(&lyonv1.ListAuditRequest{Tail: -1}))
	if connectCode(err) != connect.CodeInvalidArgument {
		t.Errorf("negative tail: code = %v, want InvalidArgument", connectCode(err))
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
