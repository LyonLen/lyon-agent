package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	lyonv1 "lyon-agent/gen/go/lyon/v1"
	"lyon-agent/gen/go/lyon/v1/lyonv1connect"
	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/mindmap"
	"lyon-agent/pkg/tools"
)

// newMindmapFixture serves a real server with one freshly created seat —
// every test gets its own id, so the process-wide mindmap.Open() graph
// table never bleeds between cases.
func newMindmapFixture(t *testing.T) (lyonv1connect.MindmapServiceClient, lyonv1connect.SessionServiceClient, string, *Server) {
	t.Helper()
	scratchHome(t)
	eng := agent.NewEngine(agent.DefaultConfig(), nil)
	st := NewStore(0, 0)
	t.Cleanup(st.CloseAll)
	s := New(eng, st)
	s.SetObserver(tools.NewMetrics())
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	sessCli := lyonv1connect.NewSessionServiceClient(http.DefaultClient, ts.URL)
	mmCli := lyonv1connect.NewMindmapServiceClient(http.DefaultClient, ts.URL)
	cres, err := sessCli.CreateSession(context.Background(), connect.NewRequest(&lyonv1.CreateSessionRequest{}))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return mmCli, sessCli, cres.Msg.Session.Id, s
}

func getMindmap(t *testing.T, cli lyonv1connect.MindmapServiceClient, sid string) *lyonv1.GetMindmapResponse {
	t.Helper()
	res, err := cli.GetMindmap(context.Background(), connect.NewRequest(&lyonv1.GetMindmapRequest{SessionId: sid}))
	if err != nil {
		t.Fatalf("GetMindmap: %v", err)
	}
	return res.Msg
}

func updateMindmap(t *testing.T, cli lyonv1connect.MindmapServiceClient, sid string, rev int32, origin, ops string) *lyonv1.UpdateMindmapResponse {
	t.Helper()
	res, err := cli.UpdateMindmap(context.Background(), connect.NewRequest(&lyonv1.UpdateMindmapRequest{
		SessionId: sid, Revision: rev, Origin: origin, OpsJson: ops,
	}))
	if err != nil {
		t.Fatalf("UpdateMindmap(origin=%s, ops=%s): %v", origin, ops, err)
	}
	return res.Msg
}

func updateErr(t *testing.T, cli lyonv1connect.MindmapServiceClient, sid string, rev int32, origin, ops string) error {
	t.Helper()
	_, err := cli.UpdateMindmap(context.Background(), connect.NewRequest(&lyonv1.UpdateMindmapRequest{
		SessionId: sid, Revision: rev, Origin: origin, OpsJson: ops,
	}))
	if err == nil {
		t.Fatalf("UpdateMindmap(origin=%s, ops=%s): want error, got none", origin, ops)
	}
	return err
}

// nodeField pulls one node's field out of a nodes_json payload — string
// contains on raw JSON would couple assertions to marshal field order.
func nodeField(t *testing.T, nodesJSON, id, field string) any {
	t.Helper()
	var arr []map[string]any
	if err := json.Unmarshal([]byte(nodesJSON), &arr); err != nil {
		t.Fatalf("nodes_json: %v\n%s", err, nodesJSON)
	}
	for _, n := range arr {
		if n["id"] == id {
			return n[field]
		}
	}
	t.Fatalf("node %q missing from %s", id, nodesJSON)
	return nil
}

// auditText reads today's audit JSONL from the scratch home.
func auditText(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(os.Getenv("LYON_HOME"),
		"audit", time.Now().Format("2006-01-02")+".jsonl"))
	if err != nil {
		return ""
	}
	return string(b)
}

// TestMindmapUpdateRoundTrip: origin=user rename/delete/park/confirm all
// take effect with a revision increment, and GetMindmap reads back the
// same state (/resume contract). The batch audits mindmap.update.
func TestMindmapUpdateRoundTrip(t *testing.T) {
	mm, _, sid, s := newMindmapFixture(t)

	// Fresh seat: revision 0, "[]", the §4.3 default subtitle — a valid
	// answer, not an error.
	fresh := getMindmap(t, mm, sid)
	if fresh.Revision != 0 || fresh.NodesJson != "[]" {
		t.Errorf("fresh: rev=%d nodes=%q", fresh.Revision, fresh.NodesJson)
	}
	if fresh.Subtitle != mindmap.DefaultSubtitle {
		t.Errorf("fresh subtitle = %q", fresh.Subtitle)
	}

	// user add → pinned (人改过), revision 1.
	r := updateMindmap(t, mm, sid, 0, "user",
		`[{"op":"add","id":"root","label":"主题","kind":"branch"}]`)
	if r.Revision != 1 {
		t.Fatalf("after add: rev=%d, want 1", r.Revision)
	}
	if got := nodeField(t, r.NodesJson, "root", "pinned"); got != true {
		t.Errorf("user add pinned = %v, want true", got)
	}

	// user rename → label + revision 2.
	r = updateMindmap(t, mm, sid, 1, "user",
		`[{"op":"rewrite","id":"root","label":"改名后的主题"}]`)
	if r.Revision != 2 {
		t.Fatalf("rename: rev=%d, want 2", r.Revision)
	}
	if got := nodeField(t, r.NodesJson, "root", "label"); got != "改名后的主题" {
		t.Errorf("label = %v", got)
	}

	// An agent add lands proposed (not pinned); user confirm flips the
	// verdict; park + restore cycle; delete tombstones.
	r = updateMindmap(t, mm, sid, 2, "agent",
		`[{"op":"add","id":"kid","parent_id":"root","label":"子支","kind":"fact"}]`)
	if r.Revision != 3 {
		t.Fatalf("agent add: rev=%d, want 3", r.Revision)
	}
	if got := nodeField(t, r.NodesJson, "kid", "state"); got != "proposed" {
		t.Errorf("agent add state = %v, want proposed", got)
	}
	if got := nodeField(t, r.NodesJson, "kid", "pinned"); got != false {
		t.Errorf("agent add pinned = %v, want false", got)
	}

	r = updateMindmap(t, mm, sid, 3, "user", `[{"op":"confirm","id":"kid"}]`)
	if r.Revision != 4 || nodeField(t, r.NodesJson, "kid", "state") != "confirmed" {
		t.Errorf("confirm: rev=%d state=%v", r.Revision, nodeField(t, r.NodesJson, "kid", "state"))
	}
	r = updateMindmap(t, mm, sid, 4, "user", `[{"op":"park","id":"kid"}]`)
	if r.Revision != 5 || nodeField(t, r.NodesJson, "kid", "status") != "parked" {
		t.Errorf("park: rev=%d status=%v", r.Revision, nodeField(t, r.NodesJson, "kid", "status"))
	}
	r = updateMindmap(t, mm, sid, 5, "user", `[{"op":"park","id":"kid","status":"active"}]`)
	if r.Revision != 6 || nodeField(t, r.NodesJson, "kid", "status") != "active" {
		t.Errorf("restore: rev=%d status=%v", r.Revision, nodeField(t, r.NodesJson, "kid", "status"))
	}
	r = updateMindmap(t, mm, sid, 6, "user", `[{"op":"delete","id":"kid"}]`)
	if r.Revision != 7 || nodeField(t, r.NodesJson, "kid", "tombstone") != true {
		t.Errorf("delete: rev=%d tombstone=%v", r.Revision, nodeField(t, r.NodesJson, "kid", "tombstone"))
	}

	// /resume read path: GetMindmap shows the same map.
	got := getMindmap(t, mm, sid)
	if got.Revision != 7 {
		t.Errorf("GetMindmap rev=%d, want 7 (continuous)", got.Revision)
	}
	if label := nodeField(t, got.NodesJson, "root", "label"); label != "改名后的主题" {
		t.Errorf("GetMindmap root label = %v", label)
	}

	// The batch audited mindmap.update — a plain-string event name, not a
	// pkg/hooks constant (that file is not ours to grow).
	s.eng.Hooks().Flush()
	if audit := auditText(t); !strings.Contains(audit, `"event":"mindmap.update"`) {
		t.Errorf("mindmap.update not audited:\n%s", audit)
	}
}

// TestMindmapStaleRevisionConflict: a base whose gap holds a human edit
// → FailedPrecondition (人工优先), and the map is untouched.
func TestMindmapStaleRevisionConflict(t *testing.T) {
	mm, _, sid, _ := newMindmapFixture(t)
	updateMindmap(t, mm, sid, 0, "user", `[{"op":"add","id":"root","label":"主题","kind":"branch"}]`)

	err := updateErr(t, mm, sid, 0, "user", `[{"op":"rewrite","id":"root","label":"stale"}]`)
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("stale base: code=%v, want FailedPrecondition (%v)", connect.CodeOf(err), err)
	}
	if label := nodeField(t, getMindmap(t, mm, sid).NodesJson, "root", "label"); label != "主题" {
		t.Errorf("conflicted batch landed: label=%v", label)
	}
	// Client ahead of the server is also a conflict, never a rewind.
	err = updateErr(t, mm, sid, 99, "user", `[{"op":"rewrite","id":"root","label":"ahead"}]`)
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("ahead base: code=%v, want FailedPrecondition", connect.CodeOf(err))
	}
}

// TestMindmapAgentCannotTouchPinned: §4.1 over the wire — a non-user
// origin reaching a pinned node is FailedPrecondition; its own adds still
// land proposed.
func TestMindmapAgentCannotTouchPinned(t *testing.T) {
	mm, _, sid, _ := newMindmapFixture(t)
	updateMindmap(t, mm, sid, 0, "user", `[{"op":"add","id":"root","label":"人的根","kind":"branch"}]`)

	for _, ops := range []string{
		`[{"op":"rewrite","id":"root","label":"agent 改名"}]`,
		`[{"op":"park","id":"root"}]`,
		`[{"op":"delete","id":"root"}]`,
		`[{"op":"set_parent","id":"root","parent_id":""}]`,
	} {
		err := updateErr(t, mm, sid, 1, "agent", ops)
		if connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Errorf("agent %s: code=%v, want FailedPrecondition (%v)", ops, connect.CodeOf(err), err)
		}
	}
	// confirm is origin=user only → InvalidArgument, not a silent land.
	err := updateErr(t, mm, sid, 1, "agent", `[{"op":"confirm","id":"root"}]`)
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("agent confirm: code=%v, want InvalidArgument", connect.CodeOf(err))
	}
	// The human's node is intact and the revision never moved.
	got := getMindmap(t, mm, sid)
	if got.Revision != 1 {
		t.Errorf("revision moved despite rejects: %d", got.Revision)
	}
	if label := nodeField(t, got.NodesJson, "root", "label"); label != "人的根" {
		t.Errorf("pinned node touched: label=%v", label)
	}

	// §7.3: agent adds land proposed + unpinned; the human confirms later.
	r := updateMindmap(t, mm, sid, 1, "agent",
		`[{"op":"add","id":"fresh","parent_id":"root","label":"agent 提的","kind":"fact"}]`)
	if r.Revision != 2 {
		t.Fatalf("agent add: rev=%d, want 2", r.Revision)
	}
	if got := nodeField(t, r.NodesJson, "fresh", "state"); got != "proposed" {
		t.Errorf("agent add state = %v, want proposed", got)
	}
	if got := nodeField(t, r.NodesJson, "fresh", "pinned"); got != false {
		t.Errorf("agent add pinned = %v, want false", got)
	}
}

// TestMindmapEditEventInHistory: a user batch injects EVENT_MINDMAP_EDIT
// onto the hub, and the event carries no executable intent (§4.2 — the
// security boundary, not an implementation detail).
func TestMindmapEditEventInHistory(t *testing.T) {
	mm, sessCli, sid, s := newMindmapFixture(t)
	updateMindmap(t, mm, sid, 0, "user", `[{"op":"add","id":"root","label":"主题","kind":"branch"}]`)

	h, err := s.store.Hub(sid)
	if err != nil {
		t.Fatalf("Hub: %v", err)
	}
	ev := findEvent(h, mindmap.TypeEdit)
	if ev == nil {
		t.Fatal("mindmap_edit never injected")
	}
	if got := protoEventType(ev.Type); got != lyonv1.EventType_EVENT_TYPE_MINDMAP_EDIT {
		t.Errorf("proto type = %v, want MINDMAP_EDIT", got)
	}
	if ev.Attrs["origin"] != "user" || ev.Attrs["revision"] != "1" || ev.Attrs["ops"] == "" {
		t.Errorf("attrs = %+v", ev.Attrs)
	}
	if ev.ToolName != "" || len(ev.Input) > 0 {
		t.Errorf("executable intent on the map→chat channel: tool=%q input=%q", ev.ToolName, ev.Input)
	}

	// The wire shape: type_name authoritative, enum stamped.
	hist, err := sessCli.GetHistory(context.Background(), connect.NewRequest(&lyonv1.GetHistoryRequest{SessionId: sid}))
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	var found bool
	for _, se := range hist.Msg.Events {
		if se.TypeName == "mindmap_edit" {
			found = true
			if se.Type != lyonv1.EventType_EVENT_TYPE_MINDMAP_EDIT {
				t.Errorf("wire type = %v, want MINDMAP_EDIT", se.Type)
			}
			if se.Attrs["origin"] != "user" {
				t.Errorf("wire attrs = %v", se.Attrs)
			}
		}
	}
	if !found {
		t.Error("mindmap_edit missing from GetHistory")
	}
}

// TestMindmapInvalidOps: malformed or structurally impossible batches →
// InvalidArgument, and nothing half-applies.
func TestMindmapInvalidOps(t *testing.T) {
	mm, _, sid, _ := newMindmapFixture(t)
	updateMindmap(t, mm, sid, 0, "user", `[{"op":"add","id":"root","label":"主题","kind":"branch"}]`)

	cases := []string{
		`{not-an-array`,                  // malformed JSON
		`[{"op":"explode","id":"root"}]`, // unknown op — whole batch rejected
		`[{"op":"add","id":"x","parent_id":"nope","label":"孤儿"}]`, // missing parent
		`[{"op":"add","id":"root","label":"dup"}]`,                // duplicate id
	}
	for _, ops := range cases {
		err := updateErr(t, mm, sid, 1, "user", ops)
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("ops %s: code=%v, want InvalidArgument (%v)", ops, connect.CodeOf(err), err)
		}
	}
	got := getMindmap(t, mm, sid)
	if got.Revision != 1 {
		t.Errorf("rejected batches moved the revision: %d", got.Revision)
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(got.NodesJson), &arr); err != nil || len(arr) != 1 {
		t.Errorf("rejected batches mutated the map: %s", got.NodesJson)
	}

	// Unknown seat → NotFound (not a silent empty map).
	_, err := mm.GetMindmap(context.Background(), connect.NewRequest(&lyonv1.GetMindmapRequest{
		SessionId: "deadbeefdeadbeef",
	}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("unknown seat: code=%v, want NotFound", connect.CodeOf(err))
	}
}

// TestPublishMindmapInjectsUpdate: the distiller seam — an applied diff
// becomes EVENT_MINDMAP_UPDATE on a live seat's hub, and publishing to a
// parked seat never adopts it back into memory.
func TestPublishMindmapInjectsUpdate(t *testing.T) {
	_, _, sid, s := newMindmapFixture(t)
	mindmap.Open().SetPublisher(s.PublishMindmap)
	t.Cleanup(func() { mindmap.Open().SetPublisher(nil) })

	res := mindmap.Open().ApplyDistill(sid, 0, mindmap.Diff{Ops: []mindmap.Op{
		{Op: "add", ID: "root", Label: "主题", Kind: "branch"},
	}})
	if !res.Changed {
		t.Fatal("distill seed must land")
	}
	h, err := s.store.Hub(sid)
	if err != nil {
		t.Fatalf("Hub: %v", err)
	}
	ev := findEvent(h, mindmap.TypeUpdate)
	if ev == nil {
		t.Fatal("mindmap_update never injected")
	}
	if ev.Attrs["revision"] != "1" || !strings.Contains(ev.Attrs["nodes"], "root") {
		t.Errorf("attrs = %+v", ev.Attrs)
	}
	if got := protoEventType(ev.Type); got != lyonv1.EventType_EVENT_TYPE_MINDMAP_UPDATE {
		t.Errorf("proto type = %v, want MINDMAP_UPDATE", got)
	}

	// Parked seat: the publish must be a no-op — no ring, no adoption.
	if !s.store.Park(sid) {
		t.Fatal("Park failed")
	}
	mindmap.Open().ApplyDistill(sid, 1, mindmap.Diff{Ops: []mindmap.Op{
		{Op: "add", ID: "kid", ParentID: "root", Label: "子支", Kind: "fact"},
	}})
	if _, err := s.store.Hub(sid); err == nil {
		t.Error("publishing to a parked seat adopted it into memory")
	}
}
