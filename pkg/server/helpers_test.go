package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
