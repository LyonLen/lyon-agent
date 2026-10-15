package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lyon-agent/pkg/config"
	"lyon-agent/pkg/mcp"
)

// writeLayer drops a lyon.toml layer (home config.toml or workspace
// .lyon.toml) into dir.
func writeLayer(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s in %s: %v", name, dir, err)
	}
}

// parseCommon builds the shared flag set the way cmdRun/cmdChat/cmdTools
// do and loads the file config on top of it.
func parseCommon(t *testing.T, args ...string) commonFlags {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cf := registerFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	cf.loadFileConfig(fs)
	return cf
}

func TestFileConfigLayersAndFlagPrecedence(t *testing.T) {
	home := tempDir(t)
	t.Setenv("LYON_HOME", home)
	writeLayer(t, home, "config.toml", `
model = "home-model"
effort = "high"
approval = "ask"
shell_allow = ["echo", "ls"]

[mcpServers.home-srv]
command = "definitely-not-a-real-binary"
`)
	ws := tempDir(t)
	writeLayer(t, ws, ".lyon.toml", `
model = "ws-model"
provider = "openai"
`)

	// workspace toml beats home toml; home-only keys survive.
	cf := parseCommon(t, "-workspace", ws)
	if *cf.model != "ws-model" {
		t.Errorf("model = %q, want ws-model", *cf.model)
	}
	if *cf.effort != "high" {
		t.Errorf("effort = %q, want high (home layer)", *cf.effort)
	}
	if !*cf.approve {
		t.Error("approve = false, want true (home approval = ask)")
	}
	if !*cf.openai {
		t.Error("openai = false, want true (workspace provider = openai)")
	}
	if *cf.shellAllow != "echo,ls" {
		t.Errorf("shell-allow = %q, want echo,ls", *cf.shellAllow)
	}
	if cf.conf == nil || len(cf.conf.MCPServers) != 1 || cf.conf.MCPServers[0].Name != "home-srv" {
		t.Errorf("conf.MCPServers = %+v, want the home-srv entry", cf.conf)
	}

	// an explicit flag beats both file layers.
	cf2 := parseCommon(t, "-workspace", ws, "-model", "flag-model", "-approve=false")
	if *cf2.model != "flag-model" {
		t.Errorf("model = %q, want flag-model (flag wins)", *cf2.model)
	}
	if *cf2.approve {
		t.Error("approve = true, want false (-approve=false wins)")
	}
}

func TestFileConfigAbsentKeepsFlagDefaults(t *testing.T) {
	// Neither file present: every flag variable must hold the same zero
	// default it had before lyon.toml existed — zero behavior change.
	t.Setenv("LYON_HOME", tempDir(t)) // scratch home, no config.toml
	ws := tempDir(t)                  // scratch workspace, no .lyon.toml

	cf := parseCommon(t, "-workspace", ws)
	if *cf.workspace != ws {
		t.Errorf("workspace = %q, want the flag value %q", *cf.workspace, ws)
	}
	if *cf.model != "" || *cf.effort != "" ||
		*cf.maxIter != 0 || *cf.shellAllow != "" || *cf.webTools != "" ||
		*cf.noWebTools || *cf.approve || *cf.subagent != "" ||
		*cf.compactTh != 0 || *cf.ctxWindow != 0 || *cf.maxTurns != 0 ||
		*cf.prewarm || *cf.openai || *cf.openaiBase != "" {
		t.Errorf("no config must leave every flag at its default: %+v", cf)
	}
	if len(*cf.mcpCmds) != 0 || len(*cf.mcpURLs) != 0 {
		t.Errorf("mcp flags must stay untouched: %v %v", cf.mcpCmds, cf.mcpURLs)
	}
	if cf.conf == nil {
		t.Fatal("conf should be set even with no files")
	}
	if len(cf.conf.Warnings) != 0 || len(cf.conf.Sources) != 0 || len(cf.conf.MCPServers) != 0 {
		t.Errorf("empty layers must yield no warnings/sources/servers: %+v", cf.conf)
	}
}

func TestFileConfigBrokenFileWarnsAndContinues(t *testing.T) {
	home := tempDir(t)
	t.Setenv("LYON_HOME", home)
	writeLayer(t, home, "config.toml", `model = "unterminated`)
	ws := tempDir(t)
	writeLayer(t, ws, ".lyon.toml", `effort = "xhigh"`)

	cf := parseCommon(t, "-workspace", ws) // must not panic or abort
	if *cf.model != "" {
		t.Errorf("model = %q, want empty (broken home file skipped)", *cf.model)
	}
	if *cf.effort != "xhigh" {
		t.Errorf("effort = %q, want xhigh (workspace layer survives)", *cf.effort)
	}
}

func TestBuildEngineTOMLServerTolerant(t *testing.T) {
	// A dead lyon.toml server must warn-and-skip like a dead mcp.json
	// server — never block startup. Both layers present at once.
	writeMCPConfig(t, `{"mcpServers": {"dead-json": {"command": ["`+deadBinary+`"]}}}`)
	cf := mcpFlags(t)
	cf.conf = &config.Config{MCPServers: []mcp.Config{
		{Name: "dead-toml", Command: []string{deadBinary}},
	}}
	eng, mgr, err := buildForTest(t, cf)
	if err != nil || eng == nil || mgr == nil {
		t.Fatalf("dead configured servers must not fail startup: err=%v", err)
	}
	if len(mgr.Names()) != 0 {
		t.Fatalf("dead servers must not connect, got %v", mgr.Names())
	}
}

func TestFileConfigApprovalExplicitness(t *testing.T) {
	// chat's TTY-default guess must be steerable from the file: ask and
	// never count as explicit, auto (and absent) do not.
	home := tempDir(t)
	t.Setenv("LYON_HOME", home)
	ws := tempDir(t)
	writeLayer(t, ws, ".lyon.toml", `approval = "ask"`)
	cf := parseCommon(t, "-workspace", ws)
	if !cf.conf.ApprovalExplicit() || !*cf.approve {
		t.Errorf("approval=ask must be explicit ask: mode=%q approve=%v", cf.conf.ApprovalMode, *cf.approve)
	}

	writeLayer(t, ws, ".lyon.toml", `approval = "auto"`)
	cf2 := parseCommon(t, "-workspace", ws)
	if cf2.conf.ApprovalExplicit() {
		t.Errorf("approval=auto must not be explicit, mode=%q", cf2.conf.ApprovalMode)
	}
	if *cf2.approve {
		t.Error("approval=auto must leave the flag default alone")
	}
}

func TestFileConfigSystemPromptPath(t *testing.T) {
	ws := tempDir(t)
	prompt := filepath.Join(ws, "prompt.txt")
	if err := os.WriteFile(prompt, []byte("custom frozen prompt"), 0o644); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	writeLayer(t, ws, ".lyon.toml", "system = "+`"`+filepath.ToSlash(prompt)+`"`)
	cf := parseCommon(t, "-workspace", ws)
	cfg, err := cf.buildConfig()
	if err != nil {
		t.Fatalf("buildConfig: %v", err)
	}
	if !strings.Contains(cfg.SystemPrompt, "custom frozen prompt") {
		t.Errorf("system prompt = %q, want the file's content", cfg.SystemPrompt)
	}
}
