// Package server exposes the lyon engine over HTTP + SSE: one endpoint per
// prompt, a session store with admission control and idle eviction, and a
// slow-client-proof streaming writer.
package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
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

// Create registers a session under a fresh random id.
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
	s.sessions[id] = &entry{sess: sess, created: now, lastActive: now}
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
		e.sess.Close()
	}
	return ok
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
		e.sess.Close()
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
		e.sess.Close()
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
