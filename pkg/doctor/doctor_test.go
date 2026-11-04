package doctor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lyon-agent/pkg/memory"
)

// scratch points LYON_HOME at a fresh temp dir and returns a fresh
// workspace — every test runs against isolated roots (precedent:
// cmd/lyon/testhome_test.go's LYON_HOME discipline).
func scratch(t *testing.T) (home, ws string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("LYON_HOME", home)
	return home, t.TempDir()
}

// find returns the named check from a Run result.
func find(t *testing.T, checks []Check, name string) Check {
	t.Helper()
	for _, c := range checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %q check in %d checks", name, len(checks))
	return Check{}
}

// --- gateway -------------------------------------------------------------

// TestGatewayThreeStates pins the probe: a 200 model list counts models,
// a 500 fails with a fix, and a hung endpoint times out into a fail —
// never a hang.
func TestGatewayThreeStates(t *testing.T) {
	t.Setenv("LYON_BASE_URL", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("LYON_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("LYON_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")

	t.Run("ok counts models", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/models" {
				t.Errorf("path = %s, want /v1/models", r.URL.Path)
			}
			if r.Header.Get("x-api-key") == "" {
				t.Error("missing x-api-key header")
			}
			fmt.Fprint(w, `{"data":[{"id":"m1"},{"id":"m2"},{"id":"m3"}]}`)
		}))
		defer srv.Close()
		t.Setenv("LYON_BASE_URL", srv.URL)
		t.Setenv("LYON_API_KEY", "k")

		c := checkGateway(context.Background(), Options{})
		if c.Status != StatusOK {
			t.Fatalf("status = %v (%s), want OK", c.Status, c.Detail)
		}
		if c.Detail != "3 models" {
			t.Fatalf("detail = %q, want \"3 models\"", c.Detail)
		}
	})

	t.Run("500 fails with fix", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		defer srv.Close()
		t.Setenv("LYON_BASE_URL", srv.URL)

		c := checkGateway(context.Background(), Options{})
		if c.Status != StatusFail {
			t.Fatalf("status = %v, want Fail", c.Status)
		}
		if !strings.Contains(c.Detail, "HTTP 500") {
			t.Fatalf("detail = %q, want HTTP 500", c.Detail)
		}
		if c.Fix == "" {
			t.Fatal("fail must carry a fix line")
		}
	})

	t.Run("timeout fails fast", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done() // hang until the client gives up
		}))
		defer srv.Close()
		t.Setenv("LYON_BASE_URL", srv.URL)

		start := time.Now()
		c := checkGateway(context.Background(), Options{Timeout: 200 * time.Millisecond})
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Fatalf("took %s, want ~200ms", elapsed)
		}
		if c.Status != StatusFail {
			t.Fatalf("status = %v, want Fail", c.Status)
		}
		if !strings.Contains(c.Detail, "timeout") {
			t.Fatalf("detail = %q, want timeout", c.Detail)
		}
	})
}

// --- memory --------------------------------------------------------------

// TestMemoryStatsAfterSave: write a fact into a temp workspace, then
// reconcile StoreStats against it — the numbers doctor prints are real.
func TestMemoryStatsAfterSave(t *testing.T) {
	_, ws := scratch(t)

	st := memory.Open(ws)
	if _, err := st.Save(memory.Fact{
		Name: "doctor-fact", Body: "body", Scope: memory.ScopeProject,
	}); err != nil {
		t.Fatalf("save fact: %v", err)
	}
	defer st.Flush()

	c := checkMemory(context.Background(), Options{Workspace: ws})
	if c.Status != StatusOK {
		t.Fatalf("status = %v, want OK", c.Status)
	}
	if !strings.Contains(c.Detail, "project 1") {
		t.Fatalf("detail = %q, want project 1", c.Detail)
	}
	if !strings.Contains(c.Detail, "hot 0") {
		t.Fatalf("detail = %q, want hot 0", c.Detail)
	}
}

// --- git -----------------------------------------------------------------

// TestGitNonRepoIsOK: a bare directory answers "not a git repo" and must
// not fail — doctor runs in folders that were never git-initialized.
func TestGitNonRepoIsOK(t *testing.T) {
	_, ws := scratch(t)

	c := checkGit(context.Background(), Options{Workspace: ws})
	if c.Status != StatusOK {
		t.Fatalf("status = %v (%s), want OK", c.Status, c.Detail)
	}
	if !strings.Contains(c.Detail, "not a git repo") {
		t.Fatalf("detail = %q, want not a git repo", c.Detail)
	}
}

// --- mcp -----------------------------------------------------------------

// TestMCPNoServers: an empty LYON_HOME and workspace mean no mcp.json
// and no .lyon.toml — a clean "nothing configured" OK, not a failure.
func TestMCPNoServers(t *testing.T) {
	_, ws := scratch(t)

	c := checkMCP(context.Background(), Options{Workspace: ws})
	if c.Status != StatusOK {
		t.Fatalf("status = %v (%s), want OK", c.Status, c.Detail)
	}
	if !strings.Contains(c.Detail, "no servers") {
		t.Fatalf("detail = %q, want no servers", c.Detail)
	}
}

// TestMCPBadConfigEntry: a malformed mcp.json entry lands in the detail
// as a config problem and fails the check with a fix line.
func TestMCPBadConfigEntry(t *testing.T) {
	home, ws := scratch(t)
	// command and url together is mutually exclusive → one problem.
	bad := `{"mcpServers":{"both":{"command":["x"],"url":"http://y"}}}`
	if err := os.WriteFile(filepath.Join(home, "mcp.json"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}

	c := checkMCP(context.Background(), Options{Workspace: ws})
	if c.Status != StatusFail {
		t.Fatalf("status = %v, want Fail", c.Status)
	}
	if !strings.Contains(c.Detail, "mutually exclusive") {
		t.Fatalf("detail = %q, want the entry problem", c.Detail)
	}
	if c.Fix == "" {
		t.Fatal("fail must carry a fix line")
	}
}

// TestMCPDeadServer: a server whose command does not exist fails to
// connect; doctor's own ledger reports it by name.
func TestMCPDeadServer(t *testing.T) {
	home, ws := scratch(t)
	cfg := `{"mcpServers":{"ghost":{"command":["lyon-doctor-no-such-binary"]}}}`
	if err := os.WriteFile(filepath.Join(home, "mcp.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	c := checkMCP(context.Background(), Options{Workspace: ws, Timeout: 10 * time.Second})
	if c.Status != StatusFail {
		t.Fatalf("status = %v (%s), want Fail", c.Status, c.Detail)
	}
	if !strings.Contains(c.Detail, "ghost") {
		t.Fatalf("detail = %q, want the server name", c.Detail)
	}
}

// --- config --------------------------------------------------------------

// TestConfigUnknownKeyWarns: an unknown key is a warning, not a failure —
// exit code stays 0, the typo shows up in the detail.
func TestConfigUnknownKeyWarns(t *testing.T) {
	home, ws := scratch(t)
	if err := os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("model_unknown_key = \"x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := checkConfig(context.Background(), Options{Workspace: ws})
	if c.Status != StatusWarn {
		t.Fatalf("status = %v (%s), want Warn", c.Status, c.Detail)
	}
	if !strings.Contains(c.Detail, "model_unknown_key") {
		t.Fatalf("detail = %q, want the key name", c.Detail)
	}
	if c.Fix == "" {
		t.Fatal("warn must carry a fix line")
	}
}

// --- workspace -----------------------------------------------------------

// TestWorkspaceWritable: fresh temp roots are writable → OK.
func TestWorkspaceWritable(t *testing.T) {
	_, ws := scratch(t)

	c := checkWorkspace(context.Background(), Options{Workspace: ws})
	if c.Status != StatusOK {
		t.Fatalf("status = %v (%s), want OK", c.Status, c.Detail)
	}
	if !strings.Contains(c.Detail, "writable") {
		t.Fatalf("detail = %q", c.Detail)
	}
}

// TestWorkspaceMissingDirFails: a -workspace that does not exist is a
// real misconfiguration → Fail with a fix.
func TestWorkspaceMissingDirFails(t *testing.T) {
	scratch(t)
	missing := filepath.Join(t.TempDir(), "nope")

	c := checkWorkspace(context.Background(), Options{Workspace: missing})
	if c.Status != StatusFail {
		t.Fatalf("status = %v, want Fail", c.Status)
	}
	if c.Fix == "" {
		t.Fatal("fail must carry a fix line")
	}
}

// --- lyond ---------------------------------------------------------------

// TestLyondNotRunningIsSkip: a closed port is the normal first-run
// state — OK with "not running (skip)", never a failure.
func TestLyondNotRunningIsSkip(t *testing.T) {
	old := lyondAddr
	lyondAddr = "127.0.0.1:1" // nothing listens on port 1
	defer func() { lyondAddr = old }()

	c := checkLyond(context.Background(), Options{Timeout: 500 * time.Millisecond})
	if c.Status != StatusOK {
		t.Fatalf("status = %v, want OK (skip)", c.Status)
	}
	if !strings.Contains(c.Detail, "not running") {
		t.Fatalf("detail = %q, want not running", c.Detail)
	}
}

// TestLyondHealthy: a live listener answering /healthz 200 → OK with the
// session count when the body carries one.
func TestLyondHealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"status":"ok","sessions":2}`)
	}))
	defer srv.Close()

	old := lyondAddr
	lyondAddr = strings.TrimPrefix(srv.URL, "http://")
	defer func() { lyondAddr = old }()

	c := checkLyond(context.Background(), Options{})
	if c.Status != StatusOK {
		t.Fatalf("status = %v (%s), want OK", c.Status, c.Detail)
	}
	if !strings.Contains(c.Detail, "healthy") || !strings.Contains(c.Detail, "2 session") {
		t.Fatalf("detail = %q, want healthy · 2 session(s)", c.Detail)
	}
}

// TestLyondUnhealthyFails: listener up but /healthz 500 → Fail with fix.
func TestLyondUnhealthyFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "sad", http.StatusInternalServerError)
	}))
	defer srv.Close()

	old := lyondAddr
	lyondAddr = strings.TrimPrefix(srv.URL, "http://")
	defer func() { lyondAddr = old }()

	c := checkLyond(context.Background(), Options{})
	if c.Status != StatusFail {
		t.Fatalf("status = %v, want Fail", c.Status)
	}
	if c.Fix == "" {
		t.Fatal("fail must carry a fix line")
	}
}

// --- logs / evolve -------------------------------------------------------

// TestLogsPathsExistOrMissing: both markers print, path rooted at
// $LYON_HOME; nothing exists yet in a fresh scratch → (missing), still OK.
func TestLogsPathsExistOrMissing(t *testing.T) {
	home, _ := scratch(t)

	c := checkLogs(context.Background(), Options{})
	if c.Status != StatusOK {
		t.Fatalf("status = %v, want OK", c.Status)
	}
	if !strings.Contains(c.Detail, filepath.Join(home, "logs")) {
		t.Fatalf("detail = %q, want %s/logs", c.Detail, home)
	}
	if !strings.Contains(c.Detail, "(missing)") {
		t.Fatalf("detail = %q, want (missing) markers", c.Detail)
	}

	// Touch today's log → flips to (exists).
	if err := os.MkdirAll(filepath.Join(home, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(LogPath(), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	c = checkLogs(context.Background(), Options{})
	if !strings.Contains(c.Detail, "(exists)") {
		t.Fatalf("detail = %q, want (exists) after creating the log", c.Detail)
	}
}

// TestEvolveEmptyIsOK: a fresh LYON_HOME has no leaderboard and no
// routing table — both report empty, neither fails.
func TestEvolveEmptyIsOK(t *testing.T) {
	scratch(t)

	if c := checkLeaderboard(context.Background(), Options{}); c.Status != StatusOK {
		t.Fatalf("leaderboard status = %v, want OK", c.Status)
	}
	if c := checkRouting(context.Background(), Options{}); c.Status != StatusOK {
		t.Fatalf("routing status = %v, want OK", c.Status)
	}
}

// --- Run -----------------------------------------------------------------

// TestRunOrderAndConcurrency: Run returns every check in report order.
// Concurrency itself is asserted by timing: eleven checks where the
// gateway hangs for ~1.5s must finish near the slowest single check,
// not the sum (serial would be ≥ that + the rest).
func TestRunOrderAndConcurrency(t *testing.T) {
	_, ws := scratch(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(1500 * time.Millisecond):
			fmt.Fprint(w, `{"data":[{"id":"m"}]}`)
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	t.Setenv("LYON_BASE_URL", srv.URL)
	t.Setenv("LYON_API_KEY", "k")

	start := time.Now()
	checks := Run(context.Background(), Options{Workspace: ws, Timeout: 5 * time.Second})
	elapsed := time.Since(start)

	want := []string{
		"gateway", "mcp", "memory", "config", "workspace",
		"git", "sessions", "lyond", "logs", "leaderboard", "routing",
	}
	if len(checks) != len(want) {
		t.Fatalf("got %d checks, want %d", len(checks), len(want))
	}
	for i, w := range want {
		if checks[i].Name != w {
			t.Errorf("checks[%d].Name = %q, want %q", i, checks[i].Name, w)
		}
	}
	// Serial worst case ≈ 1.5s gateway + every other probe; concurrent
	// lands at the gateway's own tail. 3s leaves room for slow CI disks
	// while still failing a serial implementation (≥1.5s + probes).
	if elapsed > 3*time.Second {
		t.Fatalf("Run took %s — checks are not concurrent", elapsed)
	}
}

// TestRunWorkspaceDefault: an empty Workspace falls back to the current
// directory without erroring.
func TestRunWorkspaceDefault(t *testing.T) {
	scratch(t)
	checks := Run(context.Background(), Options{})
	if len(checks) == 0 {
		t.Fatal("no checks")
	}
	for _, c := range checks {
		if c.Name == "" {
			t.Fatal("check with empty name")
		}
	}
}
