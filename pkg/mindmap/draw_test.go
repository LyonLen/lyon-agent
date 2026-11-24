package mindmap

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"lyon-agent/pkg/agent"
)

// The event-diagram seam (§14): ApplyDraw commits positioned shapes +
// arrows as an agent-authority write (proposed, no human-revision tick),
// place/edge ops ride UpdateMindmap, and both halves persist.

const drawSpecJSON = `{
  "title": "芯片生产系统接手",
  "nodes": [
    {"id": "cloud-a", "label": "云平台-阿里云片区", "shape": "rect", "x": 40, "y": 120},
    {"id": "sys-a", "label": "云平台系统（武汉分院）", "shape": "circle", "x": 320, "y": 60},
    {"id": "note-1", "label": "1 迁移部署到智通的云资源\n2 需要迁移费用", "shape": "note", "x": 640, "y": 40}
  ],
  "edges": [
    {"from": "cloud-a", "to": "sys-a", "label": "1 托管\n2 分院维护", "style": "dashed"},
    {"from": "sys-a", "to": "note-1", "label": "专线上云"}
  ]
}`

func TestApplyDraw(t *testing.T) {
	m, sid := scratch(t)
	base, err := m.ApplyDraw(sid, drawSpecJSON)
	if err != nil {
		t.Fatalf("ApplyDraw: %v", err)
	}
	if !base.Changed || base.Revision != 1 {
		t.Fatalf("first draw: changed=%v rev=%d, want true/1", base.Changed, base.Revision)
	}

	rev, title, _, nodes, err := m.Snapshot(sid)
	if err != nil || rev != 1 {
		t.Fatalf("snapshot: rev=%d err=%v", rev, err)
	}
	if title != "芯片生产系统接手" {
		t.Errorf("title = %q", title)
	}
	var got []Node
	if err := json.Unmarshal([]byte(nodes), &got); err != nil || len(got) != 3 {
		t.Fatalf("nodes = %s (%v)", nodes, err)
	}
	byID := map[string]Node{}
	for _, n := range got {
		byID[n.ID] = n
	}
	sys := byID["sys-a"]
	if sys.Shape != "circle" || sys.X != 320 || sys.Y != 60 {
		t.Errorf("placement lost: %+v", sys)
	}
	if sys.State != StateProposed {
		t.Errorf("drawn node state = %q, want proposed (§4.3 honesty)", sys.State)
	}
	if sys.Pinned {
		t.Error("drawn node must not pin — the model is not the human")
	}
	note := byID["note-1"]
	if !strings.Contains(note.Label, "\n") {
		t.Errorf("multi-line label flattened: %q", note.Label)
	}

	// Edges ride their own JSON — endpoints, numbered label, dashed style.
	edges := m.EdgesJSON(sid)
	var gotEdges []Edge
	if err := json.Unmarshal([]byte(edges), &gotEdges); err != nil || len(gotEdges) != 2 {
		t.Fatalf("edges = %s (%v)", edges, err)
	}
	e0 := gotEdges[0]
	if e0.From != "cloud-a" || e0.To != "sys-a" || e0.Style != "dashed" || !strings.Contains(e0.Label, "1 托管") {
		t.Errorf("edge[0] = %+v", e0)
	}
	if gotEdges[1].Style != "solid" {
		t.Errorf("default style = %q, want solid", gotEdges[1].Style)
	}

	// The draw is an AGENT write: HumanRevision must not tick — a later
	// distiller round with the pre-draw base applies on top (§5: only a
	// HUMAN edit discards a batch).
	res := m.ApplyDistill(sid, 0, Diff{Ops: []Op{
		{Op: "add", ID: "after-draw", Label: "后续节点", Kind: "branch"},
	}})
	if !res.Changed {
		t.Fatalf("distill after draw discarded (human-revision ticked?): audits=%+v", res.Audits)
	}
}

func TestApplyDrawUpsert(t *testing.T) {
	m, sid := scratch(t)
	if _, err := m.ApplyDraw(sid, drawSpecJSON); err != nil {
		t.Fatalf("first draw: %v", err)
	}
	spec := `{"nodes":[
		{"id": "sys-a", "label": "云平台系统（改名了）", "shape": "rect", "x": 500, "y": 90},
		{"id": "fresh", "label": "新图形", "shape": "diamond", "x": 900, "y": 90}
	],"edges":[{"from": "sys-a", "to": "fresh", "label": "新连线"}]}`
	res, err := m.ApplyDraw(sid, spec)
	if err != nil {
		t.Fatalf("re-draw: %v", err)
	}
	if !res.Changed {
		t.Fatal("re-draw reported no change")
	}
	_, _, _, nodes, _ := m.Snapshot(sid)
	var got []Node
	_ = json.Unmarshal([]byte(nodes), &got)
	if len(got) != 4 {
		t.Fatalf("node count = %d, want 4 (3 + fresh, sys-a updated not duplicated)", len(got))
	}
	for _, n := range got {
		if n.ID == "sys-a" {
			if n.Label != "云平台系统（改名了）" || n.X != 500 {
				t.Errorf("upsert did not refresh: %+v", n)
			}
		}
	}
	// The auto edge id (from-to) upserts: still exactly the drawn arrows.
	var es []Edge
	_ = json.Unmarshal([]byte(m.EdgesJSON(sid)), &es)
	if len(es) != 3 {
		t.Errorf("edge count = %d, want 3 (auto-id upsert must not duplicate)", len(es))
	}
}

func TestApplyDrawRejects(t *testing.T) {
	m, sid := scratch(t)
	cases := []struct {
		name string
		spec string
		want string
	}{
		{"no nodes", `{"nodes":[]}`, "no nodes"},
		{"labelless node", `{"nodes":[{"x":1,"y":2}]}`, "without label"},
		{"dead edge endpoint", `{"nodes":[{"id":"a","label":"A","x":0,"y":0}],"edges":[{"from":"a","to":"ghost"}]}`, "edge to"},
		{"self edge", `{"nodes":[{"id":"a","label":"A","x":0,"y":0}],"edges":[{"from":"a","to":"a"}]}`, "itself"},
		{"bad style", `{"nodes":[{"id":"a","label":"A","x":0,"y":0},{"id":"b","label":"B","x":9,"y":9}],"edges":[{"from":"a","to":"b","style":"wavy"}]}`, "style"},
	}
	for _, c := range cases {
		if _, err := m.ApplyDraw(sid, c.spec); err == nil || !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want ErrInvalid containing %q", c.name, err, c.want)
		}
	}
	// A rejected batch leaves NOTHING behind (D1: never half a diagram).
	if _, _, _, nodes, _ := m.Snapshot(sid); nodes != "[]" {
		t.Errorf("rejected draws mutated the map: %s", nodes)
	}
}

func TestPlaceAndEdgeOps(t *testing.T) {
	m, sid := scratch(t)
	if _, err := m.ApplyDraw(sid, drawSpecJSON); err != nil {
		t.Fatalf("seed draw: %v", err)
	}
	// place: a human drag moves the shape and pins it (§4.1 — the touch
	// is the human's).
	rev := mustUser(t, m, sid, 1,
		Op{Op: "place", ID: "sys-a", X: 400, Y: 200},
	)
	_, _, _, nodes, _ := m.Snapshot(sid)
	var got []Node
	_ = json.Unmarshal([]byte(nodes), &got)
	for _, n := range got {
		if n.ID == "sys-a" {
			if n.X != 400 || n.Y != 200 {
				t.Errorf("place lost coords: %+v", n)
			}
			if !n.Pinned || n.State != StateConfirmed {
				t.Errorf("user place must pin/confirm: pinned=%v state=%q", n.Pinned, n.State)
			}
		}
	}
	if rev != 2 {
		t.Errorf("revision = %d, want 2", rev)
	}

	// place rejects malformed coordinates (D1 — nothing half-applies).
	if err := userOpsErr(m, sid, 2, Op{Op: "place", ID: "sys-a", X: 1e9, Y: 0}); err == nil {
		t.Error("out-of-range place accepted")
	}

	// edge: upsert by id, then delete by id (to == "").
	mustUser(t, m, sid, 2,
		Op{Op: "edge", ID: "manual", From: "sys-a", To: "note-1", Label: "手工连线", Style: "dashed"},
	)
	var es []Edge
	_ = json.Unmarshal([]byte(m.EdgesJSON(sid)), &es)
	if len(es) != 3 {
		t.Fatalf("edge count after add = %d, want 3", len(es))
	}
	mustUser(t, m, sid, 3, Op{Op: "edge", ID: "manual", To: ""})
	_ = json.Unmarshal([]byte(m.EdgesJSON(sid)), &es)
	if len(es) != 2 {
		t.Errorf("edge count after delete = %d, want 2", len(es))
	}
}

// userOpsErr applies an origin=user batch and returns its error (nil on
// success) without failing the test on rejection.
func userOpsErr(m *Manager, sid string, base int, ops ...Op) error {
	b, err := json.Marshal(ops)
	if err != nil {
		return err
	}
	_, _, _, err = m.ApplyUserOps(sid, base, "user", string(b))
	return err
}

func TestDrawPublishesEdges(t *testing.T) {
	m, sid := scratch(t)
	var got map[string]string
	m.SetPublisher(func(_ string, ev agent.Event) { got = ev.Attrs })
	if _, err := m.ApplyDraw(sid, drawSpecJSON); err != nil {
		t.Fatalf("ApplyDraw: %v", err)
	}
	if got == nil {
		t.Fatal("publisher not called")
	}
	if !strings.Contains(got["edges"], "cloud-a") || got["nodes"] == "" || got["revision"] != "1" {
		t.Errorf("event attrs = %+v", got)
	}
}
