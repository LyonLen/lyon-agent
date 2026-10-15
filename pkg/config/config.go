// Package config loads lyon.toml — ROADMAP 0.2's single configuration file
// that absorbs the CLI flags. Two file layers merge, the workspace one
// winning:
//
//	$LYON_HOME/config.toml (else ~/.lyon/config.toml)   home defaults
//	<workspace>/.lyon.toml                             project overrides
//
// On top of both, explicitly-set CLI flags win (ApplyFlags, driven by
// flag.FlagSet.Visit — an unset flag is indistinguishable from its default,
// so only Visit can tell "typed" from "defaulted"). Full precedence:
//
//	flag > <workspace>/.lyon.toml > home config.toml > built-in default
//
// With neither file present Load returns the zero Config and a nil error,
// so a machine without config files behaves exactly as before this package
// existed. Nothing in a config file is ever fatal: unknown keys warn once
// each, a broken file reports an error the caller may surface, and bad
// mcpServers entries warn and skip — the same never-block-startup contract
// ~/.lyon/mcp.json has.
//
// # Schema
//
//	model             string    model id (flag -model)
//	provider          string    "anthropic" | "openai" (flag -openai)
//	effort            string    low|medium|high|xhigh|max (flag -effort)
//	approval          string    "ask" | "never" | "auto" (flag -approve)
//	workspace         string    workspace root (flag -workspace)
//	system            string    path to a frozen system-prompt file (flag -system)
//	max_iter          integer   API-call budget per prompt (flag -max-iter)
//	shell_allow       [string]  run_shell allowlist (flag -shell-allow; a
//	                            single comma string is also accepted)
//	web_tools         string    server|client|off|auto (flags -web-tools / -no-web-tools)
//	subagent_model    string    model for run_subagent (flag -subagent-model)
//	compact_threshold float     compaction trigger fraction (flag -compact-threshold)
//	context_window    integer   context size for the compaction ratio (flag -context-window)
//	max_turns         integer   in-flight API-turn cap (flag -max-turns)
//	prewarm           boolean   warm the prompt cache (flag -prewarm)
//	openai_base       string    OpenAI-compatible base URL (flag -openai-base)
//	mcpServers        table     Claude-.mcp.json-shaped servers (below)
//
// Key matching treats '-' and '_' as equivalent (max_iter = max-iter), so
// flags can be transcribed into the file as-is. Env vars (ANTHROPIC_MODEL,
// LYON_MAX_ITER, …) sit below the file layers: they still apply when the
// file leaves a key unset, and lose when the file sets it — same as they
// always lost to an explicit flag.
//
// # mcpServers
//
// The table mirrors pkg/mcp's mcp.json schema exactly — a .mcp.json body
// transcribes by wrapping each entry in [mcpServers.<name>]:
//
//	command = "npx"            or command = ["npx", "-y", "x"]
//	args    = ["-y", "x"]      appended to command either way
//	url     = "http://host/mcp"  streamable HTTP; mutually exclusive with command
//	[mcpServers.<name>.env]    environment as string key/value pairs
//
// Parsing and validation deliberately match mcp.LoadServers (see the
// parity test): sorted server names, sorted env "K=V" pairs, the same
// command/xor/url rules. Precedence in cmd/lyon's buildEngine:
// -mcp flags > these mcpServers > ~/.lyon/mcp.json, same-name override.
//
// # TOML subset
//
// Hand-rolled, because go.mod stays dependency-free. Supported: comments
// (# to end of line), bare or quoted keys, [section] and [a.b] headers,
// basic "…" strings (\\ \" \n \r \t \uXXXX escapes), literal '…' strings,
// true/false, integers and floats, multi-line scalar arrays (trailing
// comma ok), CRLF line endings. NOT supported — a parse error, with a line
// number: inline tables {}, dotted-key assignments (a.b = 1), dates,
// multi-line """…""" strings, integer underscores, hex/octal. Unknown keys
// and unknown [sections] are skipped with one warning each; a value of the
// wrong type warns and leaves the key unset.
package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"lyon-agent/pkg/mcp"
)

// Approval modes accepted by the `approval` key and ApprovalMode. "auto"
// is today's behavior — chat asks a human on a TTY, run does not — and is
// what an absent key means too.
const (
	ApprovalAsk   = "ask"   // always prompt y/n (like -approve=true)
	ApprovalNever = "never" // never prompt (like -approve=false)
	ApprovalAuto  = "auto"  // chat: ask on a TTY; run: don't ask
)

// Config is the merged lyon.toml configuration. Zero value = no config:
// every consumer falls back to its built-in default.
type Config struct {
	Model            string
	Provider         string
	Effort           string
	ApprovalMode     string // ask|never|auto|"" (unset)
	Workspace        string
	System           string // path to a system-prompt file, like -system
	MaxIter          int
	ShellAllowlist   []string
	WebTools         string // server|client|off|"" (auto)
	SubagentModel    string
	CompactThreshold float64
	ContextWindow    int64
	MaxTurns         int
	Prewarm          bool
	OpenAIBase       string
	MCPServers       []mcp.Config // name-sorted, mcp.json-shaped
	Warnings         []string     // non-fatal problems, ready for stderr
	Sources          []string     // the files that actually loaded, in order
}

// ApprovalExplicit reports whether approval came from a config decision
// (ask/never) rather than the interactive default. cmd/lyon uses it to
// tell "the user configured this" apart from the TTY-based fallback.
func (c Config) ApprovalExplicit() bool {
	return c.ApprovalMode == ApprovalAsk || c.ApprovalMode == ApprovalNever
}

// HomePath is the home-layer config location: $LYON_HOME/config.toml when
// LYON_HOME is set (containers, CI, tests), else ~/.lyon/config.toml —
// the same store-root convention as mcp.json and the session files.
func HomePath() (string, error) {
	if h := os.Getenv("LYON_HOME"); h != "" {
		return filepath.Join(h, "config.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".lyon", "config.toml"), nil
}

// WorkspacePath is the workspace-layer config file: <dir>/.lyon.toml.
func WorkspacePath(dir string) string {
	return filepath.Join(dir, ".lyon.toml")
}

// Load reads the home and workspace config files and merges them, the
// workspace layer overriding the home layer key by key (mcpServers merge
// per name). Either path may be "" or missing — that layer is simply
// absent. The returned error reports read/parse problems per file (the
// Config still carries whatever layers did load); warnings for unknown
// keys and bad entries ride on Config.Warnings.
func Load(homePath, workspacePath string) (Config, error) {
	var problems []error
	loadOne := func(path string) *file {
		if path == "" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				problems = append(problems, fmt.Errorf("%s: %w", path, err))
			}
			return nil
		}
		tree, err := parseTOML(string(data))
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", path, err))
			return nil
		}
		f := decodeFile(path, tree)
		f.sources = []string{path}
		return &f
	}
	home := loadOne(homePath)
	ws := loadOne(workspacePath)

	var out file
	switch {
	case home == nil && ws == nil:
	case ws == nil:
		out = *home
	case home == nil:
		out = *ws
	default:
		out = mergeLayers(home, ws)
	}
	cfg := out.cfg
	cfg.Warnings = out.warns
	cfg.Sources = out.sources
	sort.Slice(cfg.MCPServers, func(i, j int) bool {
		return cfg.MCPServers[i].Name < cfg.MCPServers[j].Name
	})
	return cfg, errors.Join(problems...)
}

// file is one decoded config layer: the values plus which keys the file
// actually set (presence — an explicit empty string or false must still
// override the layer below).
type file struct {
	cfg     Config
	present map[string]bool
	warns   []string
	sources []string
}

// topKeys is the accepted schema surface. Keys are matched after
// normalizeKey, so these underscore spellings also accept dashes.
var topKeys = map[string]bool{
	"model": true, "provider": true, "effort": true, "approval": true,
	"workspace": true, "system": true, "max_iter": true, "shell_allow": true,
	"web_tools": true, "subagent_model": true, "compact_threshold": true,
	"context_window": true, "max_turns": true, "prewarm": true,
	"openai_base": true, "mcpServers": true,
}

// normalizeKey folds '-' into '_' so max-iter and max_iter name the same
// key. mcpServers keeps its spelling (the Claude-ecosystem name).
func normalizeKey(k string) string { return strings.ReplaceAll(k, "-", "_") }

// decodeFile maps a parsed TOML tree onto a Config layer. Type mismatches
// and unknown keys become warnings, never errors — a warning-shaped mistake
// degrades one key, it doesn't kill the file.
func decodeFile(path string, tree map[string]any) file {
	f := file{present: map[string]bool{}}
	warnf := func(format string, args ...any) {
		f.warns = append(f.warns, path+": "+fmt.Sprintf(format, args...))
	}
	want := func(k, w string) { warnf("%s: want %s — ignored", k, w) }

	keys := make([]string, 0, len(tree))
	for k := range tree {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic warning order

	for _, k := range keys {
		v := tree[k]
		canon := normalizeKey(k)
		switch canon {
		case "model":
			if s, ok := asString(v); ok {
				f.cfg.Model, f.present[canon] = s, true
			} else {
				want(k, "a string")
			}
		case "provider":
			if s, ok := asString(v); ok {
				if s == "anthropic" || s == "openai" {
					f.cfg.Provider, f.present[canon] = s, true
				} else {
					warnf("%s: want %q or %q (got %q) — ignored", k, "anthropic", "openai", s)
				}
			} else {
				want(k, "a string")
			}
		case "effort":
			if s, ok := asString(v); ok {
				switch s {
				case "low", "medium", "high", "xhigh", "max":
					f.cfg.Effort, f.present[canon] = s, true
				default:
					warnf("%s: want low|medium|high|xhigh|max (got %q) — ignored", k, s)
				}
			} else {
				want(k, "a string")
			}
		case "approval":
			if s, ok := asString(v); ok {
				switch s {
				case ApprovalAsk, ApprovalNever, ApprovalAuto:
					f.cfg.ApprovalMode, f.present[canon] = s, true
				default:
					warnf("%s: want %q|%q|%q (got %q) — ignored", k, ApprovalAsk, ApprovalNever, ApprovalAuto, s)
				}
			} else {
				want(k, "a string")
			}
		case "workspace":
			if s, ok := asString(v); ok {
				f.cfg.Workspace, f.present[canon] = s, true
			} else {
				want(k, "a string")
			}
		case "system":
			if s, ok := asString(v); ok {
				f.cfg.System, f.present[canon] = s, true
			} else {
				want(k, "a string")
			}
		case "max_iter":
			if n, ok := asInt(v); ok {
				f.cfg.MaxIter, f.present[canon] = n, true
			} else {
				want(k, "an integer")
			}
		case "shell_allow":
			if s, ok := v.(string); ok {
				// A bare string means the comma form of -shell-allow.
				f.cfg.ShellAllowlist, f.present[canon] = splitCSV(s), true
			} else if arr, ok := asStringArray(v); ok {
				f.cfg.ShellAllowlist, f.present[canon] = cleanList(arr), true
			} else {
				want(k, "an array of strings (or a comma string)")
			}
		case "web_tools":
			if s, ok := asString(v); ok {
				switch s {
				case "server", "client", "off":
					f.cfg.WebTools, f.present[canon] = s, true
				case "auto":
					// The flag surface spells the default as ""; keep the
					// presence so an explicit "auto" still overrides home.
					f.cfg.WebTools, f.present[canon] = "", true
				default:
					warnf("%s: want server|client|off|auto (got %q) — ignored", k, s)
				}
			} else {
				want(k, "a string")
			}
		case "subagent_model":
			if s, ok := asString(v); ok {
				f.cfg.SubagentModel, f.present[canon] = s, true
			} else {
				want(k, "a string")
			}
		case "compact_threshold":
			if n, ok := asFloat(v); ok {
				f.cfg.CompactThreshold, f.present[canon] = n, true
			} else {
				want(k, "a number")
			}
		case "context_window":
			if n, ok := asInt(v); ok {
				f.cfg.ContextWindow, f.present[canon] = int64(n), true
			} else {
				want(k, "an integer")
			}
		case "max_turns":
			if n, ok := asInt(v); ok {
				f.cfg.MaxTurns, f.present[canon] = n, true
			} else {
				want(k, "an integer")
			}
		case "prewarm":
			if b, ok := v.(bool); ok {
				f.cfg.Prewarm, f.present[canon] = b, true
			} else {
				want(k, "a boolean")
			}
		case "openai_base":
			if s, ok := asString(v); ok {
				f.cfg.OpenAIBase, f.present[canon] = s, true
			} else {
				want(k, "a string")
			}
		case "mcpServers":
			tbl, ok := v.(map[string]any)
			if !ok {
				want(k, "tables of servers: [mcpServers.<name>]")
				continue
			}
			servers, warns := decodeServers(tbl)
			for _, w := range warns {
				warnf("%s", w)
			}
			f.cfg.MCPServers = servers
			f.present[canon] = true
		default:
			if _, isTbl := v.(map[string]any); isTbl {
				warnf("unknown section [%s] (ignored)", k)
			} else {
				warnf("unknown key %q (ignored)", k)
			}
		}
	}
	return f
}

// decodeServers turns the [mcpServers] table into Connect-ready configs,
// sorted by name with sorted env "K=V" pairs — the exact shape
// mcp.LoadServers produces from a mcp.json (see the parity test). Invalid
// entries are skipped with a warning each, one bad server never blocking
// the rest.
func decodeServers(tbl map[string]any) ([]mcp.Config, []string) {
	var warns []string
	warnf := func(format string, args ...any) {
		warns = append(warns, fmt.Sprintf("mcpServers: "+format, args...))
	}
	names := make([]string, 0, len(tbl))
	for n := range tbl {
		names = append(names, n)
	}
	sort.Strings(names)

	var out []mcp.Config
	for _, name := range names {
		entry, ok := tbl[name].(map[string]any)
		if !ok {
			warnf("server %q: want a table ([mcpServers.%s]) — skipped", name, name)
			continue
		}
		entryKeys := make([]string, 0, len(entry))
		for k := range entry {
			entryKeys = append(entryKeys, k)
		}
		sort.Strings(entryKeys)

		var cmd, args, env []string
		var url string
		bad := false
		for _, ek := range entryKeys {
			ev := entry[ek]
			switch normalizeKey(ek) {
			case "command":
				if arr, ok := asStringArray(ev); ok {
					cmd = arr
				} else {
					warnf("server %q: command must be a string or an array of strings — skipped", name)
					bad = true
				}
			case "args":
				if arr, ok := asStringArray(ev); ok {
					args = arr
				} else {
					warnf("server %q: args must be an array of strings — skipped", name)
					bad = true
				}
			case "env":
				if pairs, ok := asEnvPairs(ev); ok {
					env = pairs
				} else {
					warnf("server %q: env must be a table of string values ([mcpServers.%s.env]) — skipped", name, name)
					bad = true
				}
			case "url":
				if s, ok := asString(ev); ok {
					url = s
				} else {
					warnf("server %q: url must be a string — skipped", name)
					bad = true
				}
			default:
				warnf("server %q: unknown key %q (ignored)", name, ek)
			}
		}
		if bad {
			continue
		}
		var full []string // nil when empty, like mcp.LoadServers' argv
		if len(cmd)+len(args) > 0 {
			full = make([]string, 0, len(cmd)+len(args))
			full = append(full, cmd...)
			full = append(full, args...)
		}
		switch {
		case strings.TrimSpace(name) == "":
			warnf("server name must not be empty — skipped")
		case url != "" && len(full) > 0:
			warnf("server %q: command and url are mutually exclusive — skipped", name)
		case url == "" && len(full) == 0:
			warnf("server %q: needs a command or a url — skipped", name)
		default:
			out = append(out, mcp.Config{
				Name:    strings.TrimSpace(name),
				Command: full,
				URL:     url,
				Env:     env,
			})
		}
	}
	return out, warns
}

// mergeLayers overlays the workspace layer (high) on the home layer (low).
// A key the high layer set wins even when its value is empty/false; keys it
// didn't touch keep the low value; mcpServers merge per name (high wins,
// high-only appends). Neither input is mutated.
func mergeLayers(low, high *file) file {
	out := file{
		cfg:     low.cfg,
		present: make(map[string]bool, len(low.present)+len(high.present)),
		warns:   append(append([]string{}, low.warns...), high.warns...),
		sources: append(append([]string{}, low.sources...), high.sources...),
	}
	for k := range low.present {
		out.present[k] = true
	}
	for k := range high.present {
		out.present[k] = true
	}
	if high.present["model"] {
		out.cfg.Model = high.cfg.Model
	}
	if high.present["provider"] {
		out.cfg.Provider = high.cfg.Provider
	}
	if high.present["effort"] {
		out.cfg.Effort = high.cfg.Effort
	}
	if high.present["approval"] {
		out.cfg.ApprovalMode = high.cfg.ApprovalMode
	}
	if high.present["workspace"] {
		out.cfg.Workspace = high.cfg.Workspace
	}
	if high.present["system"] {
		out.cfg.System = high.cfg.System
	}
	if high.present["max_iter"] {
		out.cfg.MaxIter = high.cfg.MaxIter
	}
	if high.present["shell_allow"] {
		out.cfg.ShellAllowlist = high.cfg.ShellAllowlist
	}
	if high.present["web_tools"] {
		out.cfg.WebTools = high.cfg.WebTools
	}
	if high.present["subagent_model"] {
		out.cfg.SubagentModel = high.cfg.SubagentModel
	}
	if high.present["compact_threshold"] {
		out.cfg.CompactThreshold = high.cfg.CompactThreshold
	}
	if high.present["context_window"] {
		out.cfg.ContextWindow = high.cfg.ContextWindow
	}
	if high.present["max_turns"] {
		out.cfg.MaxTurns = high.cfg.MaxTurns
	}
	if high.present["prewarm"] {
		out.cfg.Prewarm = high.cfg.Prewarm
	}
	if high.present["openai_base"] {
		out.cfg.OpenAIBase = high.cfg.OpenAIBase
	}
	out.cfg.MCPServers = mcp.MergeServers(low.cfg.MCPServers, high.cfg.MCPServers)
	return out
}

// ApplyFlags overrides c with every flag the user explicitly set on fs —
// and only those. explicitlySet is the caller's Visit-derived predicate;
// flag defaults are indistinguishable from values, so "was it typed" can
// only come from flag.FlagSet.Visit. Flag defaults themselves are never
// read: an unset flag leaves the file value (or the zero value) in place,
// and cmd/lyon's buildConfig keeps interpreting zero the same way it
// always has.
//
// The -mcp / -mcp-http flags are deliberately absent: they are repeatable
// list flags whose layering (flag > toml > mcp.json, same-name override)
// happens in cmd/lyon's buildEngine through mcp.MergeServers, not here.
func ApplyFlags(c *Config, fs *flag.FlagSet, explicitlySet func(name string) bool) {
	if c == nil || fs == nil {
		return
	}
	get := func(name string) (string, bool) {
		if !explicitlySet(name) {
			return "", false
		}
		fl := fs.Lookup(name)
		if fl == nil {
			return "", false
		}
		return fl.Value.String(), true
	}
	warnf := func(format string, args ...any) {
		c.Warnings = append(c.Warnings, "flag: "+fmt.Sprintf(format, args...))
	}

	if v, ok := get("model"); ok {
		c.Model = v
	}
	if v, ok := get("effort"); ok {
		c.Effort = v
	}
	if v, ok := get("workspace"); ok {
		c.Workspace = v
	}
	if v, ok := get("system"); ok {
		c.System = v
	}
	if v, ok := get("subagent-model"); ok {
		c.SubagentModel = v
	}
	if v, ok := get("openai-base"); ok {
		c.OpenAIBase = v
	}
	if v, ok := get("max-iter"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			c.MaxIter = n
		} else {
			warnf("-max-iter: %v", err)
		}
	}
	if v, ok := get("max-turns"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			c.MaxTurns = n
		} else {
			warnf("-max-turns: %v", err)
		}
	}
	if v, ok := get("context-window"); ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			c.ContextWindow = n
		} else {
			warnf("-context-window: %v", err)
		}
	}
	if v, ok := get("compact-threshold"); ok {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			c.CompactThreshold = n
		} else {
			warnf("-compact-threshold: %v", err)
		}
	}
	if v, ok := get("shell-allow"); ok {
		c.ShellAllowlist = splitCSV(v)
	}
	// Same order as buildConfig resolves them: -no-web-tools first, then
	// -web-tools wins when both were typed.
	if v, ok := get("no-web-tools"); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			if b {
				c.WebTools = "off"
			}
		} else {
			warnf("-no-web-tools: %v", err)
		}
	}
	if v, ok := get("web-tools"); ok {
		c.WebTools = v
	}
	if v, ok := get("approve"); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			if b {
				c.ApprovalMode = ApprovalAsk
			} else {
				c.ApprovalMode = ApprovalNever
			}
		} else {
			warnf("-approve: %v", err)
		}
	}
	if v, ok := get("prewarm"); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Prewarm = b
		} else {
			warnf("-prewarm: %v", err)
		}
	}
	if v, ok := get("openai"); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			if b {
				c.Provider = "openai"
			} else {
				c.Provider = "anthropic"
			}
		} else {
			warnf("-openai: %v", err)
		}
	}
}

// splitCSV splits a comma-separated list, trimming parts and dropping
// empties — the same interpretation cmd/lyon has always given -shell-allow.
func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// cleanList trims elements and drops empties from an array form.
func cleanList(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func asString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case int64:
		return int(n), true
	}
	return 0, false
}

func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// asStringArray accepts a TOML array of strings, or — leniently — a bare
// string as a one-element array (mcp.json's string command form).
func asStringArray(v any) ([]string, bool) {
	if s, ok := v.(string); ok {
		return []string{s}, true
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		s, ok := e.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// asEnvPairs converts an [mcpServers.x.env] table into sorted "K=V" pairs,
// the same shape pkg/mcp builds from mcp.json (sorted for deterministic
// process environments). nil when empty, like pkg/mcp's envPairs.
func asEnvPairs(v any) ([]string, bool) {
	tbl, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	keys := make([]string, 0, len(tbl))
	for k := range tbl {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		s, ok := tbl[k].(string)
		if !ok {
			return nil, false
		}
		out = append(out, k+"="+s)
	}
	return out, true
}
