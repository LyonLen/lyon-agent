package config

import (
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"lyon-agent/pkg/mcp"
)

// writeConfig drops body into dir/name so tests can point Load at exact
// paths without ever touching the developer's real ~/.lyon.
func writeConfig(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func TestLoadNoFilesAnywhere(t *testing.T) {
	// Both layers absent (empty path and missing file) must yield the zero
	// Config and no error — the "no config file, no behavior change" case.
	c, err := Load("", filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatalf("missing files must not error: %v", err)
	}
	if !reflect.DeepEqual(c, Config{}) {
		t.Fatalf("want zero Config, got %+v", c)
	}
}

func TestLoadWorkspaceOverridesHome(t *testing.T) {
	dir := t.TempDir()
	home := writeConfig(t, dir, "config.toml", `
model = "home-model"
effort = "high"
approval = "ask"
prewarm = true
web_tools = "off"
shell_allow = ["echo", "ls"]
max_iter = 9

[mcpServers.shared]
command = ["home-bin"]

[mcpServers.home-only]
command = "home-only-bin"
`)
	ws := writeConfig(t, dir, ".lyon.toml", `
model = "ws-model"
approval = "never"
prewarm = false
web_tools = "auto"
shell_allow = ["git"]

[mcpServers.shared]
command = ["ws-bin"]

[mcpServers.ws-only]
url = "http://ws/mcp"
`)
	c, err := Load(home, ws)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Workspace wins where it spoke, home survives where it didn't.
	if c.Model != "ws-model" {
		t.Errorf("model = %q, want ws-model (workspace overrides)", c.Model)
	}
	if c.Effort != "high" {
		t.Errorf("effort = %q, want high (home-only key survives)", c.Effort)
	}
	if c.ApprovalMode != ApprovalNever {
		t.Errorf("approval = %q, want never", c.ApprovalMode)
	}
	if c.Prewarm {
		t.Error("prewarm = true, want false (workspace explicit false overrides)")
	}
	if c.WebTools != "" {
		t.Errorf("web_tools = %q, want \"\" (workspace auto overrides home off)", c.WebTools)
	}
	if !reflect.DeepEqual(c.ShellAllowlist, []string{"git"}) {
		t.Errorf("shell_allow = %v, want [git] (workspace overrides, no merge)", c.ShellAllowlist)
	}
	if c.MaxIter != 9 {
		t.Errorf("max_iter = %d, want 9 (home-only key survives)", c.MaxIter)
	}
	// mcpServers: per-name merge, same name replaced, unique names kept.
	if len(c.MCPServers) != 3 {
		t.Fatalf("mcpServers = %+v, want 3 servers", c.MCPServers)
	}
	byName := map[string]mcp.Config{}
	for _, s := range c.MCPServers {
		byName[s.Name] = s
	}
	if got := byName["shared"].Command; !reflect.DeepEqual(got, []string{"ws-bin"}) {
		t.Errorf("shared command = %v, want [ws-bin] (workspace overrides same name)", got)
	}
	if got := byName["home-only"].Command; !reflect.DeepEqual(got, []string{"home-only-bin"}) {
		t.Errorf("home-only command = %v, want [home-only-bin] (home survivor)", got)
	}
	if got := byName["ws-only"].URL; got != "http://ws/mcp" {
		t.Errorf("ws-only url = %q, want http://ws/mcp", got)
	}
	if len(c.Warnings) != 0 {
		t.Errorf("warnings = %v, want none", c.Warnings)
	}
	if !reflect.DeepEqual(c.Sources, []string{home, ws}) {
		t.Errorf("sources = %v, want [%s %s]", c.Sources, home, ws)
	}
}

func TestLoadHomeOnlyAndBrokenWorkspace(t *testing.T) {
	dir := t.TempDir()
	home := writeConfig(t, dir, "config.toml", `model = "home-model"`)
	ws := writeConfig(t, dir, ".lyon.toml", `model = "unterminated`)
	c, err := Load(home, ws)
	if err == nil {
		t.Fatal("broken workspace file must report an error")
	}
	if !strings.Contains(err.Error(), ws) {
		t.Errorf("error should name the broken file, got: %v", err)
	}
	// The surviving layer still applies — a broken file degrades, it does
	// not zero everything out.
	if c.Model != "home-model" {
		t.Fatalf("model = %q, want home-model (home layer survives)", c.Model)
	}
	if !reflect.DeepEqual(c.Sources, []string{home}) {
		t.Errorf("sources = %v, want only the loaded file", c.Sources)
	}
}

func TestLoadUnknownKeysWarnOnceAndSkip(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "config.toml", `
model = "m"
bogus_key = 1

[unknown_section]
inner = "x"

[mcpServers.one]
command = "c"
bogus_server_key = true
`)
	c, err := Load(p, "")
	if err != nil {
		t.Fatalf("unknown keys must not be fatal: %v", err)
	}
	if c.Model != "m" {
		t.Errorf("model = %q, want m (known key still applies)", c.Model)
	}
	wantWarns := 3
	if len(c.Warnings) != wantWarns {
		t.Fatalf("warnings = %v, want %d (one per unknown key/section)", c.Warnings, wantWarns)
	}
	joined := strings.Join(c.Warnings, "\n")
	for _, frag := range []string{`unknown key "bogus_key"`, "unknown section [unknown_section]", `unknown key "bogus_server_key"`} {
		if !strings.Contains(joined, frag) {
			t.Errorf("warnings %v missing %q", c.Warnings, frag)
		}
	}
	if !strings.Contains(joined, p) {
		t.Errorf("warnings should name the file, got %v", c.Warnings)
	}
}

func TestLoadTypeMismatchWarnsAndLeavesKeyUnset(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "config.toml", `
model = 42
prewarm = "yes"
`)
	c, err := Load(p, "")
	if err != nil {
		t.Fatalf("type mismatch must not be a parse error: %v", err)
	}
	if c.Model != "" || c.Prewarm {
		t.Errorf("mistyped keys must stay unset, got model=%q prewarm=%v", c.Model, c.Prewarm)
	}
	if len(c.Warnings) != 2 {
		t.Errorf("warnings = %v, want 2", c.Warnings)
	}
}

func TestLoadValueValidationWarnsAndDrops(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "config.toml", `
provider = "gemini"
effort = "ultra"
approval = "sometimes"
web_tools = "both"
`)
	c, err := Load(p, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Provider != "" || c.Effort != "" || c.ApprovalMode != "" || c.WebTools != "" {
		t.Errorf("invalid values must be dropped, got %+v", c)
	}
	if len(c.Warnings) != 4 {
		t.Errorf("warnings = %v, want 4 (one per invalid value)", c.Warnings)
	}
}

func TestLoadBadSyntaxNeverPanics(t *testing.T) {
	cases := map[string]string{
		"no value":          "model =",
		"unterminated str":  `model = "oops`,
		"unterminated lit":  "model = 'oops",
		"open section":      "[mcpServers\nx = 1",
		"dotted assignment": "a.b = 1",
		"duplicate key":     "model = \"a\"\nmodel = \"b\"",
		"garbage":           "????",
		"bad float":         "x = 1.2.3",
		"bad escape":        `x = "\q"`,
		"section clash":     "[a]\nb = 1\n[a.b]\nc = 2",
		"junk after value":  "x = 1 2",
		"true prefix":       "x = truex",
		"unterminated arr":  "x = [1, 2",
	}
	dir := t.TempDir()
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p := writeConfig(t, dir, "bad.toml", body)
			c, err := Load(p, "")
			if err == nil {
				t.Fatalf("input %q must fail to parse", body)
			}
			if !strings.Contains(err.Error(), "toml: line") {
				t.Errorf("error should carry a line number, got: %v", err)
			}
			if c.Model != "" || len(c.MCPServers) != 0 {
				t.Errorf("failed parse must yield no values, got %+v", c)
			}
		})
	}
}

func TestMCPCommandFormsAndEnv(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "config.toml", `
[mcpServers.argv-one]
command = ["npx", "-y", "x"]
args = ["--flag"]

[mcpServers.string-cmd]
command = "npx"
args = ["-y", "y"]

[mcpServers.http-one]
url = "http://host/mcp"

[mcpServers.string-cmd.env]
ZED = "1"
ALPHA = "2"
`)
	c, err := Load(p, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.MCPServers) != 3 {
		t.Fatalf("servers = %+v, want 3", c.MCPServers)
	}
	byName := map[string]mcp.Config{}
	for _, s := range c.MCPServers {
		byName[s.Name] = s
	}
	if got := byName["argv-one"].Command; !reflect.DeepEqual(got, []string{"npx", "-y", "x", "--flag"}) {
		t.Errorf("argv-one command = %v, want argv + args appended", got)
	}
	if got := byName["string-cmd"].Command; !reflect.DeepEqual(got, []string{"npx", "-y", "y"}) {
		t.Errorf("string-cmd command = %v, want string form + args", got)
	}
	if !reflect.DeepEqual(byName["string-cmd"].Env, []string{"ALPHA=2", "ZED=1"}) {
		t.Errorf("env = %v, want sorted K=V pairs", byName["string-cmd"].Env)
	}
	if byName["string-cmd"].URL != "" {
		t.Errorf("string-cmd url = %q, want empty", byName["string-cmd"].URL)
	}
	if got := byName["http-one"].URL; got != "http://host/mcp" {
		t.Errorf("http-one url = %q", got)
	}
	if len(byName["http-one"].Command) != 0 {
		t.Errorf("http-one command = %v, want none", byName["http-one"].Command)
	}
}

// TestMCPServersMatchMCPJSONParity pins the ROADMAP requirement: lyon.toml's
// mcpServers is the mcp.json schema. The same servers written both ways
// must decode to identical mcp.Config slices (names sorted, command+args
// appended, env sorted, same validation).
func TestMCPServersMatchMCPJSONParity(t *testing.T) {
	dir := t.TempDir()
	jsonBody := `{"mcpServers": {
	  "stdio-one": {"command": ["npx", "-y", "x"], "args": ["--flag"], "env": {"K": "V", "A": "B"}},
	  "string-cmd": {"command": "npx", "args": ["-y", "y"]},
	  "http-one": {"url": "http://host/mcp"}
	}}`
	jsonPath := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(jsonPath, []byte(jsonBody), 0o644); err != nil {
		t.Fatalf("write mcp.json: %v", err)
	}
	tomlBody := `
[mcpServers.stdio-one]
command = ["npx", "-y", "x"]
args = ["--flag"]

[mcpServers.stdio-one.env]
K = "V"
A = "B"

[mcpServers.string-cmd]
command = "npx"
args = ["-y", "y"]

[mcpServers.http-one]
url = "http://host/mcp"
`
	tomlPath := writeConfig(t, dir, ".lyon.toml", tomlBody)

	fromJSON, problems := mcp.LoadServers(jsonPath)
	if len(problems) != 0 {
		t.Fatalf("mcp.json should parse clean: %v", problems)
	}
	c, err := Load("", tomlPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Warnings) != 0 {
		t.Fatalf("toml should decode clean: %v", c.Warnings)
	}
	if !reflect.DeepEqual(c.MCPServers, fromJSON) {
		t.Fatalf("mcpServers mismatch:\n toml: %+v\n json: %+v", c.MCPServers, fromJSON)
	}
}

func TestMCPBadEntriesWarnAndSkip(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "config.toml", `
[mcpServers.good]
command = "ok-bin"

[mcpServers.both]
command = "c"
url = "http://x"

[mcpServers.neither]

[mcpServers.badenv]
command = "c"

[mcpServers.badenv.env]
N = 5
`)
	c, err := Load(p, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.MCPServers) != 1 || c.MCPServers[0].Name != "good" {
		t.Fatalf("servers = %+v, want only the good one", c.MCPServers)
	}
	if len(c.Warnings) != 3 {
		t.Errorf("warnings = %v, want 3 (both / neither / bad env)", c.Warnings)
	}
}

// flagSetForApply mirrors the shared flag surface registerFlags builds in
// cmd/lyon — the names ApplyFlags knows about.
func flagSetForApply(t *testing.T, args ...string) (*flag.FlagSet, map[string]bool) {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String("model", "", "")
	fs.String("effort", "", "")
	fs.String("workspace", "", "")
	fs.String("system", "", "")
	fs.Int("max-iter", 0, "")
	fs.String("shell-allow", "", "")
	fs.String("web-tools", "", "")
	fs.Bool("no-web-tools", false, "")
	fs.Bool("approve", false, "")
	fs.String("subagent-model", "", "")
	fs.Float64("compact-threshold", 0, "")
	fs.Int64("context-window", 0, "")
	fs.Int("max-turns", 0, "")
	fs.Bool("prewarm", false, "")
	fs.Bool("openai", false, "")
	fs.String("openai-base", "", "")
	fs.Var(&multiFlag{}, "mcp", "")
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	return fs, explicit
}

// multiFlag stands in for cmd/lyon's repeatable -mcp flag: ApplyFlags must
// ignore it (its layering lives in buildEngine), but it must exist so the
// flag set parses -mcp without error.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, "|") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func baseFileConfig() Config {
	return Config{
		Model:            "file-model",
		Effort:           "high",
		ApprovalMode:     ApprovalNever,
		MaxIter:          7,
		ShellAllowlist:   []string{"git"},
		WebTools:         "server",
		CompactThreshold: 0.5,
	}
}

func TestApplyFlagsOnlyExplicitFlagsWin(t *testing.T) {
	c := baseFileConfig()
	fs, explicit := flagSetForApply(t, "-model", "flag-model", "-approve=true")
	ApplyFlags(&c, fs, func(name string) bool { return explicit[name] })

	if c.Model != "flag-model" {
		t.Errorf("model = %q, want flag-model (explicit flag overrides file)", c.Model)
	}
	if c.ApprovalMode != ApprovalAsk {
		t.Errorf("approval = %q, want ask (-approve=true)", c.ApprovalMode)
	}
	// Untouched by flags: the file values survive.
	if c.Effort != "high" || c.MaxIter != 7 || c.WebTools != "server" ||
		c.CompactThreshold != 0.5 || !reflect.DeepEqual(c.ShellAllowlist, []string{"git"}) {
		t.Errorf("unset flags must not override file values, got %+v", c)
	}
}

func TestApplyFlagsNothingExplicitKeepsFile(t *testing.T) {
	c := baseFileConfig()
	want := baseFileConfig()
	fs, explicit := flagSetForApply(t) // no flags typed
	ApplyFlags(&c, fs, func(name string) bool { return explicit[name] })
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("no explicit flags must leave the file config untouched:\n got %+v\nwant %+v", c, want)
	}
}

func TestApplyFlagsApproveFalseAndWebToolsOrder(t *testing.T) {
	c := baseFileConfig()
	c.ApprovalMode = ApprovalAsk
	c.WebTools = "off"
	fs, explicit := flagSetForApply(t, "-approve=false")
	ApplyFlags(&c, fs, func(name string) bool { return explicit[name] })
	if c.ApprovalMode != ApprovalNever {
		t.Errorf("approval = %q, want never (-approve=false)", c.ApprovalMode)
	}

	// Both web flags typed: -web-tools wins, matching buildConfig order.
	c2 := baseFileConfig()
	fs2, explicit2 := flagSetForApply(t, "-no-web-tools", "-web-tools=client")
	ApplyFlags(&c2, fs2, func(name string) bool { return explicit2[name] })
	if c2.WebTools != "client" {
		t.Errorf("web_tools = %q, want client (last spelling wins)", c2.WebTools)
	}

	// Only the shorthand typed: off.
	c3 := baseFileConfig()
	fs3, explicit3 := flagSetForApply(t, "-no-web-tools")
	ApplyFlags(&c3, fs3, func(name string) bool { return explicit3[name] })
	if c3.WebTools != "off" {
		t.Errorf("web_tools = %q, want off", c3.WebTools)
	}
}

func TestApplyFlagsOpenAIAndShellAllow(t *testing.T) {
	c := baseFileConfig()
	fs, explicit := flagSetForApply(t, "-openai", "-shell-allow", "echo,ls ,,git")
	ApplyFlags(&c, fs, func(name string) bool { return explicit[name] })
	if c.Provider != "openai" {
		t.Errorf("provider = %q, want openai (-openai)", c.Provider)
	}
	if !reflect.DeepEqual(c.ShellAllowlist, []string{"echo", "ls", "git"}) {
		t.Errorf("shell_allow = %v, want CSV split with empties dropped", c.ShellAllowlist)
	}

	// -openai=false explicitly rejects the file's provider=openai.
	c2 := Config{Provider: "openai"}
	fs2, explicit2 := flagSetForApply(t, "-openai=false")
	ApplyFlags(&c2, fs2, func(name string) bool { return explicit2[name] })
	if c2.Provider != "anthropic" {
		t.Errorf("provider = %q, want anthropic (-openai=false)", c2.Provider)
	}
}

func TestApplyFlagsMCPFlagsIgnoredAndUnknownFlagSafe(t *testing.T) {
	c := baseFileConfig()
	c.MCPServers = []mcp.Config{{Name: "file-srv", Command: []string{"file-bin"}}}
	fs, explicit := flagSetForApply(t, "-mcp", "srv=cmd --flag")
	ApplyFlags(&c, fs, func(name string) bool {
		// A predicate claiming an unknown flag was set must not panic.
		if name == "not-a-flag" {
			return true
		}
		return explicit[name]
	})
	if len(c.MCPServers) != 1 || c.MCPServers[0].Name != "file-srv" {
		t.Errorf("-mcp must not flow through ApplyFlags, got %+v", c.MCPServers)
	}
}

func TestApplyFlagsNumericRoundTrip(t *testing.T) {
	c := Config{}
	fs, explicit := flagSetForApply(t,
		"-max-iter", "12", "-compact-threshold", "0.85",
		"-context-window", "128000", "-max-turns", "4", "-prewarm")
	ApplyFlags(&c, fs, func(name string) bool { return explicit[name] })
	if c.MaxIter != 12 || c.CompactThreshold != 0.85 ||
		c.ContextWindow != 128000 || c.MaxTurns != 4 || !c.Prewarm {
		t.Errorf("numeric flags lost in round trip: %+v", c)
	}
}

func TestHomePathHonorsLYONHome(t *testing.T) {
	t.Setenv("LYON_HOME", `C:\scratch\lyon-home`)
	p, err := HomePath()
	if err != nil {
		t.Fatalf("HomePath: %v", err)
	}
	if filepath.Base(p) != "config.toml" || filepath.Base(filepath.Dir(p)) != "lyon-home" {
		t.Errorf("HomePath = %q, want $LYON_HOME/config.toml", p)
	}
	// Unset LYON_HOME falls back to ~/.lyon/config.toml — the file must
	// only be named here, never created.
	t.Setenv("LYON_HOME", "")
	p, err = HomePath()
	if err != nil {
		t.Fatalf("HomePath: %v", err)
	}
	if filepath.Base(p) != "config.toml" || filepath.Base(filepath.Dir(p)) != ".lyon" {
		t.Errorf("HomePath = %q, want ~/.lyon/config.toml", p)
	}
}

func TestWorkspacePathJoinsDotfile(t *testing.T) {
	if got := WorkspacePath("some/dir"); got != filepath.Join("some", "dir", ".lyon.toml") {
		t.Errorf("WorkspacePath = %q", got)
	}
}

func TestLoadShellAllowStringForm(t *testing.T) {
	dir := t.TempDir()
	p := writeConfig(t, dir, "config.toml", `shell_allow = "echo, ls"`)
	c, err := Load(p, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(c.ShellAllowlist, []string{"echo", "ls"}) {
		t.Errorf("shell_allow = %v, want [echo ls]", c.ShellAllowlist)
	}
}
