package server

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"

	"lyon-agent/pkg/agent"
)

// hub is a session's event fan-out: the single consumer of the session's
// event channel, stamping a monotonic per-session seq (from 1) and
// broadcasting every event to N subscribers — the legacy SSE pump, any
// number of concurrent WatchEvents streams (#并发倾向: fan-out, one
// consumer, never N consumers racing on one chan).
//
// Nobody can block the driver: a subscriber that falls behind its bounded
// channel is kicked (channel closed + flagged), never waited on.
//
// Lifecycle: Store.Create starts run; it exits when the session's event
// channel closes (Session.Close), which closes every subscriber so live
// streams end cleanly instead of hanging.
type hub struct {
	events <-chan agent.Event // the session's single consumer channel

	seq atomic.Int64 // last stamped seq; incremented only by push under mu

	mu     sync.Mutex
	ring   []hubItem          // bounded replay buffer, seq-ascending
	subs   map[uint64]*hubSub // live subscribers by id
	nextID uint64
}

// hubItem is one stamped event on the wire.
type hubItem struct {
	seq int64
	ev  agent.Event
}

// hubSub is one subscriber's inbox. After ch is closed, the receiver may
// read kicked: it is written under hub.mu before the close, and the close
// itself orders that write for whoever observes ok==false.
type hubSub struct {
	id     uint64
	ch     chan hubItem
	kicked bool
}

const (
	hubRingSize  = 4096 // replay buffer per session (WatchEvents reconnect window)
	hubSubBuffer = 1024 // per-subscriber channel depth before a kick
)

// ErrRingGap means a WatchEvents from_seq anchor has already been evicted
// from the ring: the client must re-anchor (e.g. replay with from_seq=-1)
// and retry.
var ErrRingGap = errors.New("requested from_seq is no longer in the replay ring")

func newHub(events <-chan agent.Event) *hub {
	return &hub{events: events, subs: make(map[uint64]*hubSub)}
}

// run is the hub's single consumer of the driver's events: dispatch each
// through push (stamp, sanitize, ring, fan out). It never blocks on a
// subscriber and never drops for the driver.
func (h *hub) run() {
	for ev := range h.events {
		h.mu.Lock()
		h.push(ev)
		h.mu.Unlock()
	}
	// Session closed (event chan drained and closed): close every
	// subscriber so streams see EOF after their buffered events.
	h.mu.Lock()
	for id, sub := range h.subs {
		delete(h.subs, id)
		close(sub.ch)
	}
	h.mu.Unlock()
}

// inject stamps a server-synthesized event (prompt echo, restored
// snapshot history) through the exact path run() uses, so seq-ascending
// always matches ring-ascending no matter who produced the event. Safe
// after shutdown: run's exit path has emptied h.subs under this same
// lock, so the fan-out loop below touches no closed channel.
func (h *hub) inject(ev agent.Event) {
	h.mu.Lock()
	h.push(ev)
	h.mu.Unlock()
}

// injectAll stamps a batch under one lock — a restored conversation
// enters the ring atomically with respect to subscribers, so nobody ever
// replays half a history before the rest lands.
func (h *hub) injectAll(evs []agent.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ev := range evs {
		h.push(ev)
	}
}

// resetAndInject replaces the replay ring with evs under one lock — the
// rollback seam: the conversation itself was rewritten, so the old events
// would narrate a history that no longer exists. seq keeps ascending
// (stamp-only-under-mu stays true, existing live subscribers just see the
// new batch continue after the old last seq); reconnecting with -1
// replays exactly the post-reset transcript.
func (h *hub) resetAndInject(evs []agent.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ring = h.ring[:0]
	for _, ev := range evs {
		h.push(ev)
	}
}

// push stamps, sanitizes, rings, and fans one event out. Caller holds
// h.mu — stamping under the lock is what keeps ring order and seq order
// identical across the driver path (run) and synthetic injections.
func (h *hub) push(ev agent.Event) {
	it := hubItem{seq: h.seq.Add(1), ev: sanitizeEvent(ev)}
	h.ring = append(h.ring, it)
	if len(h.ring) > hubRingSize {
		// Keep the newest window. The backing array's older slots are
		// overwritten by later appends (amortized growth, bounded).
		h.ring = h.ring[len(h.ring)-hubRingSize:]
	}
	for id, sub := range h.subs {
		select {
		case sub.ch <- it:
		default:
			// Slow subscriber: kick it rather than stall every other
			// subscriber and the driver behind them.
			delete(h.subs, id)
			sub.kicked = true
			close(sub.ch)
		}
	}
}

// subscribe registers a live subscriber and computes its replay batch.
// fromSeq semantics (WatchEventsRequest.from_seq):
//
//	0  = live only
//	-1 = replay everything still in the ring, then live
//	n>0 = replay ring entries with seq > n, then live
//
// If the ring no longer holds n+1 (anchor evicted, or n is from the
// future), it returns ErrRingGap without subscribing — the client
// re-anchors and retries.
func (h *hub) subscribe(fromSeq int64) (*hubSub, []hubItem, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if fromSeq > 0 {
		// Two ways an anchor is unusable: it is from the future (beyond
		// anything this hub ever stamped — replaying "seq > n" would then
		// re-deliver live events the client already claims to have), or it
		// is too old (its successor was evicted from the ring).
		if fromSeq > h.seq.Load() || (len(h.ring) > 0 && h.ring[0].seq > fromSeq+1) {
			return nil, nil, ErrRingGap
		}
	}
	sub := &hubSub{id: h.nextID, ch: make(chan hubItem, hubSubBuffer)}
	h.nextID++
	h.subs[sub.id] = sub
	var replay []hubItem
	switch {
	case fromSeq == 0:
		// live only
	case fromSeq == -1:
		replay = make([]hubItem, len(h.ring))
		copy(replay, h.ring)
	default:
		for _, it := range h.ring {
			if it.seq > fromSeq {
				replay = append(replay, it)
			}
		}
	}
	return sub, replay, nil
}

// dump snapshots the replay ring seq-ascending without registering a
// subscriber — GetHistory's unary twin of subscribe(-1). A caller that
// wants the live tail afterwards anchors at the last returned seq (0 when
// empty → the client falls back to -1): everything stamped before the
// snapshot is inside, everything stamped after replays off the anchor.
func (h *hub) dump() []hubItem {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.ring) == 0 {
		return nil
	}
	out := make([]hubItem, len(h.ring))
	copy(out, h.ring)
	return out
}

// subscribeLive subscribes at the current seq with no replay — the SSE
// attach point. Cannot fail: the current seq is by definition still
// in-ring, and events stamped concurrently land in the new subscriber
// live. Call it BEFORE Submit so nothing between Submit and the first
// read is lost.
func (h *hub) subscribeLive() *hubSub {
	h.mu.Lock()
	defer h.mu.Unlock()
	sub := &hubSub{id: h.nextID, ch: make(chan hubItem, hubSubBuffer)}
	h.nextID++
	h.subs[sub.id] = sub
	return sub
}

// unsubscribe removes sub from the fan-out. Safe after a kick or hub
// shutdown (already removed). The channel is never closed here — only the
// run goroutine closes subscriber channels.
func (h *hub) unsubscribe(sub *hubSub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if cur, ok := h.subs[sub.id]; ok && cur == sub {
		delete(h.subs, sub.id)
	}
}

// sanitizeEvent returns a copy of ev with valid-UTF-8 string fields:
// protojson rejects invalid UTF-8 outright, while encoding/json (the SSE
// frame path) substitutes U+FFFD on the way out — so for valid input (the
// overwhelming case) the SSE bytes are unchanged, and for a rare invalid
// run both surfaces render replacement characters.
func sanitizeEvent(ev agent.Event) agent.Event {
	fix := func(s string) string { return strings.ToValidUTF8(s, "�") }
	ev.Type = agent.EventType(fix(string(ev.Type)))
	ev.Text = fix(ev.Text)
	ev.Thinking = fix(ev.Thinking)
	ev.ToolID = fix(ev.ToolID)
	ev.ToolName = fix(ev.ToolName)
	ev.SubID = fix(ev.SubID)
	ev.Err = fix(ev.Err)
	ev.StopReason = fix(ev.StopReason)
	ev.Detail = fix(ev.Detail)
	if len(ev.Input) > 0 {
		ev.Input = json.RawMessage(fix(string(ev.Input)))
	}
	if len(ev.Attrs) > 0 {
		// Copy: the map may be shared with the emitter (e.g. the session's
		// memory buffer), and sanitizing must never mutate producer state.
		attrs := make(map[string]string, len(ev.Attrs))
		for k, v := range ev.Attrs {
			attrs[fix(k)] = fix(v)
		}
		ev.Attrs = attrs
	}
	return ev
}
