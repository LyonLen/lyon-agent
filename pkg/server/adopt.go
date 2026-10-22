package server

import (
	"errors"

	"lyon-agent/pkg/agent"
)

// acquire resolves a seat id to a live session, adopting a disk-only seat
// back into memory on first touch. This is what makes the dump the source of
// truth: List reports a parked seat, the client clicks it, and the very next
// RPC reads the conversation back from disk under the SAME id — no new seat,
// no duplicate row, no 404 for a session the list just showed.
//
// Adoption is per-request work, not a startup sweep: 500 idle conversations
// cost 500 small files, not 500 live sessions. It is safe to race — two
// callers may both read the file, Register accepts exactly one, and the loser
// closes its copy and returns the winner's session.
func (s *Server) acquire(id string) (*agent.Session, error) {
	if sess, err := s.store.Get(id); err == nil {
		return sess, nil
	}
	p, ok := s.store.Parked(id)
	if !ok {
		return nil, ErrNotFound
	}
	snap, err := agent.LoadSnapshotFrom(p.Path)
	if err != nil {
		// Corrupt dump: the file is still the seat (the next write may
		// repair it), so keep it parked and surface the damage.
		return nil, err
	}
	if snap == nil {
		// Missing or empty — nothing left to adopt.
		s.store.DropParked(id)
		return nil, ErrNotFound
	}
	sess := s.eng.NewSession(s.obs)
	sess.Restore(*snap) // no in-flight prompt on a rebuilt seat
	if err := s.store.Register(sess, id); err != nil {
		sess.Close()
		if errors.Is(err, errSeatExists) {
			// Another request adopted this seat first; its session is the
			// one the client should talk to.
			if live, gerr := s.store.Get(id); gerr == nil {
				return live, nil
			}
		}
		return nil, err
	}
	// Seed the hub with the restored conversation, exactly like
	// RestoreSession: GetHistory and WatchEvents(from_seq=-1) then narrate
	// the seat's whole history with one wire shape for live and adopted
	// seats alike.
	if h, herr := s.store.Hub(id); herr == nil {
		h.injectAll(synthSnapshotEvents(snap.Messages))
	}
	return sess, nil
}

// seatTurns reads a seat's completed-turn accounting without adopting it:
// live from the session, parked from the dump's metadata. Keeps stats
// polling (and the web's per-session footer) off the adoption path.
func (s *Server) seatTurns(id string) ([]agent.TurnStat, error) {
	if sess, err := s.store.Get(id); err == nil {
		return sess.Stats(), nil
	}
	if p, ok := s.store.Parked(id); ok {
		return p.Snap.Turns, nil
	}
	return nil, ErrNotFound
}

// seatRef renders a seat for GetSession whether it is live or on disk. Without
// the parked branch, selecting a row that the reaper had unloaded would answer
// NotFound and the UI would flash "session gone" for a conversation that is
// perfectly intact.
func (s *Server) seatRef(id string) (StoreRef, error) {
	if ref, err := s.store.Ref(id); err == nil {
		return ref, nil
	}
	return s.store.ParkedRef(id)
}

// hubOf is the event-stream entry point: it adopts a parked seat first,
// because the hub ring only exists in memory. A client that subscribes is by
// definition using the conversation, so reading it back is the right cost.
func (s *Server) hubOf(id string) (*hub, error) {
	if h, err := s.store.Hub(id); err == nil {
		return h, nil
	}
	if _, err := s.acquire(id); err != nil {
		return nil, err
	}
	return s.store.Hub(id)
}
