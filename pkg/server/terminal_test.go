package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// newTermTestServer builds the smallest Server that can host /terminal: the
// mux, the hub rooted at a scratch dir, and a bearer token. No Engine —
// handleTerminal never touches it.
func newTermTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	s := &Server{
		mux:   http.NewServeMux(),
		term:  newTermHub(t.TempDir()),
		token: "e2e-tok",
	}
	s.mux.HandleFunc("GET /terminal", s.handleTerminal)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func termWSURL(base, query string) string {
	return "ws" + strings.TrimPrefix(base, "http") + "/terminal" + query
}

// termReadUntil drains server frames until needle shows up in the binary
// stream (text control frames are skipped).
func termReadUntil(t *testing.T, c *websocket.Conn, needle string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var got []byte
	for !bytes.Contains(got, []byte(needle)) {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %q; got %q", needle, got)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		typ, data, err := c.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("read while waiting for %q: %v (so far %q)", needle, err, got)
		}
		if typ == websocket.MessageBinary {
			got = append(got, data...)
		}
	}
}

func TestTerminalEchoDetachReplayClose(t *testing.T) {
	s, ts := newTermTestServer(t)
	ctx := context.Background()

	// Echo roundtrip: the command text's own echo contains the literal
	// $((6*7)), so "lyon-e2e-42" proves the shell EXECUTED it — the input
	// echo alone can never produce the marker.
	c1, resp, err := websocket.Dial(ctx, termWSURL(ts.URL, "?session=e2e-1&token=e2e-tok"), nil)
	if err != nil {
		t.Fatalf("dial: %v (resp %v)", err, resp)
	}
	if err := c1.Write(ctx, websocket.MessageBinary, []byte("printf lyon-e2e-$((6*7))\n")); err != nil {
		t.Fatalf("stdin: %v", err)
	}
	termReadUntil(t, c1, "lyon-e2e-42", 20*time.Second)

	// Ping rides the text channel; pong must come back as text.
	if err := c1.Write(ctx, websocket.MessageText, []byte(`{"type":"ping"}`)); err != nil {
		t.Fatalf("ping: %v", err)
	}
	for {
		pctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		typ, data, err := c1.Read(pctx)
		cancel()
		if err != nil {
			t.Fatalf("read pong: %v", err)
		}
		if typ == websocket.MessageText && bytes.Contains(data, []byte(`"pong"`)) {
			break
		}
	}

	// Detach must NOT kill the shell: reconnect replays the ring tail.
	c1.CloseNow()
	c2, resp, err := websocket.Dial(ctx, termWSURL(ts.URL, "?session=e2e-1&token=e2e-tok"), nil)
	if err != nil {
		t.Fatalf("re-dial: %v (resp %v)", err, resp)
	}
	termReadUntil(t, c2, "lyon-e2e-42", 10*time.Second)

	// Still the same shell (not a respawn): a fresh marker lands live.
	if err := c2.Write(ctx, websocket.MessageBinary, []byte("printf alive-$((3*3))\n")); err != nil {
		t.Fatalf("stdin 2: %v", err)
	}
	termReadUntil(t, c2, "alive-9", 10*time.Second)

	// Explicit close tears the session down for good.
	if err := c2.Write(ctx, websocket.MessageText, []byte(`{"type":"close"}`)); err != nil {
		t.Fatalf("close: %v", err)
	}
	gone := time.Now().Add(5 * time.Second)
	for {
		s.term.mu.Lock()
		n := len(s.term.sess)
		s.term.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(gone) {
			t.Fatalf("session still in hub after close: %d", n)
		}
		time.Sleep(50 * time.Millisecond)
	}
	c2.CloseNow()
}

func TestTerminalAuthAndValidation(t *testing.T) {
	_, ts := newTermTestServer(t)
	ctx := context.Background()

	// No token → the bearer wrapper rejects before the upgrade.
	_, resp, err := websocket.Dial(ctx, termWSURL(ts.URL, "?session=e2e-2"), nil)
	if err == nil {
		t.Fatal("dial without token succeeded")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got resp=%v err=%v", resp, err)
	}

	// Wrong token → same verdict (constant-time compare).
	_, resp, err = websocket.Dial(ctx, termWSURL(ts.URL, "?session=e2e-2&token=wrong"), nil)
	if err == nil {
		t.Fatal("dial with wrong token succeeded")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got resp=%v err=%v", resp, err)
	}

	// Session id charset: the id doubles as a layout nodeId — no slashes.
	_, resp, err = websocket.Dial(ctx, termWSURL(ts.URL, "?session=../etc&token=e2e-tok"), nil)
	if err == nil {
		t.Fatal("dial with bad session id succeeded")
	}
	if resp == nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got resp=%v err=%v", resp, err)
	}
}
