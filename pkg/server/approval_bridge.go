package server

import (
	"context"
	"errors"
	"sync"
	"time"

	lyonv1 "lyon-agent/gen/go/lyon/v1"
	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/tools"
)

// ErrApprovalGone is decide on a key that is not parked: answered already,
// interrupted away, or never existed. Callers drop their card on it.
var ErrApprovalGone = errors.New("approval no longer pending")

// parkKey identifies one in-flight approval: the seat (session) plus the
// process-unique execution id the adapter stamped before asking.
type parkKey struct {
	sid  string
	exec uint64
}

// park is one approval waiting on a human. resp is the wire form pushed
// to every WatchPending subscriber the moment the adapter asks.
type park struct {
	resp *lyonv1.WatchPendingResponse
	ch   chan bool // buffered(1): Decide never blocks on a dead approvee
}

// ApprovalBridge parks NeedsApproval tool executions until a web client
// decides — the lyond twin of tuiApprove (pkg/approve semantics: park on
// ch, ctx cancellation denies, fail closed). Unlike TUI's single overlay
// slot (apprMu serializes one card at a time), the bridge parks
// concurrently: multiple sessions/tools may ask at once and subscribers
// see them as they come (the product's concurrency-first stance — a
// human answering one card must not stall another session's tool).
//
// Lifecycle: create one before building the engine cfg (ApproveFunc), then
// hand it to Server.SetApproval so the store back-resolution works. With
// no SetApproval the bridge fails closed rather than opening a hole.
type ApprovalBridge struct {
	mu      sync.Mutex
	store   *Store // for sess→sid back-resolution; nil until SetApproval
	pending map[parkKey]*park
	// rem memoizes "this tool, this seat" grants (TUI allowAll semantics:
	// the "a" key vouches for the tool for the rest of the session, never
	// across sessions, never persisted — proto Decide.remember TODO(v2)
	// grows policy.json for the real thing).
	rem    map[string]bool // sid + "\x00" + tool name → granted
	subs   map[uint64]chan *lyonv1.WatchPendingResponse
	subSeq uint64
}

// NewApprovalBridge creates an empty bridge. Wire cfg.Approve =
// bridge.ApproveFunc() only under -approve; unattached RPCs stay
// Unimplemented via approvalService.
func NewApprovalBridge() *ApprovalBridge {
	return &ApprovalBridge{
		pending: make(map[parkKey]*park),
		rem:     make(map[string]bool),
		subs:    make(map[uint64]chan *lyonv1.WatchPendingResponse),
	}
}

// ApproveFunc is the engine-wide approver. The driving session is read off
// the tool context (agent.SessionFromContext — the adapter's ExecEvent has
// no seat), so one closure serves every session.
func (b *ApprovalBridge) ApproveFunc() tools.ApproveFunc {
	return func(ctx context.Context, e tools.ExecEvent) bool {
		if ctx.Err() != nil {
			return false
		}
		sess := agent.SessionFromContext(ctx)
		if sess == nil || b.store == nil {
			return false // no seat → nothing to decide against; fail closed
		}
		sid := b.store.IDOf(sess)
		if sid == "" {
			return false // evicted under the caller mid-execution
		}
		if b.remembered(sid, e.Name) {
			return true
		}

		key := parkKey{sid: sid, exec: e.ExecID}
		p := &park{
			resp: &lyonv1.WatchPendingResponse{
				SessionId:     sid,
				ExecId:        int64(e.ExecID),
				ToolName:      e.Name,
				InputJson:     string(e.Input),
				RequestedAtMs: time.Now().UnixMilli(),
			},
			ch: make(chan bool, 1),
		}
		b.mu.Lock()
		b.pending[key] = p
		b.broadcastLocked(p.resp)
		b.mu.Unlock()

		defer func() {
			// Every exit path (decided, ctx cancelled, session reaped)
			// frees the seat and tells subscribers to drop the card —
			// otherwise an interrupted run would leave a phantom pending
			// on every watch. Resolve carries the bare key (empty
			// tool_name is the removal signal; approval.proto).
			b.mu.Lock()
			if _, ok := b.pending[key]; ok {
				delete(b.pending, key)
				b.broadcastLocked(&lyonv1.WatchPendingResponse{
					SessionId: sid, ExecId: int64(e.ExecID),
				})
			}
			b.mu.Unlock()
		}()

		select {
		case v := <-p.ch:
			return v
		case <-ctx.Done():
			return false // interrupt / reap / shutdown — deny, matching TUI
		}
	}
}

// decide answers one parked approval. remember also vouches for the tool
// name on this seat (TUI "a"). ErrApprovalGone covers double decisions
// and unknown keys.
func (b *ApprovalBridge) decide(sid string, exec uint64, approve, remember bool) error {
	key := parkKey{sid: sid, exec: exec}
	b.mu.Lock()
	p, ok := b.pending[key]
	if !ok {
		b.mu.Unlock()
		return ErrApprovalGone
	}
	delete(b.pending, key)
	if remember && approve {
		b.rem[sid+"\x00"+p.resp.ToolName] = true
	}
	b.broadcastLocked(&lyonv1.WatchPendingResponse{SessionId: sid, ExecId: int64(exec)})
	b.mu.Unlock()
	p.ch <- approve // buffered(1), taken by the parked ApproveFunc
	return nil
}

// snapshot replies the current pending set — the WatchPending opener, so a
// (re)connecting client converges on what is actually parked.
func (b *ApprovalBridge) snapshot() []*lyonv1.WatchPendingResponse {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*lyonv1.WatchPendingResponse, 0, len(b.pending))
	for _, p := range b.pending {
		// The resp is immutable post-publish (a resolve only drops the
		// park entry), so subscribers can safely share the pointer —
		// protobuf messages must not be copied field-by-field either.
		out = append(out, p.resp)
	}
	return out
}

// subscribe registers a fan-out lane under the lock, so no park can slip
// between the snapshot and the live tail (events after the snapshot ride
// the lane; the snapshot itself is sent before the lane is read).
func (b *ApprovalBridge) subscribe() (uint64, <-chan *lyonv1.WatchPendingResponse) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subSeq++
	id := b.subSeq
	ch := make(chan *lyonv1.WatchPendingResponse, 128)
	b.subs[id] = ch
	return id, ch
}

// unsubscribe removes and closes the lane (idempotent). Closing only ever
// happens here or in broadcastLocked's slow-kick — both under b.mu with a
// map-presence check, so each lane closes exactly once.
func (b *ApprovalBridge) unsubscribe(id uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.subs[id]; ok {
		delete(b.subs, id)
		close(ch)
	}
}

// broadcastLocked fans an update to every lane; a lane whose buffer is
// full has a dead reader, so it is dropped — the client reconnects and
// re-reads the snapshot rather than wedging the approver.
func (b *ApprovalBridge) broadcastLocked(ev *lyonv1.WatchPendingResponse) {
	for id, ch := range b.subs {
		select {
		case ch <- ev:
		default:
			delete(b.subs, id)
			close(ch)
		}
	}
}

func (b *ApprovalBridge) remembered(sid, tool string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.rem[sid+"\x00"+tool]
}
