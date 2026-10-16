// Package server exposes the lyon engine over HTTP + SSE: one endpoint per
// prompt, a session store with admission control and idle eviction, and a
// slow-client-proof streaming writer.
package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
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
)

// Store holds live sessions with a hard admission cap and idle TTL eviction.
type Store struct {
	mu       sync.Mutex
	max      int
	ttl      time.Duration
	sessions map[string]*entry
	closed   bool
}

type entry struct {
	sess       *agent.Session
	hub        *hub // fan-out of sess.Events(); the sole consumer
	created    time.Time
	lastActive time.Time
}

// NewStore creates a store. max<=0 means unlimited; ttl<=0 disables eviction.
func NewStore(max int, ttl time.Duration) *Store {
	return &Store{
		max:      max,
		ttl:      ttl,
		sessions: make(map[string]*entry),
	}
}

// Create registers a session under a fresh random id and starts its event
// hub — from here the hub is the session's only Events() consumer, so
// everything that wants the stream (SSE pump, WatchEvents) subscribes to
// the hub instead (web plan §2).
func (s *Store) Create(sess *agent.Session) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", ErrNotFound
	}
	if s.max > 0 && len(s.sessions) >= s.max {
		return "", ErrFull
	}
	id := newID()
	now := time.Now()
	e := &entry{sess: sess, created: now, lastActive: now}
	e.hub = newHub(sess.Events())
	s.sessions[id] = e
	go e.hub.run()
	return id, nil
}

// Get returns the session for id, refreshing its idle timer.
func (s *Store) Get(id string) (*agent.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.sessions[id]
	if !ok {
		return nil, ErrNotFound
	}
	e.lastActive = time.Now()
	return e.sess, nil
}

// Delete removes and closes a session. Returns false when unknown.
func (s *Store) Delete(id string) bool {
	s.mu.Lock()
	e, ok := s.sessions[id]
	if ok {
		delete(s.sessions, id)
	}
	s.mu.Unlock()
	if ok {
		archiveAndClose(e)
	}
	return ok
}

// archiveAndClose is every store exit path — delete, reap, shutdown.
// The conversation is archived first (best effort) so a dropped session
// stays resumable from the snapshot store instead of dying with it (web
// plan Phase A — the historical data-loss root cause). Interrupt before
// Export: histMu is held end to end by an in-flight prompt, and the TUI's
// own exit uses the same settle-then-export order. Archive failures must
// not wedge teardown — the memory copy is lost either way.
func archiveAndClose(e *entry) {
	e.sess.Interrupt()
	if _, err := e.sess.ArchiveIfNew(); err != nil {
		// Best effort by design (comment above); nothing to do with a
		// disk error on the way down.
		_ = err
	}
	e.sess.Close()
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

// StoreRef pairs a live session with its registration timestamps — the
// web Session message needs created/last_active, which Get doesn't
// surface.
type StoreRef struct {
	ID         string
	Session    *agent.Session
	Created    time.Time
	LastActive time.Time
}

// Ref returns the full record for id, refreshing its idle timer.
func (s *Store) Ref(id string) (StoreRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.sessions[id]
	if !ok {
		return StoreRef{}, ErrNotFound
	}
	e.lastActive = time.Now()
	return StoreRef{ID: id, Session: e.sess, Created: e.created, LastActive: e.lastActive}, nil
}

// List snapshots every live session with id and timestamps, most recently
// active first (web ListSessions). Race-free like Sessions: the snapshot
// is taken under the lock; per-session reads after use the session's own
// locks.
func (s *Store) List() []StoreRef {
	s.mu.Lock()
	out := make([]StoreRef, 0, len(s.sessions))
	for id, e := range s.sessions {
		out = append(out, StoreRef{ID: id, Session: e.sess, Created: e.created, LastActive: e.lastActive})
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].LastActive.After(out[j].LastActive) })
	return out
}

// Len reports the current session count.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
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

// Reap closes sessions idle longer than the TTL. Returns how many were
// reaped. It is a no-op when ttl<=0.
func (s *Store) Reap() int {
	if s.ttl <= 0 {
		return 0
	}
	s.mu.Lock()
	var doomed []*entry
	deadline := time.Now().Add(-s.ttl)
	for id, e := range s.sessions {
		if e.lastActive.Before(deadline) {
			doomed = append(doomed, e)
			delete(s.sessions, id)
		}
	}
	s.mu.Unlock()
	for _, e := range doomed {
		archiveAndClose(e)
	}
	return len(doomed)
}

// CloseAll closes every session and stops further creates.
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
	for _, e := range all {
		archiveAndClose(e)
	}
}

// RunReaper periodically evicts idle sessions until stop is closed.
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
