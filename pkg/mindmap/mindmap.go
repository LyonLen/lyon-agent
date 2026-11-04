// Package mindmap is the server side of the live conversation mind-map
// (docs/design/mindmap.md): one tree per session, persisted under
// $LYON_HOME/mindmap/<session_id>.json, mutated by exactly two writers —
// the prompt_end distiller (a cheap-model diff, §2/§5/§6) and humans
// through UpdateMindmap (§7.3 map_ops).
//
// Three rules are load-bearing, not policy suggestions:
//
//   - Authority (§4.1): a pinned or confirmed node is the human's — the
//     distiller and non-user origins may only add children beneath it,
//     never rewrite / park / move / merge it. Violations are dropped
//     op-by-op and audited as mindmap.pinned_violation.
//   - Tombstones (§3): a human delete keeps the node as a negative
//     signal; same-label nodes never revive across later rounds.
//   - Revision (§5): every change bumps revision; a writer carrying a
//     base revision whose gap contains a HUMAN edit loses the race and
//     discards its whole batch (人工优先). A gap that only contains
//     distiller rounds applies on top, op re-validated.
//
// Failure degrades to silence: a bad reply, a lost race or a dropped op
// never leaves half a node on the map (D1). Nothing here calls an LLM —
// distill.go owns that half; this file is state, rules and disk.
package mindmap

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode"

	"lyon-agent/pkg/agent"
)

// Event names on the wire (agent.EventType) — the hub stamps these, the
// web card keys off type_name/proto enum (event.proto 17/18).
const (
	// TypeUpdate is the distiller's structure diff (§3: 结构变化).
	TypeUpdate = agent.EventType("mindmap_update")
	// TypeEdit is a human's map action — the map → chat input channel
	// (§3: 人的编辑动作). It carries NO executable intent (§4.2): at
	// most it becomes one chat message, never a tool call.
	TypeEdit = agent.EventType("mindmap_edit")
)

// Audit event names — plain strings, deliberately NOT constants in
// pkg/hooks (that file is not ours to grow; hooks.Manager.Fire takes any
// event name).
const (
	EventDistill         = "mindmap.distill"          // one distill attempt: applied / skipped / discarded
	EventPinnedViolation = "mindmap.pinned_violation" // §4.1: distiller touched a human-owned node
	EventUpdate          = "mindmap.update"           // UpdateMindmap batch applied
)

// DefaultSubtitle is shown until the distiller writes its own (§4.3:
// the card reads as "lyon's current understanding, correct it if wrong",
// never as a conclusion).
const DefaultSubtitle = "lyon 目前对这次讨论的理解 · 有错请你改"

// Node kinds / statuses / states — the §3 enums.
const (
	KindBranch       = "branch"
	KindDecision     = "decision"
	KindFact         = "fact"
	KindOpenQuestion = "open_question"

	StatusActive = "active"
	StatusParked = "parked"

	StateProposed  = "proposed"  // 模型猜的 (dashed)
	StateConfirmed = "confirmed" // 你认定的 (solid)
)

// The §6 hats — distiller output beyond these is dropped on arrival.
const (
	maxDepth          = 4   // root depth 0
	maxNodes          = 60  // alive (non-tombstone) nodes
	maxChildren       = 8   // alive children per branch
	maxLabelRunes     = 24  // distiller/agent label cap (§6)
	maxUserLabelRunes = 200 // human rename sanity bound (human is authoritative below this)
)

// Sentinel errors — mapped onto connect codes by pkg/server:
// ErrConflict / ErrFrozen → FailedPrecondition, ErrInvalid → InvalidArgument.
var (
	// ErrConflict is §5's optimistic lock: the base revision is stale
	// AND a human edit landed inside the gap (or the client is ahead).
	ErrConflict = errors.New("mindmap: revision conflict — a human edit landed after that base")
	// ErrFrozen is §4.1/§7.3: a non-user origin tried to modify a
	// pinned/confirmed node. origin=user never produces it.
	ErrFrozen = errors.New("mindmap: node is user-owned (pinned/confirmed)")
	// ErrInvalid is a malformed or structurally impossible op.
	ErrInvalid = errors.New("mindmap: invalid op")
)

// Audit is one record destined for hooks.Manager.Fire — event name as a
// plain string (see the const block above).
type Audit struct {
	Event     string
	Detail    string
	SessionID string
	IsError   bool
	Err       string
}

// Node is the §3 node shape — the exact JSON that rides nodes_json and
// EVENT_MINDMAP_UPDATE's attrs["nodes"]. sample.js uses the shorthand
// keys (parent/turn); the wire keys are the §3 names.
type Node struct {
	ID           string `json:"id"`
	ParentID     string `json:"parent_id"`
	Label        string `json:"label"`
	SourceTurn   int    `json:"source_turn"`
	Kind         string `json:"kind"`
	Status       string `json:"status"`
	Pinned       bool   `json:"pinned"`
	EditedByUser bool   `json:"edited_by_user"`
	Tombstone    bool   `json:"tombstone"`
	State        string `json:"state"`
}

// Op is one entry of ops_json — the §7.3 map_ops set (merge /
// set_parent / rewrite / add / park / delete / confirm) plus the
// distiller's use of the same shapes. Field sets differ per op:
//
//	add         id(new|empty→slug), parent_id, label, kind, source_turn
//	rewrite     id, label
//	park        id, status(active|parked, default parked — 恢复 uses active)
//	set_parent  id, parent_id ("" = top level)
//	delete      id            (user/agent: tombstone cascade; distiller: parks)
//	confirm     id, state(confirmed|proposed; default confirmed — proposed = 取消确认)
//	merge       id(survivor), source_ids (absorbed → children move up, source tombstoned)
type Op struct {
	Op         string   `json:"op"`
	ID         string   `json:"id"`
	Label      string   `json:"label,omitempty"`
	ParentID   string   `json:"parent_id,omitempty"`
	Kind       string   `json:"kind,omitempty"`
	Status     string   `json:"status,omitempty"`
	State      string   `json:"state,omitempty"`
	SourceTurn int      `json:"source_turn,omitempty"`
	SourceIDs  []string `json:"source_ids,omitempty"`
}

// Diff is one distiller reply (§6: a diff, never the full tree).
type Diff struct {
	Title    string `json:"title,omitempty"`
	Subtitle string `json:"subtitle,omitempty"`
	Ops      []Op   `json:"ops"`
}

// state is the on-disk document ($LYON_HOME/mindmap/<sid>.json).
type state struct {
	Version int `json:"version"`
	// Revision bumps on EVERY accepted change (distiller or human) —
	// the §5 optimistic-lock clock.
	Revision int `json:"revision"`
	// HumanRevision is the revision at which the last human edit landed
	// (0 = none). A writer whose base < current loses only when
	// HumanRevision > base: that gap contains a human edit (人工优先).
	HumanRevision int    `json:"human_revision"`
	Title         string `json:"title,omitempty"`
	Subtitle      string `json:"subtitle,omitempty"`
	Nodes         []Node `json:"nodes"`
}

func (g *state) clone() *state {
	c := *g
	c.Nodes = append([]Node(nil), g.Nodes...)
	return &c
}

// Manager holds every live session's map. One process-wide default
// (Open) serves lyond; tests build fresh ones (New) so a t.Setenv
// LYON_HOME isolates them. The directory is resolved per operation from
// the environment — never cached — so a scratch home set after New still
// takes effect.
type Manager struct {
	mu       sync.Mutex
	graphs   map[string]*state
	inflight map[string]bool // distill re-entrancy guard, one pass per seat
	publish  func(sessionID string, ev agent.Event)
}

// New builds an empty manager (tests, embedders).
func New() *Manager {
	return &Manager{
		graphs:   map[string]*state{},
		inflight: map[string]bool{},
	}
}

var defaultManager = New()

// Open is the process-wide manager (lyond wiring).
func Open() *Manager { return defaultManager }

// SetPublisher installs the sink for distill-produced events — lyond
// wires it to the server's hub inject (pkg/server/mindmap.go). Nil (the
// default) means updates apply to state but go nowhere on the wire.
func (m *Manager) SetPublisher(fn func(sessionID string, ev agent.Event)) {
	m.mu.Lock()
	m.publish = fn
	m.mu.Unlock()
}

func (m *Manager) publisher() func(string, agent.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.publish
}

// Off reports whether LYON_MINDMAP opts out wholesale (§8): no distill,
// no events, no LLM calls (D2).
func Off() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LYON_MINDMAP"))) {
	case "off", "0", "false", "no":
		return true
	}
	return false
}

// homeDir mirrors pkg/hooks/pkg/memory: $LYON_HOME, else ~/.lyon.
func homeDir() string {
	if h := os.Getenv("LYON_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".lyon")
}

// validSeatID keeps sid inside a filename: seat ids are hex16, but the
// guard makes path traversal impossible regardless of caller.
func validSeatID(sid string) bool {
	if sid == "" || len(sid) > 128 {
		return false
	}
	for _, r := range sid {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

func graphPath(sid string) (string, error) {
	if !validSeatID(sid) {
		return "", fmt.Errorf("%w: bad session id %q", ErrInvalid, sid)
	}
	return filepath.Join(homeDir(), "mindmap", sid+".json"), nil
}

// writeFileAtomic is temp + rename — a crash mid-write leaves the old
// file, never a truncated one (same pattern as pkg/agent's drafts).
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// loadLocked returns the seat's map, loading it from disk on first
// touch. A missing file is a fresh empty map; an unreadable/corrupt one
// degrades the same way (启动加载容错 — the next save repairs it).
// Caller holds m.mu.
func (m *Manager) loadLocked(sid string) (*state, error) {
	if !validSeatID(sid) {
		return nil, fmt.Errorf("%w: bad session id %q", ErrInvalid, sid)
	}
	if g, ok := m.graphs[sid]; ok {
		return g, nil
	}
	g := &state{Version: 1, Nodes: []Node{}}
	if path, err := graphPath(sid); err == nil {
		if b, rerr := os.ReadFile(path); rerr == nil {
			var disk state
			if json.Unmarshal(b, &disk) == nil && disk.Version >= 1 && disk.Nodes != nil {
				if disk.HumanRevision > disk.Revision {
					disk.HumanRevision = disk.Revision
				}
				g = &disk
			}
		}
	}
	m.graphs[sid] = g
	return g, nil
}

// persistLocked rewrites the seat's file (best-effort: the in-memory
// state is this process's truth; the next change retries the write).
// Caller holds m.mu.
func (m *Manager) persistLocked(sid string, g *state) error {
	path, err := graphPath(sid)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b)
}

func subtitleOf(g *state) string {
	if g.Subtitle == "" {
		return DefaultSubtitle
	}
	return g.Subtitle
}

func nodesJSON(g *state) string {
	if len(g.Nodes) == 0 {
		return "[]"
	}
	b, err := json.Marshal(g.Nodes)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// Snapshot is GetMindmap's read side: revision, title, subtitle and the
// whole node array as JSON. A seat with no map yet answers revision 0
// with "[]" — not an error.
func (m *Manager) Snapshot(sid string) (rev int, title, subtitle, nodes string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, err := m.loadLocked(sid)
	if err != nil {
		return 0, "", "", "", err
	}
	return g.Revision, g.Title, subtitleOf(g), nodesJSON(g), nil
}

// State is a copy of the map for inspection (tests, debugging).
func (m *Manager) State(sid string) (rev int, nodes []Node, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, err := m.loadLocked(sid)
	if err != nil {
		return 0, nil, err
	}
	return g.Revision, append([]Node(nil), g.Nodes...), nil
}

// Inventory renders the node list for the distiller prompt: id + label
// per the §2 input contract, plus the markers that keep the model off
// human-owned rows and away from tombstones. Returns the base revision
// the next ApplyDistill must carry (§5).
func (m *Manager) Inventory(sid string) (int, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, err := m.loadLocked(sid)
	if err != nil {
		return 0, "", err
	}
	var b strings.Builder
	b.WriteString("nodes (id | parent_id | label | kind | status | state | pinned):\n")
	alive := 0
	for _, n := range g.Nodes {
		if n.Tombstone {
			continue
		}
		alive++
		pin := "no"
		if n.Pinned {
			pin = "yes"
		}
		fmt.Fprintf(&b, "%s | %s | %s | %s | %s | %s | %s\n",
			n.ID, n.ParentID, n.Label, n.Kind, n.Status, n.State, pin)
	}
	if alive == 0 {
		b.WriteString("(empty — seed with a root: {\"op\":\"add\",\"id\":\"root\",\"parent_id\":\"\",\"label\":\"…\",\"kind\":\"branch\",\"source_turn\":1})\n")
	}
	var dead []string
	for _, n := range g.Nodes {
		if n.Tombstone {
			dead = append(dead, n.Label)
		}
	}
	if len(dead) > 0 {
		b.WriteString("deleted labels (do NOT revive in any form): " + strings.Join(dead, "; ") + "\n")
	}
	return g.Revision, b.String(), nil
}

// checkBase is §5's optimistic lock, shared by both writers. The gap
// (base, current] may contain distiller rounds — those apply on top —
// but any human edit inside the gap (HumanRevision > base) makes the
// caller lose.
func checkBase(base, current, human int) error {
	switch {
	case base < 0:
		return fmt.Errorf("%w: negative base revision %d", ErrInvalid, base)
	case base == current:
		return nil
	case base < current:
		if human > base {
			return fmt.Errorf("%w: base %d, current %d, last human edit at %d", ErrConflict, base, current, human)
		}
		return nil
	default: // base > current: client ahead of the server — treat as a race
		return fmt.Errorf("%w: base %d ahead of current %d", ErrConflict, base, current)
	}
}

// ApplyUserOps applies one UpdateMindmap batch atomically (§7.3): every
// op must succeed or the whole batch is rejected and the map untouched.
// origin == "user" is the human on the card — full authority, pins what
// it touches. Any other origin is agent map_ops: §7.3's proposed-landing
// rules apply (never modify pinned/confirmed → ErrFrozen).
// Returns the new revision, the new nodes JSON, and whether anything
// actually changed (an empty or no-op batch does not bump revision).
func (m *Manager) ApplyUserOps(sid string, base int, origin, opsJSON string) (int, string, bool, error) {
	auth := authAgent
	if origin == "user" {
		auth = authUser
	}
	ops, err := parseOps(opsJSON)
	if err != nil {
		return 0, "", false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	g, err := m.loadLocked(sid)
	if err != nil {
		return 0, "", false, err
	}
	if err := checkBase(base, g.Revision, g.HumanRevision); err != nil {
		return 0, "", false, err
	}
	if len(ops) == 0 {
		return g.Revision, nodesJSON(g), false, nil
	}
	work := g.clone()
	for i, o := range ops {
		if err := applyOp(work, o, auth); err != nil {
			return 0, "", false, fmt.Errorf("op[%d] %s %q: %w", i, o.Op, o.ID, err)
		}
	}
	if reflect.DeepEqual(work, g) {
		return g.Revision, nodesJSON(g), false, nil // every op was a no-op
	}
	work.Revision++
	work.HumanRevision = work.Revision // this batch IS the human edit
	m.graphs[sid] = work
	_ = m.persistLocked(sid, work)
	return work.Revision, nodesJSON(work), true, nil
}

// ApplyResult is one distiller round's outcome — what the caller needs
// to build EVENT_MINDMAP_UPDATE plus the audit lines to fire.
type ApplyResult struct {
	Changed   bool
	Revision  int
	Title     string
	Subtitle  string
	NodesJSON string
	Audits    []Audit
}

// ApplyDistill commits one distiller diff under §5: if the base
// revision's gap contains a human edit, the WHOLE batch is discarded
// (R1, 人工优先); otherwise each op is re-validated against the current
// state — frozen targets drop with a mindmap.pinned_violation audit
// (P1), structural violations drop silently (counted), tombstone
// revivals drop (P2). A round that lands nothing does not bump the
// revision (D1-adjacent: no half nodes, no phantom ticks).
//
// A diff that lands also fires the wire seam: EVENT_MINDMAP_UPDATE
// through the publisher lyond installed (server hub inject). Publishing
// happens after the state lock drops so the sink can take its own.
func (m *Manager) ApplyDistill(sid string, base int, diff Diff) ApplyResult {
	res := m.applyDistillLocked(sid, base, diff)
	if !res.Changed {
		return res
	}
	if pub := m.publisher(); pub != nil {
		pub(sid, agent.Event{
			Type: TypeUpdate,
			Attrs: map[string]string{
				"revision": fmt.Sprintf("%d", res.Revision),
				"title":    res.Title,
				"subtitle": res.Subtitle,
				"nodes":    res.NodesJSON,
			},
		})
	}
	return res
}

// applyDistillLocked is ApplyDistill's state half — the lock lives here
// so the publish above runs outside it.
func (m *Manager) applyDistillLocked(sid string, base int, diff Diff) ApplyResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, err := m.loadLocked(sid)
	if err != nil {
		return ApplyResult{Audits: []Audit{{
			Event: EventDistill, SessionID: sid, IsError: true,
			Err: err.Error(), Detail: "distill skipped: " + err.Error(),
		}}}
	}
	res := ApplyResult{
		Revision:  g.Revision,
		Title:     g.Title,
		Subtitle:  subtitleOf(g),
		NodesJSON: nodesJSON(g),
	}
	if err := checkBase(base, g.Revision, g.HumanRevision); err != nil {
		res.Audits = append(res.Audits, Audit{
			Event: EventDistill, SessionID: sid,
			Detail: fmt.Sprintf("discarded whole diff: %v (人工优先)", err),
		})
		return res
	}

	work := g.clone()
	var audits []Audit
	applied, dropped, violations := 0, 0, 0
	for _, o := range diff.Ops {
		if err := applyOp(work, o, authDistill); err != nil {
			dropped++
			if errors.Is(err, ErrFrozen) {
				violations++
				audits = append(audits, Audit{
					Event: EventPinnedViolation, SessionID: sid,
					Detail: fmt.Sprintf("dropped distill op %s %q: %v", o.Op, o.ID, err),
				})
			}
			continue
		}
		applied++
	}
	changed := applied > 0
	if t := strings.TrimSpace(diff.Title); t != "" && t != work.Title {
		work.Title = clipRunes(t, 120)
		changed = true
	}
	if s := strings.TrimSpace(diff.Subtitle); s != "" && s != work.Subtitle {
		work.Subtitle = clipRunes(s, 160)
		changed = true
	}
	if !changed {
		res.Audits = append(audits, Audit{
			Event: EventDistill, SessionID: sid,
			Detail: fmt.Sprintf("no change: %d op(s) applied, %d dropped (%d pinned violations)", applied, dropped, violations),
		})
		return res
	}
	work.Revision++
	m.graphs[sid] = work
	_ = m.persistLocked(sid, work)
	res.Changed = true
	res.Revision = work.Revision
	res.Title = work.Title
	res.Subtitle = subtitleOf(work)
	res.NodesJSON = nodesJSON(work)
	audits = append(audits, Audit{
		Event: EventDistill, SessionID: sid,
		Detail: fmt.Sprintf("applied %d op(s), dropped %d (%d pinned violations), revision %d→%d",
			applied, dropped, violations, g.Revision, work.Revision),
	})
	res.Audits = audits
	return res
}

// beginDistill claims the seat's single distill slot — one LLM call per
// seat at a time (C1's per-round budget; a second prompt_end arriving
// mid-flight skips its round rather than queueing calls).
func (m *Manager) beginDistill(sid string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.inflight[sid] {
		return false
	}
	m.inflight[sid] = true
	return true
}

func (m *Manager) endDistill(sid string) {
	m.mu.Lock()
	delete(m.inflight, sid)
	m.mu.Unlock()
}

// ── op application ────────────────────────────────────────────────

// authority decides which rules an op plays under.
type authority int

const (
	authUser    authority = iota // origin == "user": human on the card, full authority
	authAgent                    // UpdateMindmap with any other origin (§7.3 map_ops)
	authDistill                  // the prompt_end distiller (§6)
)

func parseOps(s string) ([]Op, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var ops []Op
	if err := json.Unmarshal([]byte(s), &ops); err != nil {
		return nil, fmt.Errorf("%w: ops_json: %v", ErrInvalid, err)
	}
	return ops, nil
}

// applyOp routes one op to its handler. Every error wraps a sentinel so
// pkg/server can map it onto a connect code with errors.Is.
func applyOp(g *state, o Op, auth authority) error {
	switch o.Op {
	case "add":
		return applyAdd(g, o, auth)
	case "rewrite":
		return applyRewrite(g, o, auth)
	case "park":
		return applyPark(g, o, auth)
	case "set_parent":
		return applySetParent(g, o, auth)
	case "delete":
		return applyDelete(g, o, auth)
	case "confirm":
		return applyConfirm(g, o, auth)
	case "merge":
		return applyMerge(g, o, auth)
	default:
		return fmt.Errorf("%w: unknown op %q", ErrInvalid, o.Op)
	}
}

func nodeBy(g *state, id string) *Node {
	for i := range g.Nodes {
		if g.Nodes[i].ID == id {
			return &g.Nodes[i]
		}
	}
	return nil
}

func aliveCount(g *state) int {
	n := 0
	for i := range g.Nodes {
		if !g.Nodes[i].Tombstone {
			n++
		}
	}
	return n
}

func aliveChildren(g *state, parent string) int {
	n := 0
	for i := range g.Nodes {
		c := &g.Nodes[i]
		if !c.Tombstone && c.ParentID == parent && c.ID != parent {
			n++
		}
	}
	return n
}

// depthOf is a node's distance from the root (root = 0), with a cycle
// guard so a corrupt graph can't hang the walk.
func depthOf(g *state, id string) int {
	d := 0
	seen := map[string]bool{}
	cur := nodeBy(g, id)
	for cur != nil && cur.ParentID != "" && !seen[cur.ID] {
		seen[cur.ID] = true
		d++
		cur = nodeBy(g, cur.ParentID)
		if d > maxDepth+maxNodes {
			break
		}
	}
	return d
}

// subtreeHeight is the number of edges to the deepest alive descendant.
func subtreeHeight(g *state, id string) int {
	level := []string{id}
	h := 0
	for len(level) > 0 && h <= maxDepth+2 {
		var next []string
		for _, pid := range level {
			for i := range g.Nodes {
				c := &g.Nodes[i]
				if !c.Tombstone && c.ParentID == pid && c.ID != pid {
					next = append(next, c.ID)
				}
			}
		}
		if len(next) == 0 {
			break
		}
		h++
		level = next
	}
	return h
}

// isDescendant reports whether id sits inside the subtree rooted at
// ancestor (walks up from id).
func isDescendant(g *state, id, ancestor string) bool {
	seen := map[string]bool{}
	cur := nodeBy(g, id)
	for cur != nil && !seen[cur.ID] {
		if cur.ID == ancestor {
			return true
		}
		seen[cur.ID] = true
		if cur.ParentID == "" {
			break
		}
		cur = nodeBy(g, cur.ParentID)
	}
	return false
}

// frozen is §4.1's human-owned test: pinned OR confirmed. Tombstones are
// checked separately (a deleted node is simply not a valid target).
func frozen(n *Node) bool { return n.Pinned || n.State == StateConfirmed }

// pin marks a node as the human's — content edits by origin=user always
// pin (§3: 人改过 → true), which also freezes it for later distills.
func pin(n *Node) {
	n.Pinned = true
	n.EditedByUser = true
	n.State = StateConfirmed
}

func validKind(k string) bool {
	switch k {
	case KindBranch, KindDecision, KindFact, KindOpenQuestion:
		return true
	}
	return false
}

// requireTarget resolves an existing, non-deleted node.
func requireTarget(g *state, id string) (*Node, error) {
	if id == "" {
		return nil, fmt.Errorf("%w: missing id", ErrInvalid)
	}
	n := nodeBy(g, id)
	if n == nil {
		return nil, fmt.Errorf("%w: node %q not found", ErrInvalid, id)
	}
	if n.Tombstone {
		return nil, fmt.Errorf("%w: node %q is deleted (tombstone)", ErrInvalid, id)
	}
	return n, nil
}

// requireModifiable is requireTarget plus §4.1: non-user origins may not
// touch pinned/confirmed nodes (origin=user never hits this branch).
func requireModifiable(g *state, id string, auth authority) (*Node, error) {
	n, err := requireTarget(g, id)
	if err != nil {
		return nil, err
	}
	if auth != authUser && frozen(n) {
		return nil, fmt.Errorf("%w: node %q", ErrFrozen, id)
	}
	return n, nil
}

func validID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for i, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case (r == '-' || r == '_') && i > 0:
		default:
			return false
		}
	}
	return true
}

// slugID derives a stable b-<ascii> id from a label (§6: new nodes use
// slugs). Non-ASCII drops out; an all-CJK label collapses to "b-node".
func slugID(label string) string {
	var b strings.Builder
	b.WriteString("b-")
	dash := false
	for _, r := range strings.ToLower(label) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			if dash && b.Len() > 2 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		default:
			dash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" || s == "b" {
		return "b-node"
	}
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

// resolveID picks the id a new node gets: the model's slug if free and
// well-formed, else a sanitized/collision-suffixed variant.
func resolveID(g *state, want, label string) string {
	id := want
	if !validID(id) {
		id = slugID(label)
		if want != "" {
			if s := sanitizeID(want); validID(s) {
				id = s
			}
		}
	}
	if nodeBy(g, id) == nil {
		return id
	}
	for i := 2; i < 1000; i++ {
		cand := fmt.Sprintf("%s-%d", id, i)
		if nodeBy(g, cand) == nil {
			return cand
		}
	}
	return fmt.Sprintf("%s-x%d", id, time.Now().UnixNano()%100000)
}

// sanitizeID force-fits an arbitrary string into the id alphabet.
func sanitizeID(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		case r == '-' || r == '_':
			dash = true
			if b.Len() == 0 {
				continue
			}
		default:
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// clipRunes shortens s to at most n runes (label caps are in runes —
// CJK is one rune per 字).
func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// ── individual ops ────────────────────────────────────────────────

func applyAdd(g *state, o Op, auth authority) error {
	label := strings.TrimSpace(o.Label)
	if label == "" {
		return fmt.Errorf("%w: add without label", ErrInvalid)
	}
	kind := o.Kind
	if kind == "" {
		kind = KindBranch
	}
	if !validKind(kind) {
		return fmt.Errorf("%w: bad kind %q", ErrInvalid, kind)
	}
	parent := o.ParentID
	if parent != "" {
		if _, err := requireTarget(g, parent); err != nil {
			return fmt.Errorf("%w: add parent: %v", ErrInvalid, err)
		}
	}

	id := o.ID
	switch auth {
	case authDistill:
		// §6 hats + P1/P2 admission, enforced server-side whatever the
		// prompt says.
		label = clipRunes(label, maxLabelRunes)
		for i := range g.Nodes {
			n := &g.Nodes[i]
			if !n.Tombstone && similar(n.Label, label) {
				return fmt.Errorf("%w: label matches existing node %q", ErrInvalid, n.ID)
			}
		}
		for i := range g.Nodes {
			n := &g.Nodes[i]
			if n.Tombstone && similar(n.Label, label) {
				return fmt.Errorf("%w: label matches deleted node %q (tombstone)", ErrInvalid, n.ID)
			}
		}
		if nodeBy(g, id) != nil {
			return fmt.Errorf("%w: id %q already exists", ErrInvalid, id)
		}
		id = resolveID(g, id, label)
		if aliveCount(g) >= maxNodes {
			return fmt.Errorf("%w: node cap %d", ErrInvalid, maxNodes)
		}
		if parent != "" && aliveChildren(g, parent) >= maxChildren {
			return fmt.Errorf("%w: child cap %d under %q", ErrInvalid, maxChildren, parent)
		}
		depth := 0
		if parent != "" {
			depth = depthOf(g, parent) + 1
		}
		if depth > maxDepth {
			return fmt.Errorf("%w: depth cap %d", ErrInvalid, maxDepth)
		}
	case authAgent:
		// §7.3 agent map_ops: same hats as the distiller, but no
		// similarity merge (the human asked for this exact node).
		label = clipRunes(label, maxLabelRunes)
		if nodeBy(g, id) != nil {
			return fmt.Errorf("%w: id %q already exists", ErrInvalid, id)
		}
		id = resolveID(g, id, label)
		if aliveCount(g) >= maxNodes {
			return fmt.Errorf("%w: node cap %d", ErrInvalid, maxNodes)
		}
		if parent != "" && aliveChildren(g, parent) >= maxChildren {
			return fmt.Errorf("%w: child cap %d under %q", ErrInvalid, maxChildren, parent)
		}
		depth := 0
		if parent != "" {
			depth = depthOf(g, parent) + 1
		}
		if depth > maxDepth {
			return fmt.Errorf("%w: depth cap %d", ErrInvalid, maxDepth)
		}
	default: // authUser — human is authoritative; only sanity bounds
		if r := []rune(label); len(r) > maxUserLabelRunes {
			return fmt.Errorf("%w: label longer than %d runes", ErrInvalid, maxUserLabelRunes)
		}
		if id == "" {
			id = slugID(label)
		}
		if !validID(id) {
			return fmt.Errorf("%w: bad id %q", ErrInvalid, id)
		}
		if nodeBy(g, id) != nil {
			return fmt.Errorf("%w: id %q already exists", ErrInvalid, id)
		}
	}

	n := Node{
		ID:         id,
		ParentID:   parent,
		Label:      label,
		SourceTurn: o.SourceTurn,
		Kind:       kind,
		Status:     StatusActive,
	}
	if auth == authUser {
		pin(&n)
	} else {
		n.State = StateProposed
	}
	g.Nodes = append(g.Nodes, n)
	return nil
}

func applyRewrite(g *state, o Op, auth authority) error {
	n, err := requireModifiable(g, o.ID, auth)
	if err != nil {
		return err
	}
	label := strings.TrimSpace(o.Label)
	if label == "" {
		return fmt.Errorf("%w: rewrite without label", ErrInvalid)
	}
	if auth == authUser {
		if r := []rune(label); len(r) > maxUserLabelRunes {
			return fmt.Errorf("%w: label longer than %d runes", ErrInvalid, maxUserLabelRunes)
		}
	} else {
		label = clipRunes(label, maxLabelRunes)
	}
	if n.Label == label {
		return nil
	}
	n.Label = label
	if auth == authUser {
		pin(n)
	}
	return nil
}

func applyPark(g *state, o Op, auth authority) error {
	n, err := requireModifiable(g, o.ID, auth)
	if err != nil {
		return err
	}
	status := o.Status
	if status == "" {
		status = StatusParked
	}
	if status != StatusActive && status != StatusParked {
		return fmt.Errorf("%w: bad status %q", ErrInvalid, status)
	}
	if n.Status == status {
		return nil
	}
	n.Status = status
	if auth == authUser {
		pin(n)
	}
	return nil
}

func applySetParent(g *state, o Op, auth authority) error {
	n, err := requireModifiable(g, o.ID, auth)
	if err != nil {
		return err
	}
	parent := o.ParentID
	if parent == n.ID {
		return fmt.Errorf("%w: node %q is its own parent", ErrInvalid, n.ID)
	}
	if parent != "" {
		if _, err := requireTarget(g, parent); err != nil {
			return fmt.Errorf("%w: set_parent target: %v", ErrInvalid, err)
		}
		// cycle: walking up from the new parent must never reach n
		seen := map[string]bool{}
		cur := nodeBy(g, parent)
		for cur != nil && !seen[cur.ID] {
			if cur.ID == n.ID {
				return fmt.Errorf("%w: set_parent would cycle at %q", ErrInvalid, n.ID)
			}
			seen[cur.ID] = true
			if cur.ParentID == "" {
				break
			}
			cur = nodeBy(g, cur.ParentID)
		}
	}
	if n.ParentID == parent {
		return nil
	}
	if auth == authDistill {
		if parent != "" && aliveChildren(g, parent) >= maxChildren {
			return fmt.Errorf("%w: child cap %d under %q", ErrInvalid, maxChildren, parent)
		}
		d := 0
		if parent != "" {
			d = depthOf(g, parent) + 1
		}
		if d+subtreeHeight(g, n.ID) > maxDepth {
			return fmt.Errorf("%w: subtree would exceed depth %d", ErrInvalid, maxDepth)
		}
	}
	n.ParentID = parent
	if auth == authUser {
		pin(n)
	}
	return nil
}

func applyDelete(g *state, o Op, auth authority) error {
	if auth == authDistill {
		// The model never tombstones (§6 剪枝优先于扩张): a delete from
		// the distiller is a park.
		return applyPark(g, o, auth)
	}
	n, err := requireTarget(g, o.ID)
	if err != nil {
		return err
	}
	if auth != authUser && frozen(n) {
		return fmt.Errorf("%w: node %q", ErrFrozen, o.ID)
	}
	// Cascade tombstones over the whole subtree — a live child of a dead
	// branch would be a rendering lie. Collect first (mutation doesn't
	// move nodes, but be explicit).
	dead := []*Node{n}
	for i := 0; i < len(dead); i++ {
		pid := dead[i].ID
		for j := range g.Nodes {
			c := &g.Nodes[j]
			if c.ParentID == pid && c.ID != pid && !c.Tombstone {
				if auth != authUser && frozen(c) {
					return fmt.Errorf("%w: child %q of deleted branch", ErrFrozen, c.ID)
				}
				dead = append(dead, c)
			}
		}
	}
	for _, d := range dead {
		d.Tombstone = true
		d.Status = StatusParked
		if auth == authUser {
			pin(d)
		}
	}
	return nil
}

func applyConfirm(g *state, o Op, auth authority) error {
	if auth != authUser {
		return fmt.Errorf("%w: confirm is origin=user only", ErrInvalid)
	}
	n, err := requireTarget(g, o.ID)
	if err != nil {
		return err
	}
	state := o.State
	if state == "" {
		state = StateConfirmed
	}
	if state != StateConfirmed && state != StateProposed {
		return fmt.Errorf("%w: bad state %q", ErrInvalid, state)
	}
	if n.State == state {
		return nil
	}
	n.State = state // verdict only — does not pin (unconfirm must stay possible)
	return nil
}

func applyMerge(g *state, o Op, auth authority) error {
	target, err := requireTarget(g, o.ID)
	if err != nil {
		return err
	}
	if auth != authUser && frozen(target) {
		return fmt.Errorf("%w: merge target %q", ErrFrozen, target.ID)
	}
	if len(o.SourceIDs) == 0 {
		return fmt.Errorf("%w: merge without source_ids", ErrInvalid)
	}
	sources := make([]*Node, 0, len(o.SourceIDs))
	seen := map[string]bool{}
	for _, sid := range o.SourceIDs {
		if sid == target.ID {
			return fmt.Errorf("%w: merge target %q is also a source", ErrInvalid, target.ID)
		}
		if seen[sid] {
			continue
		}
		seen[sid] = true
		s, err := requireTarget(g, sid)
		if err != nil {
			return fmt.Errorf("%w: merge source: %v", ErrInvalid, err)
		}
		if auth != authUser && frozen(s) {
			return fmt.Errorf("%w: merge source %q", ErrFrozen, s.ID)
		}
		if isDescendant(g, target.ID, s.ID) {
			return fmt.Errorf("%w: merge target %q lives under source %q", ErrInvalid, target.ID, s.ID)
		}
		sources = append(sources, s)
	}
	// Move each source's direct children up to the target, then tombstone
	// the source (吸收: the label dies, its content survives).
	for _, s := range sources {
		for j := range g.Nodes {
			c := &g.Nodes[j]
			if c.ParentID == s.ID && c.ID != s.ID {
				c.ParentID = target.ID
				if auth == authUser {
					pin(c)
				}
			}
		}
		s.Tombstone = true
		s.Status = StatusParked
		if auth == authUser {
			pin(s)
		}
	}
	return nil
}

// ── label similarity (§6: server-side merge by label) ─────────────

// similar reports whether two labels are the same phrase for merging /
// tombstone-blocking purposes: exact after normalization, or bigram Dice
// ≥ 0.75 (CJK-safe — bigrams are over runes).
func similar(a, b string) bool {
	na, nb := normLabel(a), normLabel(b)
	if na == "" || nb == "" {
		return false
	}
	if na == nb {
		return true
	}
	return dice(na, nb) >= 0.75
}

func normLabel(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func dice(a, b string) float64 {
	ra, rb := []rune(a), []rune(b)
	if len(ra) < 2 || len(rb) < 2 {
		return 0
	}
	ga := runeGrams(ra)
	gb := runeGrams(rb)
	common := 0
	for k, va := range ga {
		if vb, ok := gb[k]; ok {
			if va < vb {
				common += va
			} else {
				common += vb
			}
		}
	}
	return 2 * float64(common) / float64(len(ra)-1+len(rb)-1)
}

func runeGrams(r []rune) map[string]int {
	m := map[string]int{}
	for i := 0; i+1 < len(r); i++ {
		m[string(r[i:i+2])]++
	}
	return m
}
