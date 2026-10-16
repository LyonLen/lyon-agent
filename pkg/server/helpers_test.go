package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestMain pins $LYON_HOME to a scratch dir for the whole package: the
// store's delete/reap/shutdown paths archive snapshots to disk (Phase A),
// and no test may touch the real ~/.lyon. Individual tests override with
// t.Setenv when they need their own root; both stack fine.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "lyon-server-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "scratch LYON_HOME:", err)
		os.Exit(1)
	}
	os.Setenv("LYON_HOME", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// testClient is a thin httptest wrapper for route tests that must not hit
// the Anthropic API (none of these endpoints do without a message, and the
// message paths are rejected before Submit or covered by live acceptance).
type testClient struct {
	ts *httptest.Server
}

func newHTTPTestServer(t *testing.T, s *Server) *testClient {
	t.Helper()
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return &testClient{ts: ts}
}

func (c *testClient) get(path string) (int, string) {
	resp, err := http.Get(c.ts.URL + path)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (c *testClient) postJSON(path, body string) (int, string) {
	resp, err := http.Post(c.ts.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (c *testClient) del(path string) (int, string) {
	req, _ := http.NewRequest(http.MethodDelete, c.ts.URL+path, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func jsonGetString(t *testing.T, body, key string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("bad JSON %q: %v", body, err)
	}
	v, _ := m[key].(string)
	return v
}
