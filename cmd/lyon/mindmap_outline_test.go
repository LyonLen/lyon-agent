package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/mindmap"
)

// seedOutlineMap builds the U1 fixture through pkg/mindmap's own write
// path — ApplyUserOps origin=user, the mustUser pattern from
// pkg/mindmap/mindmap_test.go: a root, children across two depths, one
// parked leaf and one tombstoned node. LYON_HOME is redirected at a
// scratch dir first (and the sid is minted per call) so nothing lands in
// the developer's real ~/.lyon and a -count=2 rerun never meets the
// singleton's cached state.
func seedOutlineMap(t *testing.T) (sid string) {
	t.Helper()
	t.Setenv("LYON_HOME", tempDir(t))
	sid = fmt.Sprintf("mmoutline%d", time.Now().UnixNano())
	ops := []mindmap.Op{
		{Op: "add", ID: "root", Label: "root topic", Kind: mindmap.KindBranch, SourceTurn: 1},
		{Op: "add", ID: "a", ParentID: "root", Label: "decision a", Kind: mindmap.KindDecision, SourceTurn: 2},
		{Op: "add", ID: "a1", ParentID: "a", Label: "fact a1", Kind: mindmap.KindFact, SourceTurn: 3},
		{Op: "add", ID: "b", ParentID: "root", Label: "question b", Kind: mindmap.KindOpenQuestion, SourceTurn: 4},
		{Op: "add", ID: "bp", ParentID: "b", Label: "parked leaf", Kind: mindmap.KindBranch, SourceTurn: 5},
		{Op: "add", ID: "gone", ParentID: "root", Label: "gone node", Kind: mindmap.KindBranch, SourceTurn: 6},
		{Op: "delete", ID: "gone"}, // tombstone — U1: not a row
		{Op: "park", ID: "bp"},     // parked — the ⏸ marker row
	}
	b, err := json.Marshal(ops)
	if err != nil {
		t.Fatalf("marshal ops: %v", err)
	}
	rev, _, changed, err := mindmap.Open().ApplyUserOps(sid, 0, "user", string(b))
	if err != nil {
		t.Fatalf("ApplyUserOps seed: %v", err)
	}
	if !changed {
		t.Fatalf("ApplyUserOps seed: no change (rev %d)", rev)
	}
	return sid
}

// TestMindmapOutlineU1 is U1's TUI half: 大纲行数 = 节点数（剔除
// tombstone）, with the §7.2 shape — 2-space indent per depth, ├─/└─
// sibling glyphs, web kind icons, t<source_turn> and the ⏸ parked
// marker — plus LYON_MINDMAP=off yielding nothing at all.
func TestMindmapOutlineU1(t *testing.T) {
	t.Setenv("LYON_MINDMAP", "on")
	sid := seedOutlineMap(t)

	rows := mindmapOutlineRows(sid, 60)
	want := []string{
		"▸ root topic t1",
		"  ├─ ◆ decision a t2",
		"    └─ ● fact a1 t3",
		"  └─ ○ question b t4",
		"    └─ ▸ parked leaf t5 ⏸",
	}
	if len(rows) != len(want) {
		t.Fatalf("outline rows = %d, want %d (alive node count; tombstone excluded):\n%q",
			len(rows), len(want), rows)
	}
	for i, r := range want {
		if rows[i] != r {
			t.Errorf("row %d = %q, want %q", i, rows[i], r)
		}
	}
	if joined := strings.Join(rows, "\n"); strings.Contains(joined, "gone node") {
		t.Fatalf("tombstoned node leaked into the outline:\n%s", joined)
	}

	// LYON_MINDMAP=off: the builder renders nothing (D2's gate), so the
	// panel has no outline block to steal rows for.
	t.Setenv("LYON_MINDMAP", "off")
	if got := mindmapOutlineRows(sid, 60); got != nil {
		t.Fatalf("off must yield nil outline, got %q", got)
	}
}

// TestRenderStepsMindmapOutline pins renderSteps' contract around the
// outline (M3 §7.2): still exactly h rows of at most w cells, with the
// outline block claiming the panel's bottom rows — root first, deepest
// leaf last — and yielding the whole panel back when the map is off.
func TestRenderStepsMindmapOutline(t *testing.T) {
	t.Setenv("LYON_MINDMAP", "on")
	m := newTestModel(t)
	sid := seedOutlineMap(t)
	m.sess.SetSeat(sid, time.Now())
	// One real step so the outline demonstrably steals from the list.
	m.applyEvent(agent.Event{Type: agent.EventToolUseStart, ToolID: "t1", ToolName: "read_file"})

	const w, h = 40, 20
	rows := m.renderSteps(w, h)
	if len(rows) != h {
		t.Fatalf("rows = %d, want %d", len(rows), h)
	}
	for i, r := range rows {
		if n := ansi.StringWidth(r); n > w {
			t.Fatalf("row %d is %d cells: %q", i, n, r)
		}
	}
	// The seeded map has 5 alive nodes → the last 5 rows are the outline,
	// root at the top of the block, parked leaf at the panel bottom.
	if !strings.Contains(rows[0], "steps") {
		t.Fatalf("header row: %q", rows[0])
	}
	if !strings.Contains(rows[1], "read_file") {
		t.Fatalf("step row under the header: %q", rows[1])
	}
	if !strings.Contains(rows[h-5], "root topic") {
		t.Fatalf("outline root at panel bottom: %q", rows[h-5])
	}
	if !strings.HasPrefix(rows[h-4], "  ├─ ") {
		t.Fatalf("depth-1 glyph+indent: %q", rows[h-4])
	}
	if !strings.Contains(rows[h-1], "parked leaf") || !strings.Contains(rows[h-1], "⏸") {
		t.Fatalf("parked leaf marker on the last row: %q", rows[h-1])
	}
	if joined := strings.Join(rows, "\n"); strings.Contains(joined, "gone node") {
		t.Fatalf("tombstoned node leaked into renderSteps:\n%s", joined)
	}

	// Map off: no outline block, same row-count contract.
	t.Setenv("LYON_MINDMAP", "off")
	off := m.renderSteps(w, h)
	if len(off) != h {
		t.Fatalf("off rows = %d, want %d", len(off), h)
	}
	if strings.Contains(strings.Join(off, "\n"), "root topic") {
		t.Fatalf("off must render no outline: %q", off)
	}
}
