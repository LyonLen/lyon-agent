package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lyon-agent/pkg/agent"
)

// newAuthServer builds a token-guarded server plus its test URL.
func newAuthServer(t *testing.T, token string) (string, *Server) {
	t.Helper()
	s := New(agent.NewEngine(agent.DefaultConfig(), nil), NewStore(0, 0))
	s.SetToken(token)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts.URL, s
}

func doReq(t *testing.T, method, url, bearer string, body string) (int, string) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// lyond runs shell and file writes through these routes: without a token
// every one of them is anonymous remote code execution.
func TestAuthRejectsAnonymous(t *testing.T) {
	base, _ := newAuthServer(t, "s3cret")

	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/metrics"},
		{http.MethodGet, "/v1/sessions/nope/stats"},
		{http.MethodPost, "/v1/sessions"},
		{http.MethodPost, "/v1/sessions/nope/messages"},
		{http.MethodDelete, "/v1/sessions/nope"},
	} {
		code, body := doReq(t, tc.method, base+tc.path, "", `{"message":"hi"}`)
		if code != http.StatusUnauthorized {
			t.Errorf("%s %s without token: status %d (want 401): %s", tc.method, tc.path, code, body)
		}
		if !contains(body, "token") {
			t.Errorf("%s %s body should name the token: %s", tc.method, tc.path, body)
		}
	}
}

func TestAuthWrongToken(t *testing.T) {
	base, _ := newAuthServer(t, "s3cret")
	code, _ := doReq(t, http.MethodPost, base+"/v1/sessions", "wrong", "")
	if code != http.StatusUnauthorized {
		t.Fatalf("wrong token: status %d (want 401)", code)
	}
}

func TestAuthAcceptsBearerAndHeader(t *testing.T) {
	base, _ := newAuthServer(t, "s3cret")

	code, body := doReq(t, http.MethodPost, base+"/v1/sessions", "s3cret", "")
	if code != http.StatusCreated {
		t.Fatalf("bearer: status %d (want 201): %s", code, body)
	}
	if id := jsonGetString(t, body, "session_id"); id == "" {
		t.Fatalf("no session id: %s", body)
	}

	req, _ := http.NewRequest(http.MethodGet, base+"/metrics", nil)
	req.Header.Set("X-Lyon-Token", "s3cret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("X-Lyon-Token: status %d (want 200)", resp.StatusCode)
	}
}

// Liveness probes stay open so orchestrators can check readiness without
// holding the token — /healthz leaks no session or usage data.
func TestAuthLeavesHealthOpen(t *testing.T) {
	base, _ := newAuthServer(t, "s3cret")
	code, body := doReq(t, http.MethodGet, base+"/healthz", "", "")
	if code != http.StatusOK {
		t.Fatalf("healthz without token: status %d (want 200): %s", code, body)
	}
}

// No token configured = legacy behaviour, nothing to wrap.
func TestAuthDisabledWithoutToken(t *testing.T) {
	base, _ := newAuthServer(t, "")
	code, body := doReq(t, http.MethodPost, base+"/v1/sessions", "", "")
	if code != http.StatusCreated {
		t.Fatalf("open server: status %d (want 201): %s", code, body)
	}
}
