package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/mcp"
)

// mcpFlags builds the shared flag set with only the MCP flags potentially
// set, mirroring how cmdRun/cmdChat/cmdTools construct it.
func mcpFlags(t *testing.T, args ...string) commonFlags {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cf := registerFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse flags %v: %v", args, err)
	}
	return cf
}

// deadBinary is guaranteed not to exist: connecting it fails fast, which is
// how these tests reach the tolerant/fatal branches without a real server.
const deadBinary = "lyon-no-such-binary-for-mcp-test"

func writeMCPConfig(t *testing.T, body string) {
	t.Helper()
	home := tempDir(t)
	t.Setenv("LYON_HOME", home)
	if body == "" {
		return // scratch LYON_HOME without an mcp.json
	}
	if err := os.WriteFile(filepath.Join(home, "mcp.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write mcp.json: %v", err)
	}
}

// buildForTest runs buildEngine and closes whatever came back, reporting
// only the engine's tool names (empty on a nil engine).
func buildForTest(t *testing.T, cf commonFlags) (*agent.Engine, *mcp.Manager, error) {
	t.Helper()
	eng, mgr, err := cf.buildEngine(agent.DefaultConfig())
	if mgr != nil {
		t.Cleanup(func() { _ = mgr.Close() })
	}
	if eng != nil {
		t.Cleanup(eng.Close)
	}
	return eng, mgr, err
}

func TestBuildEngineNoMCPAnywhere(t *testing.T) {
	writeMCPConfig(t, "") // scratch home, no mcp.json
	eng, mgr, err := buildForTest(t, mcpFlags(t))
	if err != nil || eng == nil || mgr == nil {
		t.Fatalf("flag-less startup must stay clean: eng=%v mgr=%v err=%v", eng, mgr, err)
	}
	if len(mgr.Names()) != 0 {
		t.Fatalf("no servers expected, got %v", mgr.Names())
	}
	for _, name := range eng.Registry().Names() {
		if strings.HasPrefix(name, "mcp_") {
			t.Fatalf("built-ins only, found %s", name)
		}
	}
}

func TestBuildEngineLoadsMCPConfigFile(t *testing.T) {
	// A dead server in mcp.json must warn-and-skip, not block startup —
	// the E1 tolerance contract.
	writeMCPConfig(t, `{"mcpServers": {"dead": {"command": ["`+deadBinary+`"]}}}`)
	eng, mgr, err := buildForTest(t, mcpFlags(t))
	if err != nil || eng == nil || mgr == nil {
		t.Fatalf("dead file server must not fail startup: err=%v", err)
	}
	if len(mgr.Names()) != 0 {
		t.Fatalf("dead server must not connect, got %v", mgr.Names())
	}
}

func TestBuildEngineMalformedMCPConfigFile(t *testing.T) {
	writeMCPConfig(t, `{"mcpServers": {oops`)
	eng, _, err := buildForTest(t, mcpFlags(t))
	if err != nil || eng == nil {
		t.Fatalf("malformed mcp.json must warn and continue: err=%v", err)
	}
}

func TestBuildEngineFlagConnectFailureIsFatal(t *testing.T) {
	// Flags keep their pre-E1 contract: a dead -mcp server aborts the
	// startup (the user asked for it explicitly on the command line) —
	// the opposite of the tolerant file path above. Same name, same dead
	// binary, two contracts; the override merge itself is unit-tested in
	// pkg/mcp (TestMergeServersFlagOverridesByName).
	writeMCPConfig(t, "")
	_, _, err := buildForTest(t, mcpFlags(t, "-mcp", "srv="+deadBinary))
	if err == nil {
		t.Fatal("dead flag server must stay fatal")
	}
	if !strings.Contains(err.Error(), "srv") {
		t.Fatalf("error should name the server, got: %v", err)
	}
}
