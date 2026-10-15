package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lyon-agent/pkg/tools"
)

// tempDir is t.TempDir with a deletion retry: on Windows the cleanup
// occasionally races a transient handle (defender/indexer) in %TEMP% and
// RemoveAll fails with "directory not empty" — same flake as cmd/lyon's
// helper, pre-cleared here so this package stays green on its own.
func tempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() {
		for i := 0; i < 20; i++ {
			if os.RemoveAll(dir) == nil {
				return
			}
			os.RemoveAll(dir)
		}
	})
	return dir
}

// writeConfig drops an mcp.json into a fresh LYON_HOME and points the env
// at it, so ConfigPath resolves inside the test instead of ~/.lyon.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	home := tempDir(t)
	t.Setenv("LYON_HOME", home)
	path := filepath.Join(home, "mcp.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadServersCommandAndURL(t *testing.T) {
	path := writeConfig(t, `{"mcpServers": {
		"http":   {"url": "http://host/mcp"},
		"argv":   {"command": ["npx", "-y", "pkg-x"], "args": ["--flag"], "env": {"B": "2", "A": "1"}},
		"string": {"command": "python", "args": ["-m", "srv"]}
	}}`)
	servers, problems := LoadServers(path)
	if len(problems) != 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
	// Sorted name order keeps mcp_<srv>_<tool> registration deterministic.
	if len(servers) != 3 || servers[0].Name != "argv" || servers[1].Name != "http" || servers[2].Name != "string" {
		t.Fatalf("servers = %+v, want [argv http string]", servers)
	}
	argv := servers[0]
	if strings.Join(argv.Command, " ") != "npx -y pkg-x --flag" {
		t.Errorf("argv command = %v, want command+args appended", argv.Command)
	}
	if strings.Join(argv.Env, ",") != "A=1,B=2" {
		t.Errorf("argv env = %v, want sorted A=1,B=2", argv.Env)
	}
	if servers[1].URL != "http://host/mcp" || len(servers[1].Command) != 0 {
		t.Errorf("http server = %+v, want url only", servers[1])
	}
	// The bare-string command shape (Claude Code's .mcp.json style).
	if strings.Join(servers[2].Command, " ") != "python -m srv" {
		t.Errorf("string command = %v, want [python -m srv]", servers[2].Command)
	}
}

func TestLoadServersMissingFile(t *testing.T) {
	servers, problems := LoadServers(filepath.Join(tempDir(t), "mcp.json"))
	if servers != nil || problems != nil {
		t.Fatalf("missing file must be silent, got servers=%v problems=%v", servers, problems)
	}
}

func TestLoadServersMalformedJSON(t *testing.T) {
	path := writeConfig(t, `{"mcpServers": {oops`)
	servers, problems := LoadServers(path)
	if servers != nil || len(problems) != 1 {
		t.Fatalf("malformed JSON: servers=%v problems=%v, want one problem", servers, problems)
	}
	if !strings.Contains(problems[0].Error(), "parse") {
		t.Errorf("problem should say parse failed, got: %v", problems[0])
	}
}

func TestLoadServersBadEntriesSkipped(t *testing.T) {
	path := writeConfig(t, `{"mcpServers": {
		"":      {"command": ["x"]},
		"both":  {"command": ["x"], "url": "http://host"},
		"neither": {},
		"good":  {"url": "http://ok"}
	}}`)
	servers, problems := LoadServers(path)
	if len(servers) != 1 || servers[0].Name != "good" {
		t.Fatalf("servers = %+v, want only the good entry", servers)
	}
	if len(problems) != 3 {
		t.Fatalf("problems = %v, want 3 (one per bad entry)", problems)
	}
}

func TestLoadServersFieldDefaults(t *testing.T) {
	path := writeConfig(t, `{"mcpServers": {"s": {"command": ["x"], "args": [], "env": {}}}}`)
	servers, problems := LoadServers(path)
	if len(problems) != 0 || len(servers) != 1 {
		t.Fatalf("servers=%v problems=%v", servers, problems)
	}
	if servers[0].Env != nil {
		t.Errorf("empty env map must yield nil Env, got %v", servers[0].Env)
	}
	if servers[0].URL != "" {
		t.Errorf("no url key must yield empty URL, got %q", servers[0].URL)
	}
}

func TestMergeServersFlagOverridesByName(t *testing.T) {
	file := []Config{
		{Name: "a", Command: []string{"file-a"}},
		{Name: "b", Command: []string{"file-b"}},
	}
	flags := []Config{
		{Name: "b", Command: []string{"flag-b"}},
		{Name: "c", URL: "http://flag-c"},
	}
	merged := MergeServers(file, flags)
	if len(merged) != 3 {
		t.Fatalf("merged = %+v, want 3 entries", merged)
	}
	if merged[0].Name != "a" || strings.Join(merged[0].Command, " ") != "file-a" {
		t.Errorf("file-only entry must survive, got %+v", merged[0])
	}
	if merged[1].Name != "b" || strings.Join(merged[1].Command, " ") != "flag-b" {
		t.Errorf("same-name flag must win, got %+v", merged[1])
	}
	if merged[2].Name != "c" {
		t.Errorf("flag-only entry must append, got %+v", merged[2])
	}
	// Inputs untouched.
	if strings.Join(file[1].Command, " ") != "file-b" || len(flags) != 2 {
		t.Error("MergeServers must not mutate its inputs")
	}
}

func TestConnectFileServersToleratesFailures(t *testing.T) {
	writeConfig(t, `{"mcpServers": {
		"dead": {"command": ["lyon-no-such-binary-for-mcp-test"]},
		"bad":  {}
	}}`)
	var warns []string
	mgr := NewManager()
	reg := tools.NewRegistry()
	ConnectFileServers(context.Background(), mgr, reg, nil, func(err error) {
		warns = append(warns, err.Error())
	})
	t.Cleanup(func() { _ = mgr.Close() })

	if len(mgr.Names()) != 0 {
		t.Fatalf("dead server must not register a connection, got %v", mgr.Names())
	}
	if len(reg.Names()) != 0 {
		t.Fatalf("no tools expected, got %v", reg.Names())
	}
	// One warn for the bad entry, one for the dead connect — startup lives.
	if len(warns) != 2 {
		t.Fatalf("warns = %v, want 2 (bad entry + dead server)", warns)
	}
	joined := strings.Join(warns, "\n")
	if !strings.Contains(joined, `"bad"`) || !strings.Contains(joined, "dead") {
		t.Errorf("warns should name both offenders, got: %v", warns)
	}
}

func TestConnectFileServersFlagOverrideSkipsFileEntry(t *testing.T) {
	// The file entry would fail to connect (no such binary); a same-name
	// flag takes over, so ConnectFileServers must not touch the file entry
	// — observable as zero warns from this path.
	writeConfig(t, `{"mcpServers": {"srv": {"command": ["lyon-no-such-binary-for-mcp-test"]}}}`)
	var warns []string
	mgr := NewManager()
	reg := tools.NewRegistry()
	ConnectFileServers(context.Background(), mgr, reg,
		[]Config{{Name: "srv", Command: []string{"echo"}}}, func(err error) {
			warns = append(warns, err.Error())
		})
	t.Cleanup(func() { _ = mgr.Close() })

	if len(warns) != 0 {
		t.Fatalf("overridden file entry must be skipped silently, got warns=%v", warns)
	}
	if len(mgr.Names()) != 0 {
		t.Fatalf("flags connect in the caller's loop, not here, got %v", mgr.Names())
	}
}

func TestConnectFileServersMissingFileIsSilent(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t)) // no mcp.json inside
	var warns []string
	mgr := NewManager()
	reg := tools.NewRegistry()
	ConnectFileServers(context.Background(), mgr, reg, nil, func(err error) {
		warns = append(warns, err.Error())
	})
	t.Cleanup(func() { _ = mgr.Close() })
	if len(warns) != 0 {
		t.Fatalf("missing config must be silent, got %v", warns)
	}
}

func TestConfigPathHonorsLYON_HOME(t *testing.T) {
	home := tempDir(t)
	t.Setenv("LYON_HOME", home)
	path, err := ConfigPath()
	if err != nil || path != filepath.Join(home, "mcp.json") {
		t.Fatalf("ConfigPath = %q, %v, want %q", path, err, filepath.Join(home, "mcp.json"))
	}

	t.Setenv("LYON_HOME", "") // fall back to ~/.lyon
	path, err = ConfigPath()
	if err != nil {
		t.Fatalf("ConfigPath fallback: %v", err)
	}
	if filepath.Base(filepath.Dir(path)) != ".lyon" || filepath.Base(path) != "mcp.json" {
		t.Fatalf("fallback path = %q, want ~/.lyon/mcp.json", path)
	}
}
