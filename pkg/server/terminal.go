package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aymanbagabas/go-pty"
	"github.com/coder/websocket"

	"lyon-agent/pkg/tools"
)

// Terminal bridge: GET /terminal?session=<id> upgrades to a WebSocket and
// pins to a per-session PTY. Protocol mirrors stereo-claw-cloud's terminal
// channel (minus the k8s/tmux layers — lyon is one local process):
//
//	binary  client→server: stdin bytes        server→client: PTY output
//	text    client→server: {"type":"resize","cols","rows"} |
//	                        {"type":"close"} | {"type":"ping"}
//	text    server→client: {"type":"exited"} | {"type":"pong"} |
//	                        {"type":"error","message"}
//
// One shell per (chat session, terminal pane): ?session=<sid>&pane=<leafId>
// keys the hub as sid~pane, so a session may open any number of terminal
// panes without them stealing each other. The shell belongs to that pair —
// closing the pane, a refresh, or hopping sessions never kills it (ring
// replay on return); only idle reaping, session deletion, or lyond exiting
// does. The pane param is optional: without it the bare session id keys a
// single shell (tests, single-pane clients).
// Detaching does NOT kill the shell — output rings in a bounded buffer
// and a reconnect replays the tail. Only idle reaping, an explicit
// close, or lyond exiting ends the shell.
const (
	// termIdleTimeout reaps a session nobody reattached to. Generous on
	// purpose: a browser refresh or laptop lid should never cost a shell.
	termIdleTimeout = 30 * time.Minute
	// termRingBytes bounds the replay buffer per session (tail only).
	termRingBytes = 256 << 10
	// termWriteWait caps one WebSocket write so a wedged client cannot
	// pin the session's write lock (and with it the pump) forever.
	termWriteWait = 10 * time.Second
	// termMaxSessions caps concurrent live shells — a runaway pane loop
	// must not spawn unbounded processes.
	termMaxSessions = 32
)

var termIDPat = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// termCtl is a client→server text frame.
type termCtl struct {
	Type string `json:"type"` // "resize" | "close" | "ping"
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
}

// termNote is a server→client text frame.
type termNote struct {
	Type    string `json:"type"` // "exited" | "pong" | "error"
	Message string `json:"message,omitempty"`
}

// termSession is one live PTY plus its attach state.
//
// Locking: sess.mu serializes all state AND every WebSocket write for
// this session — the output pump, control frames, and replay all write
// under it, which satisfies coder/websocket's "one writer at a time"
// contract without a separate writer goroutine. Writes are deadline-
// bounded (termWriteWait), so holding the lock across one is safe.
// PTY I/O is exempt: reads happen only in pump, stdin/resize only in the
// session's handler goroutine, and those are different pipe ends.
type termSession struct {
	id      string
	hub     *termHub
	pty     pty.Pty
	cmd     *pty.Cmd
	ptyOnce sync.Once // closePTY: go-pty's Close is not idempotent

	mu      sync.Mutex
	conn    *websocket.Conn // attached browser; nil = detached
	ring    []byte          // output tail captured while it matters
	exited  bool            // shell process ended
	closing bool            // explicit teardown in flight
	idle    *time.Timer     // armed while detached
}

// closePTY closes the PTY exactly once. go-pty's conPty.Close is NOT
// idempotent: a second call re-enters ClosePseudoConsole with the stale
// handle — a use-after-free in conhost that corrupts heaps under churn
// (the test-suite killer; a production crash waiting for the right
// exit-vs-kill race). Both kill() and awaitExit() legitimately reach the
// close (a naturally exited shell is later killLocked'd by the reapper
// or the reconnect rebuild), so the once-guard lives here.
func (s *termSession) closePTY() {
	s.ptyOnce.Do(func() { _ = s.pty.Close() })
}

// termHub owns the session map. Lock order everywhere: hub.mu → sess.mu
// (never the reverse).
type termHub struct {
	mu        sync.Mutex
	sess      map[string]*termSession
	workspace string
}

func newTermHub(workspace string) *termHub {
	return &termHub{sess: map[string]*termSession{}, workspace: workspace}
}

// attach resolves or creates the session for id and pins conn to it.
// Replay happens inside reattach, under sess.mu, so buffered output can
// never interleave with a pump write. An exited session is rebuilt — a
// client returning to a dead shell wants a live one.
func (h *termHub) attach(id string, conn *websocket.Conn) (*termSession, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s := h.sess[id]; s != nil && s.alive() {
		if err := s.reattach(conn); err != nil {
			return nil, err
		}
		return s, nil
	}
	if s := h.sess[id]; s != nil {
		// Dead shell: reap it now and fall through to a fresh one.
		h.killLocked(s)
	}
	if len(h.sess) >= termMaxSessions {
		return nil, fmt.Errorf("terminal session limit (%d) reached", termMaxSessions)
	}
	s, err := h.startLocked(id)
	if err != nil {
		return nil, err
	}
	h.sess[id] = s
	if err := s.reattach(conn); err != nil {
		h.killLocked(s)
		return nil, err
	}
	return s, nil
}

// alive reports whether the shell is usable (lock-local snapshot).
func (s *termSession) alive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.exited && !s.closing
}

// startLocked spawns the shell on a fresh PTY. Caller holds h.mu.
func (h *termHub) startLocked(id string) (*termSession, error) {
	shell, args, err := tools.InteractiveCommand()
	if err != nil {
		return nil, err
	}
	p, err := pty.New()
	if err != nil {
		return nil, fmt.Errorf("open pty: %w", err)
	}
	c := p.Command(shell, args...)
	if h.workspace != "" {
		c.Dir = h.workspace
	}
	if err := c.Start(); err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("start %s: %w", shell, err)
	}
	_ = p.Resize(80, 24) // sane boot size; the client's fit lands right after
	s := &termSession{id: id, hub: h, pty: p, cmd: c}
	go s.pump()
	go s.awaitExit()
	return s, nil
}

// reattach pins conn, cancels the idle timer, supersedes any stale conn,
// and replays the ring — all under sess.mu so ordering vs the pump holds.
func (s *termSession) reattach(conn *websocket.Conn) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idle != nil {
		s.idle.Stop()
		s.idle = nil
	}
	if s.closing {
		return errors.New("session is closing")
	}
	if old := s.conn; old != nil && old != conn {
		// Superseded (refresh raced the old read-loop): the old handler's
		// detach will see conn mismatch and no-op.
		_ = old.Close(websocket.StatusNormalClosure, "replaced")
	}
	s.conn = conn
	if len(s.ring) > 0 {
		if err := writeWithTimeout(conn, websocket.MessageBinary, s.ring); err != nil {
			s.conn = nil
			s.armIdleLocked()
			return fmt.Errorf("replay: %w", err)
		}
	}
	return nil
}

// detach releases conn — but ONLY if it is still the attached one (a
// refresh races the old read-loop's exit against the new attach). Arming
// the idle timer here is what keeps the shell alive across the gap.
func (s *termSession) detach(conn *websocket.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != conn {
		return // already replaced by a reconnect
	}
	s.conn = nil
	if !s.closing {
		s.armIdleLocked()
	}
}

func (s *termSession) armIdleLocked() {
	if s.idle != nil {
		s.idle.Stop()
	}
	s.idle = time.AfterFunc(termIdleTimeout, func() { s.hub.reap(s) })
}

// reap is the idle timer callback, hub-serialized. Re-checks everything
// under lock: an attach may have raced the timer (Stop is not
// synchronous with a firing), and only a genuinely detached, non-closing
// session dies.
func (h *termHub) reap(s *termSession) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sess[s.id] != s {
		return // replaced or already deleted
	}
	s.mu.Lock()
	busy := s.conn != nil || s.closing
	s.mu.Unlock()
	if busy {
		return
	}
	h.killLocked(s)
}

// closeSess handles the client's explicit {"type":"close"}.
func (h *termHub) closeSess(s *termSession) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sess[s.id] != s {
		return
	}
	h.killLocked(s)
}

// closeAll kills every session — lyond itself is exiting. A ConPTY child
// does not die with its parent on Windows (no kill-on-close job object),
// so without this each lyond exit leaves orphaned shells nobody owns.
func (h *termHub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.sess {
		h.killLocked(s) // deletes from the map; safe during range
	}
}

// killBySession tears down every shell a chat session owns — one per
// (session, pane) pair, so a session that opened three terminal panes
// leaves three shells. Called when the chat session is deleted; the idle
// reaper would otherwise keep them warm for another 30 minutes.
func (h *termHub) killBySession(sid string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	prefix := sid + "~"
	for id, s := range h.sess {
		if strings.HasPrefix(id, prefix) {
			h.killLocked(s)
		}
	}
}

// killLocked tears a session down while holding h.mu (kill itself only
// takes sess.mu — the hub → sess order holds).
func (h *termHub) killLocked(s *termSession) {
	delete(h.sess, s.id)
	s.kill()
}

// kill stops the shell and severs any attached conn.
func (s *termSession) kill() {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return
	}
	s.closing = true
	conn := s.conn
	s.conn = nil
	s.mu.Unlock()

	if conn != nil {
		_ = conn.Close(websocket.StatusPolicyViolation, "session closed")
	}
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	s.closePTY() // unblocks pump and awaitExit; go-pty finalizes ConPTY
}

// pump owns PTY→client. Every chunk lands in the ring (replay material
// for the next attach) and, while attached, on the wire. A failed write
// releases the conn so the idle clock starts — reconnect still replays.
func (s *termSession) pump() {
	buf := make([]byte, 32<<10)
	for {
		n, err := s.pty.Read(buf)
		if n > 0 {
			s.mu.Lock()
			s.ring = appendRing(s.ring, buf[:n], termRingBytes)
			if s.conn != nil && !s.closing {
				if werr := writeWithTimeout(s.conn, websocket.MessageBinary, buf[:n]); werr != nil {
					_ = s.conn.Close(websocket.StatusGoingAway, "write failed")
					s.conn = nil
					s.armIdleLocked()
				}
			}
			s.mu.Unlock()
		}
		if err != nil {
			return // shell exited or session killed — awaitExit owns the rest
		}
	}
}

// awaitExit waits for the shell, tells the attached client, and releases
// the ConPTY handles. The session entry stays until detach/idle/close
// collects it, so a reconnect sees exited and rebuilds.
func (s *termSession) awaitExit() {
	_ = s.cmd.Wait()
	s.mu.Lock()
	s.exited = true
	if s.conn != nil && !s.closing {
		_ = writeWithTimeout(s.conn, websocket.MessageText, mustJSON(termNote{Type: "exited"}))
	}
	s.mu.Unlock()
	s.closePTY()
}

// stdin writes client input to the PTY. Handler-goroutine only (with
// resize), which keeps writes to the input pipe serialized.
func (s *termSession) stdin(p []byte) error {
	_, err := s.pty.Write(p)
	return err
}

// resize applies the client's viewport to the PTY.
func (s *termSession) resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return nil
	}
	return s.pty.Resize(cols, rows)
}

// note sends one control frame, write-serialized with the pump.
func (s *termSession) note(n termNote) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil && !s.closing {
		_ = writeWithTimeout(s.conn, websocket.MessageText, mustJSON(n))
	}
}

// appendRing keeps the tail: append, then trim from the front when over
// cap. The O(n) trim happens only once full — negligible next to a
// shell's output rate.
func appendRing(ring, p []byte, capBytes int) []byte {
	ring = append(ring, p...)
	if len(ring) > capBytes {
		ring = append(ring[:0], ring[len(ring)-capBytes:]...)
	}
	return ring
}

func writeWithTimeout(c *websocket.Conn, typ websocket.MessageType, p []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), termWriteWait)
	defer cancel()
	return c.Write(ctx, typ, p)
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"type":"error"}`)
	}
	return b
}

// handleTerminal is the HTTP upgrade entry. Auth rides Handler()'s bearer
// wrapper, which accepts ?token= as well (browsers cannot set headers on
// a WebSocket handshake — the same constraint SCC works around the same
// way).
func (s *Server) handleTerminal(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("session")
	if !termIDPat.MatchString(sid) {
		writeErr(w, http.StatusBadRequest, errors.New("session id must match [A-Za-z0-9_-]{1,64}"))
		return
	}
	// One shell per (session, pane): the same chat session may open any
	// number of terminal panes, each with its own ConPTY. The composite
	// key keeps them apart; without a pane param the id is the bare
	// session (single-shell clients and the tests).
	id := sid
	if pane := r.URL.Query().Get("pane"); pane != "" {
		if !termIDPat.MatchString(pane) {
			writeErr(w, http.StatusBadRequest, errors.New("pane id must match [A-Za-z0-9_-]{1,64}"))
			return
		}
		id = sid + "~" + pane
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The dev vite proxy rewrites Host, so Origin (localhost:5183) and
		// Host (127.0.0.1:8080) never match; production serves same-origin.
		// The query token carries authentication — an attacker page cannot
		// read it, so origin checks would add nothing.
		InsecureSkipVerify: true,
	})
	if err != nil {
		return // Accept already wrote the handshake error
	}
	defer conn.Close(websocket.StatusNormalClosure, "bye")

	sess, err := s.term.attach(id, conn)
	if err != nil {
		_ = writeWithTimeout(conn, websocket.MessageText,
			mustJSON(termNote{Type: "error", Message: err.Error()}))
		_ = conn.Close(websocket.StatusPolicyViolation, err.Error())
		return
	}
	defer sess.detach(conn)

	// Read loop: stdin / resize / close / ping. Browsers have no ping
	// frame API, so the client's {"type":"ping"} keeps idle proxies
	// honest and sess.note answers pong — every write goes through
	// sess.mu, so frames never interleave with pump output.
	for {
		typ, data, err := conn.Read(r.Context())
		if err != nil {
			return // client gone — detach arms the idle clock
		}
		if typ == websocket.MessageBinary {
			if len(data) > 0 && sess.stdin(data) != nil {
				return // PTY gone (killed mid-write)
			}
			continue
		}
		var ctl termCtl
		if err := json.Unmarshal(data, &ctl); err != nil {
			continue // tolerate junk rather than dropping the shell
		}
		switch ctl.Type {
		case "resize":
			_ = sess.resize(ctl.Cols, ctl.Rows)
		case "close":
			s.term.closeSess(sess)
			return
		case "ping":
			sess.note(termNote{Type: "pong"})
		}
	}
}
