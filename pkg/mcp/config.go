package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"lyon-agent/pkg/tools"
)

// This file is the ~/.lyon/mcp.json entry point (task E1): startup loads
// persisted servers without any -mcp flags, flags override same-name file
// entries, and every failure degrades to a warning — one broken server must
// never block the whole process. The on-disk shape follows the Claude
// ecosystem's .mcp.json so users can reuse their existing files:
//
//	{"mcpServers": {
//	  "stdio-one": {"command": ["npx", "-y", "x"], "args": ["--flag"], "env": {"K": "V"}},
//	  "string-cmd": {"command": "npx", "args": ["-y", "y"]},
//	  "http-one": {"url": "http://host/mcp"}
//	}}
type fileConfig struct {
	McpServers map[string]fileServer `json:"mcpServers"`
}

// fileServer is one mcp.json entry. Command accepts either an argv array or
// a bare string (Claude Code's .mcp.json uses the string form with a
// separate args array); Args is appended to Command either way. Env is the
// Claude-style key/value map, converted to "K=V" pairs for Config.Env.
type fileServer struct {
	Command argvArray         `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	URL     string            `json:"url"`
}

// argvArray unmarshals the two command shapes seen in the wild: a JSON
// string ("npx") or an argv array (["npx", "-y", "x"]).
type argvArray []string

func (a *argvArray) UnmarshalJSON(data []byte) error {
	var one string
	if err := json.Unmarshal(data, &one); err == nil {
		*a = argvArray{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return fmt.Errorf("command must be a string or an array of strings")
	}
	*a = argvArray(many)
	return nil
}

// ConfigPath is the default config location: $LYON_HOME/mcp.json when
// LYON_HOME is set (containers, CI, tests), else ~/.lyon/mcp.json — the
// same store-root convention as the session/leaderboard files.
func ConfigPath() (string, error) {
	if h := os.Getenv("LYON_HOME"); h != "" {
		return filepath.Join(h, "mcp.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".lyon", "mcp.json"), nil
}

// LoadServers parses the mcp.json at path into Connect-ready configs, in
// sorted name order (map iteration is random; the registry's tool names —
// and therefore the prompt cache prefix — must be deterministic). A missing
// file is not an error (flag-only setups keep working): it yields no
// servers and no problems. A malformed file yields one problem. Individually
// bad entries (empty name, command+url together, neither) are skipped and
// reported per-entry through problems — one bad server never blocks the
// rest.
func LoadServers(path string) ([]Config, []error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []error{fmt.Errorf("mcp.json: read %s: %w", path, err)}
	}
	var fc fileConfig
	if err := json.Unmarshal(data, &fc); err != nil {
		return nil, []error{fmt.Errorf("mcp.json: parse %s: %w", path, err)}
	}
	names := make([]string, 0, len(fc.McpServers))
	for name := range fc.McpServers {
		names = append(names, name)
	}
	sort.Strings(names)

	var servers []Config
	var problems []error
	for _, name := range names {
		entry := fc.McpServers[name]
		cmd := append(append([]string(nil), entry.Command...), entry.Args...)
		switch {
		case strings.TrimSpace(name) == "":
			problems = append(problems, fmt.Errorf("mcp.json: %s: server name must not be empty", path))
		case entry.URL != "" && len(cmd) > 0:
			problems = append(problems, fmt.Errorf("mcp.json: server %q: command and url are mutually exclusive", name))
		case entry.URL == "" && len(cmd) == 0:
			problems = append(problems, fmt.Errorf("mcp.json: server %q: needs a command or a url", name))
		default:
			servers = append(servers, Config{
				Name:    strings.TrimSpace(name),
				Command: cmd,
				URL:     entry.URL,
				Env:     envPairs(entry.Env),
			})
		}
	}
	return servers, problems
}

// MergeServers layers flag servers over file servers: a flag entry with the
// same name replaces the file entry, flag-only entries append after the
// file survivors. Neither input slice is mutated.
func MergeServers(file, flags []Config) []Config {
	overridden := make(map[string]bool, len(flags))
	for _, s := range flags {
		overridden[strings.TrimSpace(s.Name)] = true
	}
	out := make([]Config, 0, len(file)+len(flags))
	for _, s := range file {
		if !overridden[strings.TrimSpace(s.Name)] {
			out = append(out, s)
		}
	}
	return append(out, flags...)
}

// ConnectFileServers dials the mcp.json servers that flags do not override
// and registers their tools into reg. Tolerant by design: a missing config
// file is silent, and every other failure (bad entry, dead server,
// unlistable tools) goes to warn and is skipped, so one broken server can
// never block startup. Surviving connections land on mgr; the caller owns
// its lifetime. Flag servers are NOT connected here — the caller keeps
// their existing fatal contract.
func ConnectFileServers(ctx context.Context, mgr *Manager, reg *tools.Registry, flags []Config, warn func(error)) {
	path, err := ConfigPath()
	if err != nil {
		warn(fmt.Errorf("mcp.json: locate config: %w", err))
		return
	}
	servers, problems := LoadServers(path)
	for _, p := range problems {
		warn(p)
	}
	// MergeServers appends the flag entries last; only the head — the file
	// survivors after flag overrides — connects here. Flags go through the
	// caller's own loop, which keeps their fatal contract.
	merged := MergeServers(servers, flags)
	for _, s := range merged[:len(merged)-len(flags)] {
		connectFileServer(ctx, mgr, reg, s, warn)
	}
}

// connectFileServer dials one file-loaded server and registers its tools;
// every failure is a warn, never an error return.
func connectFileServer(ctx context.Context, mgr *Manager, reg *tools.Registry, s Config, warn func(error)) {
	conn, err := mgr.Connect(ctx, s)
	if err != nil {
		warn(err)
		return
	}
	ts, err := conn.Tools(ctx)
	if err != nil {
		warn(err)
		return
	}
	for _, t := range ts {
		if err := reg.Register(t); err != nil {
			warn(err)
		}
	}
}

// envPairs converts the Claude-style env map into sorted "K=V" pairs
// (sorted for deterministic process environments across runs).
func envPairs(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(m))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out
}
