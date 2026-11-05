package mindmap

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// scratchHome points LYON_HOME at a fresh temp dir with a retrying
// cleanup — same hardening as pkg/server's scratchHome (and pkg/eval,
// pkg/market, pkg/memory): these tests write graph files microseconds
// before teardown, and on Windows a transient handle (AV/indexer) on a
// fresh file keeps its directory entry alive past the framework's
// one-shot RemoveAll, surfacing as "directory is not empty" even when
// every assertion passed. Registered after t.TempDir, so it settles
// first (LIFO) and the framework's sweep then finds a missing path.
// Returns the dir.
func scratchHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { settleRemoveAll(dir) })
	t.Setenv("LYON_HOME", dir)
	return dir
}

// settleRemoveAll retries a doomed RemoveAll until the OS lets go of the
// files, giving up after ~2s. Log-only: a straggler must not fail a test
// that already passed its assertions.
func settleRemoveAll(dir string) {
	var err error
	for i := 0; i < 40; i++ {
		if err = os.RemoveAll(dir); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	fmt.Fprintf(os.Stderr, "settle cleanup failed for %s: %v\n", dir, err)
}

// scratch isolates one test: a fresh manager under a throwaway LYON_HOME
// (never the real ~/.lyon) and a valid seat id.
func scratch(t *testing.T) (*Manager, string) {
	t.Helper()
	scratchHome(t)
	return New(), "testseat00000001"
}

// mustUser applies an origin=user batch and returns the new revision —
// the human path every P/R/S case builds its fixture with.
func mustUser(t *testing.T, m *Manager, sid string, base int, ops ...Op) int {
	t.Helper()
	b, err := json.Marshal(ops)
	if err != nil {
		t.Fatalf("marshal ops: %v", err)
	}
	rev, _, changed, err := m.ApplyUserOps(sid, base, "user", string(b))
	if err != nil {
		t.Fatalf("ApplyUserOps(%+v): %v", ops, err)
	}
	if !changed {
		t.Fatalf("ApplyUserOps(%+v): expected a change", ops)
	}
	return rev
}

// nodeByID indexes a State dump for assertions.
func nodeByID(t *testing.T, m *Manager, sid string) map[string]Node {
	t.Helper()
	_, nodes, err := m.State(sid)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	by := make(map[string]Node, len(nodes))
	for _, n := range nodes {
		by[n.ID] = n
	}
	return by
}

// assertTree is G1's structural half: parents exist and are alive, no
// cycles, depth ≤ 4, ≤ 8 alive children per node, ≤ 60 alive total,
// kind/status/state inside their enums. Label length is checked by the
// G1 test itself (labels are never content-clipped; only output above
// the 200-rune sanity bound is cut).
func assertTree(t *testing.T, m *Manager, sid string) []Node {
	t.Helper()
	_, nodes, err := m.State(sid)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	by := make(map[string]Node, len(nodes))
	for _, n := range nodes {
		if _, dup := by[n.ID]; dup {
			t.Errorf("duplicate id %q", n.ID)
		}
		by[n.ID] = n
		switch n.Kind {
		case KindBranch, KindDecision, KindFact, KindOpenQuestion:
		default:
			t.Errorf("node %s: bad kind %q", n.ID, n.Kind)
		}
		if n.Status != StatusActive && n.Status != StatusParked {
			t.Errorf("node %s: bad status %q", n.ID, n.Status)
		}
		if n.State != StateProposed && n.State != StateConfirmed {
			t.Errorf("node %s: bad state %q", n.ID, n.State)
		}
	}
	alive := 0
	for _, n := range nodes {
		if n.Tombstone {
			continue
		}
		alive++
		if n.ParentID != "" {
			p, ok := by[n.ParentID]
			if !ok {
				t.Errorf("node %s: parent %q missing", n.ID, n.ParentID)
				continue
			}
			if p.Tombstone {
				t.Errorf("node %s: parent %q is a tombstone", n.ID, n.ParentID)
			}
		}
		// depth + cycle: walk up with a seen-set
		depth := 0
		seen := map[string]bool{}
		cur := n
		for cur.ParentID != "" {
			if seen[cur.ID] {
				t.Errorf("node %s: parent cycle", n.ID)
				break
			}
			seen[cur.ID] = true
			p, ok := by[cur.ParentID]
			if !ok {
				break
			}
			cur = p
			depth++
			if depth > maxDepth+maxNodes {
				t.Errorf("node %s: runaway parent walk", n.ID)
				break
			}
		}
		if depth > maxDepth {
			t.Errorf("node %s: depth %d > %d", n.ID, depth, maxDepth)
		}
		kids := 0
		for _, c := range nodes {
			if !c.Tombstone && c.ParentID == n.ID && c.ID != n.ID {
				kids++
			}
		}
		if kids > maxChildren {
			t.Errorf("node %s: %d alive children > %d", n.ID, kids, maxChildren)
		}
	}
	if alive > maxNodes {
		t.Errorf("alive nodes %d > %d", alive, maxNodes)
	}
	return nodes
}

func hasAudit(audits []Audit, event string) bool {
	for _, a := range audits {
		if a.Event == event {
			return true
		}
	}
	return false
}

// ── G1: structure stays legal whatever the distiller sends ────────

func TestG1StructureValid(t *testing.T) {
	m, sid := scratch(t)
	diff := Diff{Ops: []Op{
		// legal skeleton: depth 0→4
		{Op: "add", ID: "root", Label: "会话主题", Kind: "branch", SourceTurn: 1},
		{Op: "add", ID: "a", ParentID: "root", Label: "分支甲", Kind: "decision", SourceTurn: 2},
		{Op: "add", ID: "b", ParentID: "a", Label: "分支乙", Kind: "fact", SourceTurn: 3},
		{Op: "add", ID: "c", ParentID: "b", Label: "分支丙", Kind: "fact", SourceTurn: 4},
		{Op: "add", ID: "d", ParentID: "c", Label: "分支丁", Kind: "open_question", SourceTurn: 5},
		// violations — each must be dropped on arrival
		{Op: "add", ID: "e", ParentID: "d", Label: "第五层", Kind: "branch"},              // depth 5
		{Op: "add", ID: "badkind", ParentID: "root", Label: "坏枚举", Kind: "epic"},       // kind ∉ enum
		{Op: "add", ID: "orphan", ParentID: "no-such-id", Label: "孤儿", Kind: "branch"}, // parent missing
		{Op: "set_parent", ID: "a", ParentID: "d"},                                     // cycle: a is d's ancestor
		// runaway label — clipped to the 200-rune sanity bound, not dropped
		// (labels under 200 pass through whole: Image #62 「别省略」)
		{Op: "add", ID: "long", ParentID: "root", Label: strings.Repeat("长", 300), Kind: "branch"},
	}}
	res := m.ApplyDistill(sid, 0, diff)
	if !res.Changed {
		t.Fatal("legal ops must land")
	}
	nodes := assertTree(t, m, sid)
	by := make(map[string]Node, len(nodes))
	for _, n := range nodes {
		by[n.ID] = n
	}
	for _, id := range []string{"root", "a", "b", "c", "d", "long"} {
		if _, ok := by[id]; !ok {
			t.Errorf("legal node %q missing", id)
		}
	}
	for _, id := range []string{"e", "badkind", "orphan"} {
		if _, ok := by[id]; ok {
			t.Errorf("violating node %q landed", id)
		}
	}
	if by["a"].ParentID != "root" {
		t.Errorf("cycle move landed: a.parent = %q", by["a"].ParentID)
	}
	if n := len([]rune(by["long"].Label)); n > maxLabelRunes {
		t.Errorf("label not clipped: %d runes", n)
	}
	if by["d"].State != StateProposed {
		t.Errorf("distill node state = %q, want proposed", by["d"].State)
	}
}

func TestG1NodeCap(t *testing.T) {
	m, sid := scratch(t)
	var ops []Op
	for i := 0; i <= maxNodes; i++ { // 61 attempts
		id := fmt.Sprintf("a%d", i)
		ops = append(ops, Op{Op: "add", ID: id, Label: id, Kind: "branch"})
	}
	res := m.ApplyDistill(sid, 0, Diff{Ops: ops})
	if !res.Changed {
		t.Fatal("expected the first 60 to land")
	}
	by := nodeByID(t, m, sid)
	if len(by) != maxNodes {
		t.Fatalf("alive nodes = %d, want %d", len(by), maxNodes)
	}
	if _, ok := by[fmt.Sprintf("a%d", maxNodes)]; ok {
		t.Error("61st node landed past the cap")
	}
	assertTree(t, m, sid)
}

// ── G3: ids never flicker across rounds ───────────────────────────

func TestG3IDStabilityAcrossRounds(t *testing.T) {
	m, sid := scratch(t)
	r1 := m.ApplyDistill(sid, 0, Diff{Ops: []Op{
		{Op: "add", ID: "root", Label: "主题", Kind: "branch", SourceTurn: 1},
		{Op: "add", ID: "br1", ParentID: "root", Label: "存储", Kind: "decision", SourceTurn: 2},
	}})
	if !r1.Changed {
		t.Fatal("round 1 must land")
	}
	before := nodeByID(t, m, sid)

	// Round 2: transcript only appended — the diff adds, never rewrites.
	r2 := m.ApplyDistill(sid, r1.Revision, Diff{Ops: []Op{
		{Op: "add", ID: "br2", ParentID: "root", Label: "网络", Kind: "decision", SourceTurn: 5},
		{Op: "add", ID: "leaf", ParentID: "br1", Label: "缓存策略", Kind: "fact", SourceTurn: 6},
	}})
	if !r2.Changed {
		t.Fatal("round 2 must land")
	}
	after := nodeByID(t, m, sid)
	if len(after) != len(before)+2 {
		t.Fatalf("nodes = %d, want %d (additions only)", len(after), len(before)+2)
	}
	for id, n := range before {
		got, ok := after[id]
		if !ok {
			t.Errorf("node %q vanished in round 2", id)
			continue
		}
		if got != n {
			t.Errorf("node %q changed across rounds:\n  before %+v\n  after  %+v", id, n, got)
		}
	}
}

// ── G4: refuted branches park, never die ──────────────────────────

func TestG4RefutedParkedNotDeleted(t *testing.T) {
	m, sid := scratch(t)
	r1 := m.ApplyDistill(sid, 0, Diff{Ops: []Op{
		{Op: "add", ID: "root", Label: "主题", Kind: "branch"},
		{Op: "add", ID: "old", ParentID: "root", Label: "旧理解", Kind: "decision"},
	}})
	if !r1.Changed {
		t.Fatal("seed must land")
	}
	r2 := m.ApplyDistill(sid, r1.Revision, Diff{Ops: []Op{
		{Op: "park", ID: "old"},
		{Op: "rewrite", ID: "old", Label: "已被推翻"},
	}})
	if !r2.Changed {
		t.Fatal("park+rewrite must land")
	}
	n := nodeByID(t, m, sid)["old"]
	if n.Tombstone {
		t.Fatal("park must not tombstone")
	}
	if n.Status != StatusParked {
		t.Errorf("status = %q, want parked", n.Status)
	}
	if n.Label != "已被推翻" {
		t.Errorf("label = %q, want the rewritten one", n.Label)
	}
	assertTree(t, m, sid)
}

// ── P1: pinned nodes are the human's ──────────────────────────────

func TestP1PinnedViolationDropped(t *testing.T) {
	m, sid := scratch(t)
	// The human creates the node — a user add pins it (§3).
	base := mustUser(t, m, sid, 0, Op{Op: "add", ID: "x", Label: "人起的名", Kind: "branch"})

	// The next distill round tries to rename / park / move it.
	res := m.ApplyDistill(sid, base, Diff{Ops: []Op{
		{Op: "rewrite", ID: "x", Label: "模型想叫的名字"},
		{Op: "park", ID: "x"},
		{Op: "set_parent", ID: "x", ParentID: "root"},
	}})
	if res.Changed {
		t.Fatal("every pinned violation must be dropped — no change may land")
	}
	if !hasAudit(res.Audits, EventPinnedViolation) {
		t.Errorf("missing %s audit: %+v", EventPinnedViolation, res.Audits)
	}
	n := nodeByID(t, m, sid)["x"]
	if n.Label != "人起的名" {
		t.Errorf("label = %q, human's name lost", n.Label)
	}
	if n.Status != StatusActive {
		t.Errorf("status = %q, want active", n.Status)
	}
	// The human may still edit their own node.
	base2 := mustUser(t, m, sid, base, Op{Op: "rewrite", ID: "x", Label: "人改后的名"})
	if got := nodeByID(t, m, sid)["x"]; got.Label != "人改后的名" || !got.Pinned {
		t.Errorf("user rename lost: %+v", got)
	}
	if base2 != base+1 {
		t.Errorf("revision = %d, want %d", base2, base+1)
	}
}

// ── P2: tombstones stay dead across rounds ────────────────────────

func TestP2TombstoneNotRevived(t *testing.T) {
	m, sid := scratch(t)
	base := mustUser(t, m, sid, 0,
		Op{Op: "add", ID: "root", Label: "主题", Kind: "branch"},
		Op{Op: "add", ID: "victim", ParentID: "root", Label: "受害者", Kind: "fact"},
		Op{Op: "delete", ID: "victim"},
	)
	by := nodeByID(t, m, sid)
	if !by["victim"].Tombstone {
		t.Fatal("user delete must tombstone, not erase")
	}
	aliveBefore := 0
	for _, n := range by {
		if !n.Tombstone {
			aliveBefore++
		}
	}

	for round := 1; round <= 3; round++ {
		res := m.ApplyDistill(sid, base, Diff{Ops: []Op{
			{Op: "add", ID: fmt.Sprintf("rev%d", round), ParentID: "root", Label: "受害者", Kind: "fact"},
			// punctuation variant — same phrase after normalization
			{Op: "add", ID: fmt.Sprintf("revx%d", round), ParentID: "root", Label: "受害者！", Kind: "fact"},
			{Op: "rewrite", ID: "victim", Label: "借尸还魂"},
		}})
		if res.Changed {
			t.Fatalf("round %d: a tombstoned label came back", round)
		}
		by = nodeByID(t, m, sid)
		if !by["victim"].Tombstone {
			t.Fatalf("round %d: tombstone erased", round)
		}
		if by["victim"].Label != "受害者" {
			t.Fatalf("round %d: tombstone label rewritten to %q", round, by["victim"].Label)
		}
	}
	aliveAfter := 0
	for _, n := range by {
		if !n.Tombstone {
			aliveAfter++
		}
	}
	if aliveAfter != aliveBefore {
		t.Errorf("alive nodes %d → %d across revival attempts", aliveBefore, aliveAfter)
	}
}

// ── R1: a human edit in the gap discards the whole distill ────────

func TestR1RevisionConflictDiscardsBatch(t *testing.T) {
	m, sid := scratch(t)
	r1 := m.ApplyDistill(sid, 0, Diff{Ops: []Op{
		{Op: "add", ID: "root", Label: "主题", Kind: "branch"},
	}})
	if !r1.Changed {
		t.Fatal("seed must land")
	}
	// A human edit lands while the next distill is in flight.
	hrev := mustUser(t, m, sid, r1.Revision, Op{Op: "rewrite", ID: "root", Label: "人改过的主题"})

	// The in-flight diff carries the pre-human base — whole batch loses.
	r2 := m.ApplyDistill(sid, r1.Revision, Diff{Ops: []Op{
		{Op: "add", ID: "late", ParentID: "root", Label: "迟到节点", Kind: "fact"},
	}})
	if r2.Changed {
		t.Fatal("batch with a human edit in its gap must be discarded")
	}
	if !hasAudit(r2.Audits, EventDistill) {
		t.Errorf("discard must audit %s: %+v", EventDistill, r2.Audits)
	}
	by := nodeByID(t, m, sid)
	if _, ok := by["late"]; ok {
		t.Error("discarded node landed")
	}
	if by["root"].Label != "人改过的主题" {
		t.Errorf("root label = %q, human's edit lost", by["root"].Label)
	}
	rev, _, _ := m.State(sid)
	if rev != hrev {
		t.Errorf("revision = %d, want %d (discards do not tick)", rev, hrev)
	}
}

// A gap containing ONLY distiller rounds still applies — the model
// catches up on its own earlier work (§5: 顺序应用).
func TestDistillAppliesOverModelOnlyGap(t *testing.T) {
	m, sid := scratch(t)
	r1 := m.ApplyDistill(sid, 0, Diff{Ops: []Op{
		{Op: "add", ID: "root", Label: "主题", Kind: "branch"},
	}})
	r2 := m.ApplyDistill(sid, 0, Diff{Ops: []Op{ // stale base, but no human edit in between
		{Op: "add", ID: "child", ParentID: "root", Label: "子节点", Kind: "fact"},
	}})
	if !r2.Changed {
		t.Fatal("model-only gap must apply on top")
	}
	if _, ok := nodeByID(t, m, sid)["child"]; !ok {
		t.Error("child missing")
	}
	if r2.Revision != r1.Revision+1 {
		t.Errorf("revision = %d, want %d", r2.Revision, r1.Revision+1)
	}
}

// ── S1: the map survives a reload ─────────────────────────────────

func TestS1PersistenceReload(t *testing.T) {
	home := scratchHome(t)
	m1 := New()
	sid := "reloadseat000001"

	base := mustUser(t, m1, sid, 0, Op{Op: "add", ID: "root", Label: "主题", Kind: "branch"})
	// A distill round adds a proposed node (not pinned, not confirmed).
	d := m1.ApplyDistill(sid, base, Diff{Ops: []Op{
		{Op: "add", ID: "guess", ParentID: "root", Label: "模型猜的", Kind: "decision"},
	}})
	if !d.Changed {
		t.Fatal("distill seed must land")
	}
	// A human deletes another branch and confirms the guess later.
	base = mustUser(t, m1, sid, d.Revision,
		Op{Op: "add", ID: "gone", ParentID: "root", Label: "要删的", Kind: "fact"},
		Op{Op: "delete", ID: "gone"},
	)
	base = mustUser(t, m1, sid, base, Op{Op: "confirm", ID: "guess"})

	path := filepath.Join(home, "mindmap", sid+".json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("graph file missing: %v", err)
	}

	// A fresh manager — the /resume path — reads the same bytes back.
	m2 := New()
	rev2, nodes, err := m2.State(sid)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if rev2 != base {
		t.Fatalf("revision = %d, want %d (continuous)", rev2, base)
	}
	by := make(map[string]Node, len(nodes))
	for _, n := range nodes {
		by[n.ID] = n
	}
	if !by["root"].Pinned || by["root"].State != StateConfirmed {
		t.Errorf("root attributes lost: %+v", by["root"])
	}
	if by["guess"].State != StateConfirmed || by["guess"].Pinned {
		t.Errorf("confirm lost on reload: %+v", by["guess"])
	}
	if !by["gone"].Tombstone {
		t.Errorf("tombstone lost on reload: %+v", by["gone"])
	}
	// Revision keeps counting from where it left off.
	next := m2.ApplyDistill(sid, rev2, Diff{Ops: []Op{
		{Op: "add", ID: "after", ParentID: "root", Label: "重载后", Kind: "fact"},
	}})
	if !next.Changed || next.Revision != rev2+1 {
		t.Errorf("post-reload apply: changed=%v rev=%d, want +1 from %d", next.Changed, next.Revision, rev2)
	}
}

func TestCorruptGraphFileTolerated(t *testing.T) {
	home := scratchHome(t)
	sid := "corruptseat00001"
	dir := filepath.Join(home, "mindmap")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sid+".json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := New()
	rev, title, subtitle, nodes, err := m.Snapshot(sid)
	if err != nil {
		t.Fatalf("corrupt file must degrade, not error: %v", err)
	}
	if rev != 0 || nodes != "[]" || title != "" || subtitle != DefaultSubtitle {
		t.Errorf("fresh state after corrupt load: rev=%d title=%q nodes=%q", rev, title, nodes)
	}
}

// ── UpdateMindmap semantics (origin=user vs agent) ────────────────

func TestUserOpsRenameDeleteParkConfirm(t *testing.T) {
	m, sid := scratch(t)
	// Seed with the distiller so nodes start proposed and unpinned.
	seed := m.ApplyDistill(sid, 0, Diff{Ops: []Op{
		{Op: "add", ID: "root", Label: "主题", Kind: "branch"},
		{Op: "add", ID: "kid", ParentID: "root", Label: "子支", Kind: "fact"},
		{Op: "add", ID: "gc", ParentID: "kid", Label: "孙支", Kind: "fact"},
	}})
	if !seed.Changed {
		t.Fatal("seed must land")
	}
	base := seed.Revision

	// rename → pinned + confirmed + revision tick
	base = mustUser(t, m, sid, base, Op{Op: "rewrite", ID: "kid", Label: "改名后的子支"})
	if n := nodeByID(t, m, sid)["kid"]; n.Label != "改名后的子支" || !n.Pinned || !n.EditedByUser || n.State != StateConfirmed {
		t.Errorf("user rename: %+v", n)
	}

	// 取消确认: confirm state=proposed — verdict only, pin stays
	base = mustUser(t, m, sid, base, Op{Op: "confirm", ID: "kid", State: StateProposed})
	if n := nodeByID(t, m, sid)["kid"]; n.State != StateProposed || !n.Pinned {
		t.Errorf("unconfirm: %+v", n)
	}

	// park + restore
	base = mustUser(t, m, sid, base, Op{Op: "park", ID: "kid"})
	if n := nodeByID(t, m, sid)["kid"]; n.Status != StatusParked {
		t.Errorf("park: status=%q", n.Status)
	}
	base = mustUser(t, m, sid, base, Op{Op: "park", ID: "kid", Status: StatusActive})
	if n := nodeByID(t, m, sid)["kid"]; n.Status != StatusActive {
		t.Errorf("restore: status=%q", n.Status)
	}

	// user delete cascades tombstones over descendants
	base = mustUser(t, m, sid, base, Op{Op: "delete", ID: "kid"})
	by := nodeByID(t, m, sid)
	if !by["kid"].Tombstone || !by["gc"].Tombstone {
		t.Errorf("cascade delete: kid=%+v gc=%+v", by["kid"], by["gc"])
	}
	if !by["gc"].Pinned {
		t.Error("cascade must pin every tombstoned descendant")
	}

	// confirm on a still-proposed distill node: a verdict, not a content
	// edit — state flips, pin does not (unconfirm must stay possible).
	base = mustUser(t, m, sid, base, Op{Op: "confirm", ID: "root"})
	if n := nodeByID(t, m, sid)["root"]; n.State != StateConfirmed || n.Pinned {
		t.Errorf("confirm: %+v", n)
	}
	assertTree(t, m, sid)
	if base <= seed.Revision {
		t.Errorf("revision never advanced: %d", base)
	}
}

func TestAgentOriginCannotTouchPinned(t *testing.T) {
	m, sid := scratch(t)
	// Human renames the root — frozen from then on for non-user origins.
	base := mustUser(t, m, sid, 0, Op{Op: "add", ID: "root", Label: "人的根", Kind: "branch"})

	agentOps := func(ops ...Op) error {
		b, err := json.Marshal(ops)
		if err != nil {
			t.Fatal(err)
		}
		_, _, _, err = m.ApplyUserOps(sid, base, "agent", string(b))
		return err
	}
	if err := agentOps(Op{Op: "rewrite", ID: "root", Label: "agent 改名"}); !errors.Is(err, ErrFrozen) {
		t.Errorf("agent rename of pinned: err=%v, want ErrFrozen", err)
	}
	if err := agentOps(Op{Op: "park", ID: "root"}); !errors.Is(err, ErrFrozen) {
		t.Errorf("agent park of pinned: err=%v, want ErrFrozen", err)
	}
	if err := agentOps(Op{Op: "delete", ID: "root"}); !errors.Is(err, ErrFrozen) {
		t.Errorf("agent delete of pinned: err=%v, want ErrFrozen", err)
	}
	if err := agentOps(Op{Op: "confirm", ID: "root"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("agent confirm: err=%v, want ErrInvalid (user-only)", err)
	}
	// §7.3: agent adds land proposed and unpinned — the human confirms.
	b, _ := json.Marshal([]Op{{Op: "add", ID: "fresh", ParentID: "root", Label: "agent 提的", Kind: "fact"}})
	rev, _, changed, err := m.ApplyUserOps(sid, base, "agent", string(b))
	if err != nil || !changed {
		t.Fatalf("agent add: changed=%v err=%v", changed, err)
	}
	if n := nodeByID(t, m, sid)["fresh"]; n.State != StateProposed || n.Pinned {
		t.Errorf("agent add must land proposed/unpinned: %+v", n)
	}
	if rev != base+1 {
		t.Errorf("revision = %d, want %d", rev, base+1)
	}
	// The human's root is untouched by all of the above.
	if n := nodeByID(t, m, sid)["root"]; n.Label != "人的根" {
		t.Errorf("root label = %q", n.Label)
	}
}

func TestUpdateMindmapOptimisticLock(t *testing.T) {
	m, sid := scratch(t)
	base := mustUser(t, m, sid, 0, Op{Op: "add", ID: "root", Label: "主题", Kind: "branch"})
	_ = base // base is now 1

	// Stale base after a human edit → ErrConflict (人工优先).
	if _, _, _, err := m.ApplyUserOps(sid, 0, "user", `[{"op":"rewrite","id":"root","label":"x"}]`); !errors.Is(err, ErrConflict) {
		t.Errorf("stale base: err=%v, want ErrConflict", err)
	}
	// Client ahead of the server → also a conflict, never a silent rewind.
	if _, _, _, err := m.ApplyUserOps(sid, 99, "user", `[{"op":"rewrite","id":"root","label":"x"}]`); !errors.Is(err, ErrConflict) {
		t.Errorf("ahead base: err=%v, want ErrConflict", err)
	}
	// Malformed ops → InvalidArgument material.
	if _, _, _, err := m.ApplyUserOps(sid, 1, "user", `{not-an-array`); !errors.Is(err, ErrInvalid) {
		t.Errorf("malformed ops: err=%v, want ErrInvalid", err)
	}
	// Unknown op inside an otherwise valid batch → whole batch rejected.
	if _, _, _, err := m.ApplyUserOps(sid, 1, "user", `[{"op":"explode","id":"root"}]`); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown op: err=%v, want ErrInvalid", err)
	}
	// A no-op batch (same-label rewrite) does not bump the revision.
	rev, _, changed, err := m.ApplyUserOps(sid, 1, "user", `[{"op":"rewrite","id":"root","label":"主题"}]`)
	if err != nil || changed {
		t.Fatalf("no-op batch: changed=%v err=%v", changed, err)
	}
	if rev != 1 {
		t.Errorf("revision = %d, want 1", rev)
	}
}

// ── label similarity: the server-side merge net (§6) ──────────────

func TestDistillSimilarLabelMergedNotAdded(t *testing.T) {
	m, sid := scratch(t)
	seed := m.ApplyDistill(sid, 0, Diff{Ops: []Op{
		{Op: "add", ID: "root", Label: "存储与持久化", Kind: "branch"},
	}})
	if !seed.Changed {
		t.Fatal("seed must land")
	}
	// Near-identical phrase (punctuation + spacing) must not fork the branch.
	res := m.ApplyDistill(sid, seed.Revision, Diff{Ops: []Op{
		{Op: "add", ID: "b-dup", ParentID: "root", Label: "存储 与持久化！", Kind: "branch"},
	}})
	if res.Changed {
		t.Errorf("similar label landed as a new node: %+v", res)
	}
	by := nodeByID(t, m, sid)
	if len(by) != 1 {
		t.Errorf("nodes = %d, want 1 (merged away)", len(by))
	}
}

// ── §5 re-entrancy: one distill per seat at a time (C1) ───────────

func TestDistillReentrancyGate(t *testing.T) {
	m := New()
	if !m.beginDistill("seat-a") {
		t.Fatal("first claim must succeed")
	}
	if m.beginDistill("seat-a") {
		t.Fatal("second claim on the same seat must fail")
	}
	if !m.beginDistill("seat-b") {
		t.Fatal("other seats are independent")
	}
	m.endDistill("seat-a")
	if !m.beginDistill("seat-a") {
		t.Fatal("claim must succeed after end")
	}
}

// ── M3: relate op — 跨分支连线 (§9) ─────────────────────────────

func TestRelateOp(t *testing.T) {
	m, sid := scratch(t)
	base := mustUser(t, m, sid, 0,
		Op{Op: "add", ID: "root", Label: "主题", Kind: KindBranch},
		Op{Op: "add", ID: "br1", ParentID: "root", Label: "甲支", Kind: KindBranch},
		Op{Op: "add", ID: "br2", ParentID: "root", Label: "乙支", Kind: KindBranch},
		Op{Op: "add", ID: "a", ParentID: "br1", Label: "节点甲", Kind: KindFact},
		Op{Op: "add", ID: "b", ParentID: "br2", Label: "节点乙", Kind: KindFact},
		Op{Op: "add", ID: "gone", ParentID: "br2", Label: "将删", Kind: KindFact},
		Op{Op: "delete", ID: "gone"},
	)

	// Set: user origin on a confirmed node (rewrite-class authority —
	// user may, and it pins like every other human content edit).
	base = mustUser(t, m, sid, base, Op{
		Op: "relate", ID: "a",
		Relation: &Relation{Type: "contradicts", Target: "b"},
	})
	if rel := nodeByID(t, m, sid)["a"].Relation; rel == nil ||
		rel.Type != "contradicts" || rel.Target != "b" {
		t.Fatalf("relation not set: %+v", rel)
	}

	// JSON round-trip through Snapshot's nodes_json (the web's read path).
	_, _, _, nodesJSON, err := m.Snapshot(sid)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	var wire []Node
	if err := json.Unmarshal([]byte(nodesJSON), &wire); err != nil {
		t.Fatalf("nodes_json: %v", err)
	}
	found := false
	for _, n := range wire {
		if n.ID == "a" {
			found = true
			if n.Relation == nil || n.Relation.Type != "contradicts" || n.Relation.Target != "b" {
				t.Errorf("relation lost in nodes_json: %+v", n.Relation)
			}
		}
	}
	if !found {
		t.Fatal("node a missing from nodes_json")
	}

	// Target validation: missing node, self, tombstone, missing type —
	// each an InvalidArgument-grade rejection, map untouched.
	relateErr := func(auth string, o Op) error {
		b, err := json.Marshal([]Op{o})
		if err != nil {
			t.Fatal(err)
		}
		_, _, _, err = m.ApplyUserOps(sid, base, auth, string(b))
		return err
	}
	for _, bad := range []Op{
		{Op: "relate", ID: "a", Relation: &Relation{Type: "related", Target: "nope"}},
		{Op: "relate", ID: "a", Relation: &Relation{Type: "related", Target: "a"}},
		{Op: "relate", ID: "a", Relation: &Relation{Type: "related", Target: "gone"}},
		{Op: "relate", ID: "a", Relation: &Relation{Target: "b"}}, // type required
	} {
		if err := relateErr("user", bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("relate %+v: err=%v, want ErrInvalid", bad.Relation, err)
		}
	}

	// Agent origin: frozen class — a confirmed node is the human's.
	if err := relateErr("agent", Op{
		Op: "relate", ID: "a",
		Relation: &Relation{Type: "related", Target: "b"},
	}); !errors.Is(err, ErrFrozen) {
		t.Errorf("agent relate on confirmed: err=%v, want ErrFrozen", err)
	}
	// Agent origin on a proposed node: allowed, stays proposed/unpinned.
	d := m.ApplyDistill(sid, base, Diff{Ops: []Op{
		{Op: "add", ID: "d", ParentID: "br1", Label: "提案节点", Kind: KindFact},
	}})
	if !d.Changed {
		t.Fatal("distill seed must land")
	}
	b, _ := json.Marshal([]Op{{Op: "relate", ID: "d", Relation: &Relation{Type: "related", Target: "b"}}})
	rev, _, changed, err := m.ApplyUserOps(sid, d.Revision, "agent", string(b))
	if err != nil || !changed {
		t.Fatalf("agent relate on proposed: changed=%v err=%v", changed, err)
	}
	if n := nodeByID(t, m, sid)["d"]; n.Relation == nil || n.Pinned ||
		n.State != StateProposed {
		t.Errorf("agent relate must not pin: %+v", n)
	}
	if rev != d.Revision+1 {
		t.Errorf("revision = %d, want %d", rev, d.Revision+1)
	}
	base = rev

	// Clear: absent relation removes the edge; a second clear is a no-op
	// (revision must not tick on an already-clear node).
	base = mustUser(t, m, sid, base, Op{Op: "relate", ID: "a"})
	if rel := nodeByID(t, m, sid)["a"].Relation; rel != nil {
		t.Errorf("relation not cleared: %+v", rel)
	}
	rev, _, changed, err = m.ApplyUserOps(sid, base, "user", `[{"op":"relate","id":"a"}]`)
	if err != nil || changed {
		t.Errorf("clear of clear node: changed=%v err=%v, want no-op", changed, err)
	}
	if rev != base {
		t.Errorf("revision = %d, want %d (no-op)", rev, base)
	}
	// An identical edge (d already carries it from the agent batch) is
	// also a no-op.
	rev, _, changed, err = m.ApplyUserOps(sid, base, "user",
		`[{"op":"relate","id":"d","relation":{"type":"related","target":"b"}}]`)
	if err != nil || changed {
		t.Errorf("identical relate: changed=%v err=%v, want no-op", changed, err)
	}
	if rev != base {
		t.Errorf("revision = %d, want %d (no-op)", rev, base)
	}
}
