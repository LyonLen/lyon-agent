package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"lyon-agent/pkg/hooks"
	"lyon-agent/pkg/tools"
)

// Mode is a session's execution mode. It gates tools (plan) and the
// approval flow (bypass) — never the local security policy or blocking
// hooks, which stay enforced in every mode (fail closed by design).
type Mode string

const (
	ModeDefault Mode = "default" // configured behavior: approval as wired
	ModePlan    Mode = "plan"    // read-only exploration: mutations blocked
	ModeBypass  Mode = "bypass"  // approvals auto-approved, audit still logs
	// ModeMindmap arms the map: it is the ONLY mode where the distiller
	// draws (Image #71 — default/plan/bypass sessions never grow a map),
	// and it arms the AUTO-ask channel (Image #61): a merged map edit
	// dispatches a chat round — in every other mode those edits land as
	// a local notice only. Execution/approvals behave exactly like
	// default; the ask channel itself is web-enforced (ChatView's
	// flushMmEdit), since only the web card can edit the map. Manual
	// selection asks stay available in all modes (user-typed intent).
	ModeMindmap Mode = "mindmap"
)

// planSuffix is appended to the system prompt while plan mode is on, so
// the model knows up front why execution tools answer "blocked" instead
// of discovering it mid-turn. One extra cache miss per switch; default
// mode restores the original bytes and the old prefix can hit again.
const planSuffix = `

[MODE: PLAN] Plan mode is active — explore and plan only. Execution tools
(run_shell, write/edit files, memory writes, MCP tools) are disabled and
will return "blocked by plan mode". Research with read-only tools, then
present a concrete step-by-step plan for the user to approve. Do not
attempt any mutation; execution resumes when mode returns to default.`

// modeAtomic stores *Mode; a nil load means default (zero-value Sessions
// and tests never initialize it).
func (s *Session) Mode() Mode {
	if p := s.mode.Load(); p != nil {
		return *p
	}
	return ModeDefault
}

// SetMode switches the session mode and writes a `mode` audit record
// (a bypass grant must always leave a trace). Returns the previous mode;
// unknown names are rejected without changing anything.
func (s *Session) SetMode(m Mode) (Mode, error) {
	switch m {
	case ModeDefault, ModePlan, ModeBypass, ModeMindmap:
	default:
		return s.Mode(), fmt.Errorf("unknown mode %q (want default|plan|bypass|mindmap)", m)
	}
	prev := s.Mode()
	if prev == m {
		return prev, nil
	}
	s.mode.Store(&m)
	// Session state is disk state: a restart must not resurrect a session
	// in a mode the user already left (or, worse, forget a bypass).
	s.notifyDirty()
	s.eng.hooks.Fire(hooks.Record{
		Event:  hooks.EvMode,
		Detail: fmt.Sprintf("%s → %s", prev, m),
	})
	return prev, nil
}

// ModeBlockDecision is the human's answer to a plan-mode interception —
// the chooser card's vocabulary mapped onto what the gate does next.
type ModeBlockDecision int

const (
	ModeBlockDeny       ModeBlockDecision = iota // keep the hard block
	ModeBlockAllowPath                           // vouch this file for the session
	ModeBlockDefault                             // SetMode(default), the call proceeds
	ModeBlockBypass                              // SetMode(bypass), the call proceeds
)

// ModeBlockFunc parks a plan-gated call for a human decision. It runs
// exactly where the hard block would have fired, on the tool's goroutine
// (the ApproveFunc pattern: ctx cancellation denies). An error, like an
// unknown decision, reproduces the block. A nil ModeBlock — TUI, tests —
// means no chooser is wired: block outright, as before.
type ModeBlockFunc func(ctx context.Context, e tools.ExecEvent) (ModeBlockDecision, error)

// modeGate runs before policy/hooks in preTool: plan mode gates
// non-read-only tools — hard-blocked when no chooser is wired (TUI,
// tests), parked for a human decision when ModeBlock is (lyond). Either
// way the model's contract is identical: a denial arrives as the tool
// result "blocked by plan mode", and an approval executes immediately
// (no retry lottery). The security_block audit fires only when a block
// actually lands — a vouch or mode switch leaves its own trace (EvMode;
// the path vouch is session-scoped memory, matching approvals.json).
func (s *Session) modeGate(ctx context.Context, e tools.ExecEvent) error {
	if s.Mode() != ModePlan {
		return nil
	}
	if planReadable(s.eng.reg, e.Name) {
		return nil
	}
	if s.modePathAllowed(e.Input) {
		return nil
	}
	planBlock := func() error {
		s.eng.hooks.Fire(hooks.Record{
			Event:     hooks.EvSecurityBlock,
			Tool:      e.Name,
			Input:     string(e.Input),
			BlockedBy: "mode:plan",
			Err:       "blocked by plan mode (read-only exploration)",
			IsError:   true,
		})
		return fmt.Errorf("blocked by plan mode (read-only exploration) — switch with /mode")
	}
	if s.eng.cfg.ModeBlock == nil {
		return planBlock() // no chooser wired: the old hard block, verbatim
	}
	dec, err := s.eng.cfg.ModeBlock(ctx, e)
	if err != nil {
		return planBlock() // chooser machinery failed — fail closed as the block
	}
	switch dec {
	case ModeBlockAllowPath:
		key := modePathKey(s.eng.cfg.Workspace, e.Input)
		if key == "" {
			return planBlock() // pathless input has nothing to vouch — fail closed
		}
		s.grantModePath(key)
		return nil
	case ModeBlockDefault:
		// SetMode validates the name; a failure here would be a programmer
		// error, and failing closed (block) is the safe arm.
		if _, err := s.SetMode(ModeDefault); err != nil {
			return planBlock()
		}
		return nil
	case ModeBlockBypass:
		if _, err := s.SetMode(ModeBypass); err != nil {
			return planBlock()
		}
		return nil
	default: // ModeBlockDeny and anything unknown
		return planBlock()
	}
}

// grantModePath vouches one normalized key for this session (the
// interception card's "仅该文件").
func (s *Session) grantModePath(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.modePaths == nil {
		s.modePaths = make(map[string]bool)
	}
	s.modePaths[key] = true
}

// modePathAllowed reports whether the input's path carries a session
// vouch. Pathless inputs (run_shell, save_memory…) never do — those
// escape only through a mode switch, never through a file card.
func (s *Session) modePathAllowed(input json.RawMessage) bool {
	key := modePathKey(s.eng.cfg.Workspace, input)
	if key == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.modePaths[key]
}

// modePathKey normalizes a tool input's path to the vouch key: relative
// paths join the workspace and keys are slash-separated, so "docs/a.md"
// and "<ws>\docs\v" vouch the same file. A path outside the workspace
// keeps its absolute form — cross-root writes vouch individually.
func modePathKey(ws string, input json.RawMessage) string {
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(input, &in); err != nil || in.Path == "" {
		return ""
	}
	p := in.Path
	if !filepath.IsAbs(p) {
		p = filepath.Join(ws, p)
	}
	p = filepath.Clean(p)
	if rel, err := filepath.Rel(ws, p); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(p)
}

// planReadable decides what plan mode lets through. ParallelSafe is the
// registry's built-in "no side effects" annotation (read_file, web_*,
// view_image, read_memory — and it already excludes shell/write/edit/
// memory mutations). Two overrides: MCP tools declare safe but their
// real side effects are unknown (fail closed), and both subagent tools
// (run_subagent and the run_subagents fan-out) are allowed because they
// hand their nested loops only read-only tools that pass this very same
// gate — blocking the fan-out in plan mode would contradict that and
// silently disable the concurrent path (#53, 并发倾向).
func planReadable(reg *tools.Registry, name string) bool {
	if name == "run_subagent" || name == "run_subagents" {
		return true
	}
	if strings.HasPrefix(name, "mcp_") {
		return false
	}
	t, ok := reg.Get(name)
	if !ok {
		return false
	}
	return t.ParallelSafe()
}

// systemWithMode renders the system prompt for the current mode (plan
// injects its contract; default/bypass keep the frozen bytes).
func (s *Session) systemWithMode(base string) string {
	if s.Mode() == ModePlan {
		return base + planSuffix
	}
	return base
}
