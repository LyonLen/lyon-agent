package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"
	"time"

	"lyon-agent/internal/mockapi"
)

// TestEnvModelPrecedence: LYON_MODEL shadows ANTHROPIC_MODEL — a shell that
// exports ANTHROPIC_MODEL for Claude Code must not rename lyon's model — and
// ANTHROPIC_MODEL still works when LYON_MODEL is unset.
func TestEnvModelPrecedence(t *testing.T) {
	t.Setenv("LYON_SUBAGENT_MODEL", "")
	t.Setenv("ANTHROPIC_MODEL", "claude-code-model")
	t.Setenv("LYON_MODEL", "")
	if got := NewEngine(Config{}, nil).Config().Model; got != "claude-code-model" {
		t.Fatalf("ANTHROPIC_MODEL fallback lost: %q", got)
	}
	t.Setenv("LYON_MODEL", "lyon-own-model")
	cfg := NewEngine(Config{}, nil).Config()
	if cfg.Model != "lyon-own-model" {
		t.Fatalf("LYON_MODEL lost to ANTHROPIC_MODEL: %q", cfg.Model)
	}
	if cfg.SubagentModel != "lyon-own-model" {
		t.Fatalf("subagent tier did not follow LYON_MODEL: %q", cfg.SubagentModel)
	}
	// An explicit flag still beats both.
	if got := NewEngine(Config{Model: "flag-model"}, nil).Config().Model; got != "flag-model" {
		t.Fatalf("explicit model lost to env: %q", got)
	}
}

// TestEnvEndpointPrecedence: with both name families set, every request goes
// to LYON_BASE_URL carrying exactly one credential header, built from
// LYON_AUTH_TOKEN — the ANTHROPIC_* pair belongs to Claude Code (#54).
func TestEnvEndpointPrecedence(t *testing.T) {
	mock := mockapi.New(mockapi.Config{
		Script:  []mockapi.Turn{{Text: []string{"ok"}}},
		Latency: time.Millisecond,
	})
	t.Cleanup(mock.Close)

	var gotHeader http.Header
	mockURL, _ := url.Parse(mock.URL())
	proxy := httputil.NewSingleHostReverseProxy(mockURL)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Clone()
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)

	// The decoy: if the SDK's ANTHROPIC_* autoload wins, the request lands
	// here instead of on the proxy (and carries the wrong token).
	decoy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request reached ANTHROPIC_BASE_URL %s", r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(decoy.Close)

	t.Setenv("LYON_HOME", tempDir(t))
	t.Setenv("ANTHROPIC_BASE_URL", decoy.URL)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "claude-code-token")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("LYON_BASE_URL", ts.URL)
	t.Setenv("LYON_AUTH_TOKEN", "lyon-token")
	t.Setenv("LYON_MODEL", "lyon-model")

	eng := NewEngine(Config{Effort: "low"}, nil)
	sess := eng.NewSession(nil)
	defer sess.Close()
	if err := sess.Submit("hello"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitPromptEnd(t, sess)

	if got := gotHeader.Get("Authorization"); got != "Bearer lyon-token" {
		t.Fatalf("Authorization = %q, want the LYON_AUTH_TOKEN", got)
	}
	if got := gotHeader.Get("X-Api-Key"); got != "" {
		t.Fatalf("stray X-Api-Key = %q — two credentials were sent", got)
	}
	if body := mock.LastBody(); body != nil {
		var head struct{ Model string }
		if err := json.Unmarshal(body, &head); err == nil && head.Model != "lyon-model" {
			t.Fatalf("request model = %q, want lyon-model", head.Model)
		}
	}
}
