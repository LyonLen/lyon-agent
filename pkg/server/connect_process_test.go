package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"

	lyonv1 "lyon-agent/gen/go/lyon/v1"
	"lyon-agent/gen/go/lyon/v1/lyonv1connect"
	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/tools"
)

// TestProcessPanelListKill: the panel chain end to end — a real
// background task through the tools seam shows up in ListProcesses with
// what the row renders, KillProcess takes it down, the next list is
// empty, and a stale id answers NotFound (the row was already gone).
func TestProcessPanelListKill(t *testing.T) {
	home := scratchHome(t)
	eng := agent.NewEngine(agent.DefaultConfig(), nil)
	s := New(eng, NewStore(0, 0))
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	client := lyonv1connect.NewProcessServiceClient(http.DefaultClient, ts.URL)
	ctx := context.Background()

	// A live task under a known owner — Execute takes raw JSON, so the
	// unexported shellInput stays in pkg/tools.
	sh := &tools.Shell{Root: home}
	owner := tools.WithBackgroundOwner(context.Background(), "proc-panel-test", func(tools.BgInfo) {})
	if _, err := sh.Execute(owner, json.RawMessage(`{"command":"sleep 30","background":true}`)); err != nil {
		t.Fatalf("start background: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	list := func() *lyonv1.ListProcessesResponse {
		t.Helper()
		res, err := client.ListProcesses(ctx, connect.NewRequest(&lyonv1.ListProcessesRequest{}))
		if err != nil {
			t.Fatalf("ListProcesses: %v", err)
		}
		return res.Msg
	}

	// The list may beat the registry insert by a hair — poll for the row.
	var task *lyonv1.ProcessInfo
	for time.Now().Before(deadline) {
		for _, p := range list().Processes {
			if p.Owner == "proc-panel-test" {
				task = p
				break
			}
		}
		if task != nil {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if task == nil {
		t.Fatal("background task never appeared in ListProcesses")
	}
	if task.Command != "sleep 30" || task.Pid <= 0 || task.LogPath == "" || task.StartedUnix <= 0 {
		t.Fatalf("row: %+v", task)
	}
	if task.SessionId != "" || task.SessionTitle != "" {
		t.Errorf("a task with no live seat must not fake a title: %+v", task)
	}

	if _, err := client.KillProcess(ctx, connect.NewRequest(&lyonv1.KillProcessRequest{Id: task.Id})); err != nil {
		t.Fatalf("KillProcess: %v", err)
	}
	// The waiter unregisters on exit — poll until the row is gone.
	for {
		absent := true
		for _, p := range list().Processes {
			if p.Owner == "proc-panel-test" {
				absent = false
			}
		}
		if absent {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("row survived the kill")
		}
		time.Sleep(30 * time.Millisecond)
	}

	// A stale id (the row is already gone) → NotFound, not a silent ok.
	_, err := client.KillProcess(ctx, connect.NewRequest(&lyonv1.KillProcessRequest{Id: 999999}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("stale kill err = %v, want NotFound", err)
	}
}
