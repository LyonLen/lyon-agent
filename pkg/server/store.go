// Package server exposes the lyon engine over HTTP + SSE: one endpoint per
// prompt, a session store with admission control and idle eviction, and a
// slow-client-proof streaming writer.
package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"sort"
	"sync"
	"time"

	"lyon-agent/pkg/agent"
)

var (
	// ErrFull is returned when the session cap is reached.
	ErrFull = errors.New("session limit reached")
	// ErrNotFound is returned for unknown session ids.
	ErrNotFound = errors.New("session not found")
	// errSeatExists reports a registration that lost the race to another
	// adopter: the id is taken, so the caller should use that session.
	errSeatExists = errors.New("seat already registered")
)

// Store is a cache over the dump files, not a home for them. A seat's
// conversation lives in its draft ($LYON_HOME/sessions/<ws>/drafts/<id>.json)
// written on every state change; this process only holds the sessions that
// are actually in use. A seat leaves memory when it goes idle (Reap) or when
// the process exits (CloseAll), and it keeps its id, title and history on
// disk, so the next List still shows it and the next Acquire adopts it back.
//
// Why it matters: every bug the in-memory-only store had — a restart handing
// out new ids for the same conversation, duplicate "(untitled)" rows,
// sessions stuck "running", a SAVED row per restart — came from the map being
// the only record. The dump is the record now; the map is an accelerator.
//
// Explicit Delete is the one path that destroys: no archive, draft removed.
// Archiving a deleted seat is what resurrected deleted conversations as SAVED
// rows.
type Store struct {
	mu       sync.Mutex
	max      int
	ttl      time.Duration
	sessions map[string]*entry
	// parked holds seats that are on disk only. Their metadata is what a
	// list row and the gauge need; the conversation itself stays in the
	// file until the seat is touched again.
	parked map[string]parkedSeat
	loaded bool
	closed bool
}

type entry struct {
	sess       *agent.Session
	hub        *hub         // fan-out of sess.Events(); the sole consumer
	draft      *agent.Draft // mirror to the seat's dump file
	created    time.Time
	lastActive time.Time
}

// parkedSeat is one disk-only seat: its dump path and the metadata read back
// from it (Messages nil — the file keeps the conversation).
type parkedSeat struct {
	path string
	snap agent.Snapshot
}

// NewStore creates a store. max<=0 means unlimited; ttl<=0 disables eviction.
func NewStore(max int, ttl time.Duration) *Store {
	return &Store{
		max:      max,
		ttl:      ttl,
		sessions: make(map[string]*entry),
		parked:   make(map[string]parkedSeat),
	}
}

// LoadDrafts indexes the seats a previous process left on disk. Call it once
// before serving (lyond's startup): from then on the list includes them and
// acquiring one adopts it back under its own id. Idempotent; best effort per
// file — an unreadable draft is skipped, never fatal. Returns how many seats
// came back.
func (s *Store) LoadDrafts(workspace string) (int, error) {
	s.mu.Lock()
	if s.loaded || s.closed {
		s.mu.Unlock()
		return 0, nil
	}
	s.loaded = true
	s.mu.Unlock()

	drafts, err := agent.ListDrafts(workspace)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, d := range drafts {
		if d.ID == "" {
			continue
		}
		if _, live := s.sessions[d.ID]; live {
			continue
		}
		if _, ok := s.parked[d.ID]; ok {
			continue
		}
		s.parked[d.ID] = parkedSeat{path: d.Path, snap: metaOf(d.Snap)}
		n++
	}
	return n, nil
}

// Create registers a session under a fresh random id and starts its event
// hub — from here the hub is the session's only Events() consumer, so
// everything that wants the stream (SSE pump, WatchEvents) subscribes to
// the hub instead (web plan §2).
func (s *Store) Create(sess *agent.Session) (string, error) {
	return s.register(sess, newID())
}

// Register adopts a session under a known id — a seat read back from disk
// keeps the id the client already knows instead of becoming a copy with a
// new one. Registration failure leaves the caller owning sess.
func (s *Store) Register(sess *agent.Session, id string) error {
	if id == "" {
		return ErrNotFound
	}
	_, err := s.register(sess, id)
	return err
}

func (s *Store) register(sess *agent.Session, id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", ErrNotFound
	}
	if _, dup := s.sessions[id]; dup {
		return "", errSeatExists
	}
	if s.max > 0 && len(s.sessions) >= s.max {
		return "", ErrFull
	}
	now := time.Now()
	// A restored session carries its own registration times; a fresh one
	// has none. Created/lastActive must survive a restart, or every row
	// resets its age each time lyond comes back.
	_, created, lastActive := sess.Seat()
	if created.IsZero() {
		created = now
	}
	if lastActive.IsZero() {
		lastActive = created
	}
	// The seat id lives on the session, not just in this map: every dump
	// it mirrors carries it, so a restart re-adopts the same seat instead
	// of handing the client a new id for the same conversation.
	sess.SetSeat(id, created)
	sess.MarkActive(lastActive)
	e := &entry{sess: sess, created: created, lastActive: lastActive}
	e.hub = newHub(sess.Events())
	// Every state change mirrors the conversation to the seat's dump, so a
	// hard kill (taskkill /F, OOM) costs one prompt — and an idle seat has
	// nothing left to lose when it is unloaded. The path is derived from
	// the workspace, not the store: dumps belong to the archive directory.
	if path, err := agent.DraftPath(sess.Workspace(), id); err == nil {
		e.draft = sess.NewDraft(path)
	}
	// Interval title pass: the hub taps every prompt_end, the store is
	// where the *Session lives (lyond's cadence decision stays out of
	// pkg/agent — the TUI drives its own title check). The mirror itself
	// is not driven from here: the session touches its own draft on every
	// state change (notifyDirty), which also covers turns, mode switches
	// and title writes that never cross a prompt_end.
	e.hub.onPromptEnd = func() { maybeIntervalTitle(sess) }
	delete(s.parked, id) // adopted: the disk record is live now
	s.sessions[id] = e
	go e.hub.run()
	return id, nil
}

// Get returns the live session for id, refreshing its idle timer. Strictly
// live: a seat that is only on disk is not "gotten" (that would make a
// deleted or reaped id silently resurrect). Server.acquire is the
// adopt-on-touch counterpart.
func (s *Store) Get(id string) (*agent.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.sessions[id]
	if !ok {
		return nil, ErrNotFound
	}
	e.lastActive = time.Now()
	e.sess.MarkActive(e.lastActive)
	return e.sess, nil
}

// Delete removes a seat for good: live map entry, disk dump, in-memory
// history. Returns false when the id is unknown — an id that a previous
// process left on disk still counts as known. Nothing is archived: a delete
// that archives is a delete that comes back as a SAVED row.
func (s *Store) Delete(id string) bool {
	s.mu.Lock()
	e, live := s.sessions[id]
	if live {
		delete(s.sessions, id)
	}
	p, disk := s.parked[id]
	if disk {
		delete(s.parked, id)
	}
	s.mu.Unlock()

	if !live {
		if disk {
			_ = os.Remove(p.path)
			return true
		}
		return false
	}
	// Interrupt first: a mirror write in flight is inside Export, which waits
	// on histMu, which a running prompt holds. Stopping the prompt releases
	// the write, so Close cannot hang for the length of a run.
	e.sess.Interrupt()
	if e.draft != nil {
		e.draft.Close() // no mirror may land after the removal
		_ = os.Remove(e.draft.Path())
	}
	e.sess.Close()
	return true
}

// unload moves a live seat to disk: the mirror stops, the prompt settles, the
// final state is written to the seat's dump, and the session closes. The dump
// is authoritative, so the metadata cached for listing is read from what was
// just written — a parked seat lists exactly like the live one did, minus the
// run state (it is idle by definition).
func (s *Store) unload(id string, e *entry) {
	// Order matters: Interrupt releases histMu (drive holds it for a whole
	// prompt), and only then can a mirror's Export — and the Export below —
	// return. Closing the draft first would block for the length of the run.
	e.sess.Interrupt()
	if e.draft != nil {
		e.draft.Close() // no new mirrors; wait for the in-flight one
	}
	snap := e.sess.Export() // waits out the prompt's bookkeeping (histMu)
	ws := e.sess.Workspace()
	e.sess.Close()

	if len(snap.Messages) == 0 {
		return // nothing worth a seat — an empty session leaves no row
	}
	path := ""
	if e.draft != nil {
		path = e.draft.Path()
	} else if p, err := agent.DraftPath(ws, id); err == nil {
		path = p
	}
	if path == "" {
		return
	}
	// Write the settled state explicitly: the mirror may have last landed
	// before this prompt's tail (tool repair, final turn), and a park must
	// never lose it. Best effort — an unwritable dump means no row, not a
	// wedged teardown.
	if err := agent.WriteSnapshotFile(path, snap); err != nil {
		return
	}
	s.mu.Lock()
	s.parked[id] = parkedSeat{path: path, snap: metaOf(snap)}
	s.mu.Unlock()
}

// metaOf keeps what a list row and the gauge need and drops the
// conversation: the file holds that, and holding every parked seat's history
// in memory would rebuild the thing this store exists to avoid.
func metaOf(snap agent.Snapshot) agent.Snapshot {
	snap.Messages = nil
	snap.PendingToolResults = nil
	snap.Steps = nil
	snap.Todos = nil
	return snap
}

// Park unloads a live seat to disk without deleting it — the path for a
// stream that ended without its reader (client gone, slow-client kick): the
// driver must stop, and the conversation has to survive. Returns false when
// the id is not live.
func (s *Store) Park(id string) bool {
	s.mu.Lock()
	e, ok := s.sessions[id]
	if ok {
		delete(s.sessions, id)
	}
	s.mu.Unlock()
	if !ok {
		return false
	}
	s.unload(id, e)
	return true
}

// Hub returns the session's event hub, refreshing its idle timer.
func (s *Store) Hub(id string) (*hub, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.sessions[id]
	if !ok {
		return nil, ErrNotFound
	}
	e.lastActive = time.Now()
	return e.hub, nil
}

// StoreRef is one seat as the list and the wire Session see it: Session is
// set for a live seat, Snap for one that is only on disk.
type StoreRef struct {
	ID         string
	Session    *agent.Session
	Created    time.Time
	LastActive time.Time
	// Snap is the disk record when the seat is parked (Messages nil), and
	// nil when it is live.
	Snap *agent.Snapshot
}

// Ref returns the full record for id, refreshing its idle timer. Live only —
// see Get.
func (s *Store) Ref(id string) (StoreRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.sessions[id]
	if !ok {
		return StoreRef{}, ErrNotFound
	}
	e.lastActive = time.Now()
	e.sess.MarkActive(e.lastActive)
	return StoreRef{ID: id, Session: e.sess, Created: e.created, LastActive: e.lastActive}, nil
}

// ParkedSeat returns the disk record for a seat that is not in memory — the
// metadata plus the dump's path, so adoption can read the conversation back.
type ParkedSeat struct {
	Path string
	Snap agent.Snapshot
}

// Parked looks up a disk-only seat. It does not touch the file: the caller
// decides whether to adopt it.
func (s *Store) Parked(id string) (ParkedSeat, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.parked[id]
	if !ok {
		return ParkedSeat{}, false
	}
	return ParkedSeat{Path: p.path, Snap: p.snap}, true
}

// DropParked forgets a disk seat — its file is gone or it was adopted and
// could not be restored.
func (s *Store) DropParked(id string) {
	s.mu.Lock()
	delete(s.parked, id)
	s.mu.Unlock()
}

// ParkedRef renders a disk-only seat as a list record.
func (s *Store) ParkedRef(id string) (StoreRef, error) {
	p, ok := s.Parked(id)
	if !ok {
		return StoreRef{}, ErrNotFound
	}
	snap := p.Snap
	created, lastActive := snap.CreatedAt, snap.LastActive
	if lastActive.IsZero() {
		lastActive = snap.SavedAt
	}
	if created.IsZero() {
		created = lastActive
	}
	return StoreRef{ID: id, Created: created, LastActive: lastActive, Snap: &snap}, nil
}

// List snapshots every known seat — live first-class, parked from disk —
// most recently active first (web ListSessions). A parked seat reports the
// state its dump carries, so a restart (or an idle unload) changes nothing
// the list shows except the run badge.
func (s *Store) List() []StoreRef {
	s.mu.Lock()
	out := make([]StoreRef, 0, len(s.sessions)+len(s.parked))
	for id, e := range s.sessions {
		out = append(out, StoreRef{ID: id, Session: e.sess, Created: e.created, LastActive: e.lastActive})
	}
	for id, p := range s.parked {
		if _, live := s.sessions[id]; live {
			continue
		}
		snap := p.snap
		created, lastActive := snap.CreatedAt, snap.LastActive
		if lastActive.IsZero() {
			lastActive = snap.SavedAt
		}
		if created.IsZero() {
			created = lastActive
		}
		out = append(out, StoreRef{ID: id, Created: created, LastActive: lastActive, Snap: &snap})
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].LastActive.After(out[j].LastActive) })
	return out
}

// Len reports the live session count (what this process is actually holding).
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// SeatIDs reports every id the store knows, live or parked — the SAVED list
// uses it to hide a snapshot whose conversation is already open as a seat.
func (s *Store) SeatIDs() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]bool, len(s.sessions)+len(s.parked))
	for id := range s.sessions {
		out[id] = true
	}
	for id := range s.parked {
		out[id] = true
	}
	return out
}

// Sessions snapshots the live sessions for aggregation (/metrics). The
// snapshot is taken under the lock; reading each session's stats after is
// race-free (Stats copies under the session's own lock).
func (s *Store) Sessions() []*agent.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*agent.Session, 0, len(s.sessions))
	for _, e := range s.sessions {
		out = append(out, e.sess)
	}
	return out
}

// IDOf resolves a session back to its seat id — the approval bridge gets a
// *Session from the tool context but the wire speaks ids. "" when the
// session already left the store (reaped/deleted under the caller).
func (s *Store) IDOf(sess *agent.Session) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, e := range s.sessions {
		if e.sess == sess {
			return id
		}
	}
	return ""
}

// Reap unloads sessions idle longer than the TTL to disk. Returns how many
// were unloaded. It is a no-op when ttl<=0.
//
// A running prompt keeps its seat live no matter how old lastActive is: a
// long run is work in progress, not an abandoned conversation.
func (s *Store) Reap() int {
	if s.ttl <= 0 {
		return 0
	}
	s.mu.Lock()
	type victim struct {
		id string
		e  *entry
	}
	var doomed []victim
	deadline := time.Now().Add(-s.ttl)
	for id, e := range s.sessions {
		if e.lastActive.Before(deadline) && !e.sess.Busy() {
			doomed = append(doomed, victim{id: id, e: e})
			delete(s.sessions, id)
		}
	}
	s.mu.Unlock()
	for _, v := range doomed {
		s.unload(v.id, v.e)
	}
	return len(doomed)
}

// CloseAll unloads every session to disk and stops further creates. The
// conversations are NOT archived: the dump is the seat, so the next start
// re-adopts the same sessions under the same ids instead of turning each
// shutdown into a new SAVED row.
func (s *Store) CloseAll() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	all := s.sessions
	s.sessions = make(map[string]*entry)
	s.mu.Unlock()
	for id, e := range all {
		s.unload(id, e)
	}
}

// RunReaper periodically unloads idle sessions until stop is closed.
func (s *Store) RunReaper(interval time.Duration, stop <-chan struct{}) {
	if s.ttl <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.Reap()
		case <-stop:
			return
		}
	}
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is unrecoverable in practice; fall back to a
		// time-based id so the server keeps serving.
		return hex.EncodeToString([]byte(time.Now().Format("20060102150405.000000000")))
	}
	return hex.EncodeToString(b[:])
}
