// MemoryService — the three memory layers (team / personal under
// $LYON_HOME, project under the workspace) for the web panel. Handlers go
// through pkg/memory's process-wide Store (memory.Open caches per
// workspace), the SAME instance the agent's save_memory tool mutates, so
// panel edits, model writes and the search index stay in one place; the
// $LYON_HOME resolution lives in homeDir() server-side, which is exactly
// what the browser could never do through FileService.

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"

	lyonv1 "lyon-agent/gen/go/lyon/v1"
	"lyon-agent/gen/go/lyon/v1/lyonv1connect"
	"lyon-agent/pkg/memory"
)

type memoryService struct {
	s *Server
}

var _ lyonv1connect.MemoryServiceHandler = (*memoryService)(nil)

func (m *memoryService) store(h http.Header) *memory.Store {
	return memory.Open(m.s.wsFor(h))
}

// validMemScope mirrors pkg/memory's unexported validScope — reject bad
// scopes here instead of letting Store silently coerce them to project.
func validMemScope(scope string) bool {
	return scope == memory.ScopeTeam || scope == memory.ScopePersonal || scope == memory.ScopeProject
}

func factProto(f memory.Fact) *lyonv1.MemoryFact {
	return &lyonv1.MemoryFact{
		Name:        f.Name,
		Description: f.Description,
		Scope:       f.Scope,
		Tags:        f.Tags,
		Hot:         f.Hot,
		Hits:        int32(f.Hits),
		Body:        f.Body,
	}
}

func (m *memoryService) ListMemory(_ context.Context, req *connect.Request[lyonv1.ListMemoryRequest]) (*connect.Response[lyonv1.ListMemoryResponse], error) {
	// "" = all scopes; the store already orders project → personal → team
	// (closest first), name-ascending within a scope.
	facts := m.store(req.Header()).List("")
	out := make([]*lyonv1.MemoryFact, 0, len(facts))
	for _, f := range facts {
		out = append(out, factProto(f))
	}
	return connect.NewResponse(&lyonv1.ListMemoryResponse{Facts: out}), nil
}

func (m *memoryService) UpdateMemory(_ context.Context, req *connect.Request[lyonv1.UpdateMemoryRequest]) (*connect.Response[lyonv1.UpdateMemoryResponse], error) {
	msg := req.Msg
	if !validMemScope(msg.Scope) {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("bad memory scope %q (want team|personal|project)", msg.Scope))
	}
	st := m.store(req.Header())
	f, ok := st.Get(msg.Scope, msg.Name)
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("no memory %q in scope %q", msg.Name, msg.Scope))
	}
	// Text fields ride Save (which preserves Created/Hits and — by design —
	// the stored hot flag; hot only ever moves through ToggleHot).
	if msg.Description != nil || msg.Body != nil {
		if msg.Description != nil {
			f.Description = *msg.Description
		}
		if msg.Body != nil {
			f.Body = *msg.Body
		}
		saved, err := st.Save(f)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		f = saved
	}
	// hot is a SET: toggle only when it differs (ToggleHot flips).
	if msg.Hot != nil && *msg.Hot != f.Hot {
		if _, err := st.ToggleHot(f.Scope, f.Name); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		if cur, ok := st.Get(f.Scope, f.Name); ok {
			f = cur
		}
	}
	return connect.NewResponse(&lyonv1.UpdateMemoryResponse{Fact: factProto(f)}), nil
}

func (m *memoryService) DeleteMemory(_ context.Context, req *connect.Request[lyonv1.DeleteMemoryRequest]) (*connect.Response[lyonv1.DeleteMemoryResponse], error) {
	msg := req.Msg
	if !validMemScope(msg.Scope) {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("bad memory scope %q (want team|personal|project)", msg.Scope))
	}
	if err := m.store(req.Header()).Delete(msg.Scope, msg.Name); err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewResponse(&lyonv1.DeleteMemoryResponse{}), nil
}

// --- AI curation (TransformMemory) --------------------------------------
//
// The panel's 简化 / 合并 / 提取 buttons: a server-side one-shot model
// call reads the SELECTED facts and the result lands straight back in
// the Store — instant for the UI, no session occupied, no chat turn.
// The plan is validated BEFORE anything is written (deletes only under
// merge and only inside the selection; a write may not overwrite a fact
// the user did not select), so a confused model fails loudly instead of
// quietly reshaping the store.

const (
	transformTargetsCap = 32   // selection size the RPC accepts
	transformBodyCap    = 8192 // bytes of one fact body sent to the model
	transformMaxWrites  = 32   // plan-side bound (simplify ≤ selection)
	transformTimeout    = 2 * time.Minute
	transformMaxTokens  = 8000
)

// memTransformSystem is the one-shot's system prompt: op semantics,
// the exact reply schema, and the hard rules the handler re-checks.
const memTransformSystem = `You curate a user's persistent memory store. You receive a JSON array of memory facts: [{"scope","name","description","body"}] and one op:

- simplify: rewrite EACH fact's body to be shorter and sharper. Same facts, same names. Keep every checkable detail (commands, paths, numbers, decisions); drop repetition and filler. No new facts, no deletions.
- merge: combine ALL selected facts into ONE fact. Keep the FIRST fact's scope and name (or pick a concise lowercase slug). Every distinct fact the selection carried must survive in the merged body — nothing may be lost.
- extract: distill the selection into ONE NEW fact (new lowercase slug name, scope of the first fact) capturing the shared essence or recurring pattern. The originals stay untouched: no deletes.

Reply with ONLY one JSON object — no prose, no markdown fences:
{"summary":"what you did, one short paragraph","writes":[{"scope":"project|personal|team","name":"slug","description":"one line","body":"..."}],"deletes":[{"scope":"...","name":"..."}]}

Rules: scopes must be one of project|personal|team. Names are lowercase slugs (a-z, 0-9, hyphen). body must be non-empty. writes: exactly 1 for merge|extract; one per input fact for simplify, each keeping its input name. deletes only for merge (the consumed sources; empty if you kept the first fact's name and the rest are gone — list every source you replaced). Never mention facts that were not given to you.`

type transformPlan struct {
	Summary string `json:"summary"`
	Writes  []struct {
		Scope       string `json:"scope"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Body        string `json:"body"`
	} `json:"writes"`
	Deletes []struct {
		Scope string `json:"scope"`
		Name  string `json:"name"`
	} `json:"deletes"`
}

// transformTarget is one selected fact, loaded before the model call so
// the pass works from the STORE's copy (and the panel's stale row can't
// feed the model text the store no longer holds).
type transformTarget struct {
	scope, name string
	fact        memory.Fact
}

func (m *memoryService) TransformMemory(ctx context.Context, req *connect.Request[lyonv1.TransformMemoryRequest]) (*connect.Response[lyonv1.TransformMemoryResponse], error) {
	msg := req.Msg
	op := strings.TrimSpace(msg.Op)
	if op != "simplify" && op != "merge" && op != "extract" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown op %q (want simplify|merge|extract)", msg.Op))
	}
	if len(msg.Targets) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("select at least one memory"))
	}
	if len(msg.Targets) > transformTargetsCap {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%d targets over the cap of %d", len(msg.Targets), transformTargetsCap))
	}
	st := m.store(req.Header())

	// Load the selection — every target must exist, or the pass would
	// rewrite facts the panel no longer shows.
	targets := make([]transformTarget, 0, len(msg.Targets))
	seen := make(map[string]bool, len(msg.Targets))
	for _, t := range msg.Targets {
		if !validMemScope(t.Scope) {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("bad memory scope %q", t.Scope))
		}
		key := t.Scope + "\x00" + strings.ToLower(t.Name)
		if seen[key] {
			continue
		}
		f, ok := st.Get(t.Scope, t.Name)
		if !ok {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("no memory %q in scope %q", t.Name, t.Scope))
		}
		seen[key] = true
		targets = append(targets, transformTarget{scope: t.Scope, name: t.Name, fact: f})
	}

	payload := make([]map[string]string, 0, len(targets))
	for _, t := range targets {
		payload = append(payload, map[string]string{
			"scope":       t.scope,
			"name":        t.name,
			"description": t.fact.Description,
			"body":        clipBytes(t.fact.Body, transformBodyCap),
		})
	}
	user, err := json.Marshal(payload)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	// One-shot on the subagent tier — same seam as title/post-mortem
	// calls (Session.Ask): no session history, no tool loop, safe beside
	// a live drive.
	ctx, cancel := context.WithTimeout(ctx, transformTimeout)
	defer cancel()
	sess := m.s.eng.NewSession(nil)
	defer sess.Close()
	reply, err := sess.Ask(ctx, memTransformSystem, string(user), transformMaxTokens)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("model call failed: %w", err))
	}

	var plan transformPlan
	if err := json.Unmarshal([]byte(extractJSONObject(reply)), &plan); err != nil {
		return nil, connect.NewError(connect.CodeInternal,
			fmt.Errorf("model reply is not the plan JSON (%v): %.200s", err, reply))
	}
	changed, removed, err := applyTransformPlan(st, op, targets, plan)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	out := &lyonv1.TransformMemoryResponse{
		Op:      op,
		Summary: clipRunesStr(plan.Summary, 4000),
	}
	for _, f := range changed {
		out.Changed = append(out.Changed, factProto(f))
	}
	for _, r := range removed {
		out.Removed = append(out.Removed, &lyonv1.MemoryRef{Scope: r.scope, Name: r.name})
	}
	return connect.NewResponse(out), nil
}

// applyTransformPlan validates the model's plan against the selection
// and only then writes it: saves first (a merge's kept name must exist
// before its sources are consumed), deletes second, skipping any name
// the plan itself wrote back (a merge that kept the first source's name
// must not delete its own result).
func applyTransformPlan(st *memory.Store, op string, targets []transformTarget, plan transformPlan) ([]memory.Fact, []transformTarget, error) {
	if strings.TrimSpace(plan.Summary) == "" {
		return nil, nil, fmt.Errorf("plan has no summary")
	}
	if len(plan.Writes) > transformMaxWrites {
		return nil, nil, fmt.Errorf("%d writes over the cap of %d", len(plan.Writes), transformMaxWrites)
	}
	isTarget := func(scope, name string) bool {
		key := scope + "\x00" + strings.ToLower(name)
		for _, t := range targets {
			if t.scope+"\x00"+strings.ToLower(t.name) == key {
				return true
			}
		}
		return false
	}
	// op-specific shape.
	switch op {
	case "simplify":
		if len(plan.Deletes) != 0 {
			return nil, nil, fmt.Errorf("simplify must not delete facts")
		}
		if len(plan.Writes) == 0 || len(plan.Writes) > len(targets) {
			return nil, nil, fmt.Errorf("simplify wants 1..%d writes, got %d", len(targets), len(plan.Writes))
		}
		for _, w := range plan.Writes {
			if !isTarget(w.Scope, w.Name) {
				return nil, nil, fmt.Errorf("simplify may only rewrite selected facts (got %s/%s)", w.Scope, w.Name)
			}
		}
	case "merge", "extract":
		if len(plan.Writes) != 1 {
			return nil, nil, fmt.Errorf("%s writes exactly one fact, got %d", op, len(plan.Writes))
		}
		if op == "extract" && len(plan.Deletes) != 0 {
			return nil, nil, fmt.Errorf("extract must not delete the originals")
		}
		for _, d := range plan.Deletes {
			if !isTarget(d.Scope, d.Name) {
				return nil, nil, fmt.Errorf("merge may only delete selected facts (got %s/%s)", d.Scope, d.Name)
			}
		}
	}
	// A write may overwrite a non-selected fact only never: existing
	// names outside the selection are off limits (Save upserts by name).
	for _, w := range plan.Writes {
		if !validMemScope(w.Scope) {
			return nil, nil, fmt.Errorf("bad write scope %q", w.Scope)
		}
		if strings.TrimSpace(w.Name) == "" || strings.TrimSpace(w.Body) == "" {
			return nil, nil, fmt.Errorf("write needs a non-empty name and body")
		}
		if _, exists := st.Get(w.Scope, w.Name); exists && !isTarget(w.Scope, w.Name) {
			return nil, nil, fmt.Errorf("would overwrite non-selected memory %s/%s", w.Scope, w.Name)
		}
	}

	// Apply: writes first, then deletes (minus names just written).
	var changed []memory.Fact
	for _, w := range plan.Writes {
		saved, err := st.Save(memory.Fact{
			Name:        w.Name,
			Description: strings.TrimSpace(w.Description),
			Scope:       w.Scope,
			Body:        w.Body,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("write %s/%s: %w", w.Scope, w.Name, err)
		}
		changed = append(changed, saved)
	}
	written := make(map[string]bool, len(changed))
	for _, f := range changed {
		written[f.Scope+"\x00"+strings.ToLower(f.Name)] = true
	}
	var removed []transformTarget
	for _, d := range plan.Deletes {
		key := d.Scope + "\x00" + strings.ToLower(d.Name)
		if written[key] {
			continue // the merge kept this name — its own result stays
		}
		if err := st.Delete(d.Scope, d.Name); err != nil {
			continue // already gone — the end state holds
		}
		removed = append(removed, transformTarget{scope: d.Scope, name: d.Name})
	}
	return changed, removed, nil
}

// extractJSONObject pulls the first {...} block out of a reply that may
// wrap it in prose or ```json fences — callers parse their own output
// (Session.Ask returns verbatim text).
func extractJSONObject(s string) string {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return strings.TrimSpace(s)
	}
	return s[start : end+1]
}

// clipBytes bounds one fact body for the model payload without
// splitting a rune: walk the cut back off any UTF-8 continuation byte
// (0b10xxxxxx), so s[:cut] always ends on a complete rune.
func clipBytes(s string, cap int) string {
	if len(s) <= cap {
		return s
	}
	cut := cap
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "\n...[truncated]"
}

// clipRunesStr clips a display string by runes.
func clipRunesStr(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
