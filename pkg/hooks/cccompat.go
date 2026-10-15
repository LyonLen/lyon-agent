package hooks

// Claude Code settings.json hooks compatibility (ROADMAP 1.5).
//
// Parses the "hooks" section of a Claude Code settings file and registers
// its entries on a Manager, so a project migrated from Claude Code keeps
// its hooks working. Shape (tolerated siblings like env/model/permissions
// are ignored):
//
//	{"hooks": {"PreToolUse": [
//	  {"matcher": "Bash|Edit", "hooks": [
//	    {"type": "command", "command": "node C:/h.js", "timeout": 30}
//	  ]} ]}}
//
// Mapping is best-effort ("能解析多少算多少"): entries whose event maps
// onto a lyon event run through the normal runHook path (same timeout and
// WaitDelay protection as native hooks); events without a lyon equivalent
// are skipped and warned about once per load on stderr.
//
// Deliberate limits:
//   - matcher is preserved verbatim on the parsed group but not enforced
//     at fire time: Fire/CheckTool match on event only, and hooks.go stays
//     untouched by this compatibility layer. A migrated matcher currently
//     selects nothing — the entry runs for every occurrence of its mapped
//     lyon event.
//   - stdin carries lyon's Record JSON, not Claude Code's hook payload, so
//     hooks reading fields like tool_name see different input.
//   - only type=command entries run (plus the args exec form); prompt,
//     agent, http and mcp_tool entries are skipped and counted.
//   - blocking maps to PreToolUse only: registered with Blocking on
//     tool_exec_start, so CheckTool honors Claude Code's exit-2 / veto
//     convention through parseDeny. Other events run fire-and-forget.

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// ccDefaultTimeoutMS mirrors Claude Code's classic 60s command-hook
// default: absent or invalid "timeout" falls back to it. (lyon's own
// hooks default to 10s via runHook; migrated hooks keep Claude Code's
// headroom so a working setup does not start getting killed early.)
const ccDefaultTimeoutMS = 60_000

// evSubagentStart is pkg/agent's EventSubagentStart value — the agent
// fires it through emit; this package just matches on the string.
const evSubagentStart = "subagent_start"

// ccEventMap maps a Claude Code hook event name onto the lyon event that
// fires at the same lifecycle point. Absent = unmapped: skipped at load
// and reported once. Equivalences are semantic, not literal:
//
//	PreToolUse       → tool_exec_start  (pre-exec control path; exit 2 vetoes)
//	PostToolUse      → tool_exec_end    (after execution)
//	UserPromptSubmit → prompt_start     (user submitted a prompt)
//	Stop             → prompt_end       (the prompt's tool loop finished)
//	SessionStart     → session_start    (lyon defines the event; engine
//	                                     has no trigger yet — hooks wait)
//	SessionEnd       → unmapped         (lyon has no session_end event)
//	Notification     → unmapped         (no lyon equivalent)
//	PreCompact       → compaction
//	PostCompact      → unmapped         (would double-fire with PreCompact)
//	SubagentStart    → subagent_start
//	SubagentStop     → subagent_end
//	PermissionRequest→ approve_request  (observer only; cannot decide)
var ccEventMap = map[string]string{
	"PreToolUse":        EvToolExecStart,
	"PostToolUse":       EvToolExecEnd,
	"UserPromptSubmit":  EvPromptStart,
	"Stop":              EvPromptEnd,
	"SessionStart":      EvSessionStart,
	"PreCompact":        EvCompaction,
	"SubagentStart":     evSubagentStart,
	"SubagentStop":      EvSubagentEnd,
	"PermissionRequest": EvApproveReq,
}

// ccSettingsFile is the slice of a Claude Code settings file we care
// about; every other key (env, model, permissions, worktree…) is ignored.
type ccSettingsFile struct {
	Hooks map[string][]ccMatcherGroup `json:"hooks"`
}

// ccMatcherGroup is one outer hooks[] element: an optional matcher plus
// the inner hook entries it applies to.
type ccMatcherGroup struct {
	Matcher string      `json:"matcher"`
	Hooks   []ccRawHook `json:"hooks"`
}

// ccRawHook is one inner hooks[] entry as Claude Code writes it.
type ccRawHook struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args"`    // exec form (no shell)
	Timeout int      `json:"timeout"` // seconds — Claude Code's unit
}

// CCHookGroup is one parsed matcher group: the original Claude Code
// event name, the lyon event it maps to ("" = unmapped), the matcher
// preserved verbatim, and its inner hook entries.
type CCHookGroup struct {
	Event     string
	LyonEvent string
	Matcher   string
	Hooks     []CCHookEntry
}

// CCHookEntry is one inner hooks[] entry after normalization: type
// verbatim ("" tolerated — treated as command at load), command/args
// verbatim, and the timeout already converted to the milliseconds
// runHook consumes.
type CCHookEntry struct {
	Type      string
	Command   string
	Args      []string
	TimeoutMS int
}

// ParseClaudeSettingsHooks parses the hooks section of a Claude Code
// settings file (the whole file — sibling keys are ignored). Returns the
// groups sorted by event name for deterministic consumers; malformed JSON
// or a mistyped hooks value returns an error. It never panics.
func ParseClaudeSettingsHooks(data []byte) ([]CCHookGroup, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, nil // a blank settings file is an empty config, not a crime
	}
	var file ccSettingsFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("claude settings: %w", err)
	}
	if len(file.Hooks) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(file.Hooks))
	for name := range file.Hooks {
		if name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	groups := make([]CCHookGroup, 0, len(names))
	for _, name := range names {
		for _, raw := range file.Hooks[name] {
			g := CCHookGroup{
				Event:     name,
				LyonEvent: ccEventMap[name], // "" when unmapped
				Matcher:   raw.Matcher,      // preserved verbatim
			}
			for _, h := range raw.Hooks {
				g.Hooks = append(g.Hooks, CCHookEntry{
					Type:      h.Type,
					Command:   h.Command,
					Args:      h.Args,
					TimeoutMS: ccTimeoutMS(h.Timeout),
				})
			}
			groups = append(groups, g)
		}
	}
	return groups, nil
}

// ccTimeoutMS converts Claude Code's seconds to runHook's milliseconds,
// falling back to ccDefaultTimeoutMS when absent or non-positive.
func ccTimeoutMS(sec int) int {
	if sec <= 0 {
		return ccDefaultTimeoutMS
	}
	return sec * 1000
}

// LoadClaudeSettings reads a Claude Code settings file's hooks section
// and registers every mappable command entry on m through the normal
// hook list, so Fire (async) and CheckTool (PreToolUse blocking) pick it
// up exactly like a native hooks.json entry. Returns how many entries
// were registered and how many were skipped (unmapped event, unsupported
// type, empty command). A missing file is not an error; malformed JSON
// is — nothing is registered and nothing panics. Unmapped events are
// reported on stderr once per load. LYON_HOOKS=off loads nothing.
func LoadClaudeSettings(m *Manager, path string) (loaded, skipped int, err error) {
	if m == nil {
		return 0, 0, fmt.Errorf("claude settings: nil hooks manager")
	}
	m.mu.Lock()
	on := m.hooksOn
	m.mu.Unlock()
	if !on {
		return 0, 0, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("claude settings: %w", err)
	}
	groups, err := ParseClaudeSettingsHooks(data)
	if err != nil {
		return 0, 0, err
	}

	var add []HookCfg
	unmapped := map[string]int{} // CC event name → entries skipped
	for _, g := range groups {
		if g.LyonEvent == "" {
			// No lyon equivalent: count every entry, warn once below.
			unmapped[g.Event] += len(g.Hooks)
			skipped += len(g.Hooks)
			continue
		}
		for _, h := range g.Hooks {
			typ := h.Type
			if typ == "" {
				typ = "command" // Claude Code requires type; tolerate absence
			}
			if typ != "command" || (h.Command == "" && len(h.Args) == 0) {
				skipped++
				continue
			}
			cfg := HookCfg{
				Event:     g.LyonEvent,
				TimeoutMS: h.TimeoutMS,
				// Claude Code's exit-2 veto maps onto the blocking control
				// path: CheckTool runs it pre-exec and parseDeny honors it.
				Blocking: g.Event == "PreToolUse",
			}
			if h.Command != "" {
				cfg.Command = h.Command
			} else {
				cfg.Argv = h.Args
			}
			add = append(add, cfg)
			loaded++
		}
	}

	if len(unmapped) > 0 {
		parts := make([]string, 0, len(unmapped))
		for ev, n := range unmapped {
			parts = append(parts, fmt.Sprintf("%s(%d)", ev, n))
		}
		sort.Strings(parts)
		fmt.Fprintf(os.Stderr, "hooks: claude settings: unmapped Claude Code event(s) skipped: %s\n",
			strings.Join(parts, ", "))
	}
	if len(add) > 0 {
		m.mu.Lock()
		m.hooks = append(m.hooks, add...)
		m.mu.Unlock()
	}
	return loaded, skipped, nil
}
