package agent

import (
	"encoding/json"
	"fmt"
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
	case ModeDefault, ModePlan, ModeBypass:
	default:
		return s.Mode(), fmt.Errorf("unknown mode %q (want default|plan|bypass)", m)
	}
	prev := s.Mode()
	if prev == m {
		return prev, nil
	}
	s.mode.Store(&m)
	s.eng.hooks.Fire(hooks.Record{
		Event:  hooks.EvMode,
		Detail: fmt.Sprintf("%s → %s", prev, m),
	})
	return prev, nil
}

// modeGate runs before policy/hooks in preTool: plan mode blocks
// non-read-only tools outright (they don't reach approval or external
// hooks — the model is told why via the tool result). Audit reuses the
// security_block record with blocked_by=mode:plan so one query answers
// "what stopped this call".
func (s *Session) modeGate(name string, input json.RawMessage) error {
	if s.Mode() != ModePlan {
		return nil
	}
	if planReadable(s.eng.reg, name) {
		return nil
	}
	s.eng.hooks.Fire(hooks.Record{
		Event:     hooks.EvSecurityBlock,
		Tool:      name,
		Input:     string(input),
		BlockedBy: "mode:plan",
		Err:       "blocked by plan mode (read-only exploration)",
		IsError:   true,
	})
	return fmt.Errorf("blocked by plan mode (read-only exploration) — switch with /mode")
}

// planReadable decides what plan mode lets through. ParallelSafe is the
// registry's built-in "no side effects" annotation (read_file, web_*,
// view_image, read_memory — and it already excludes shell/write/edit/
// memory mutations). Two overrides: MCP tools declare safe but their
// real side effects are unknown (fail closed), and run_subagent is
// allowed because it receives only read-only tools and passes this very
// same gate.
func planReadable(reg *tools.Registry, name string) bool {
	if name == "run_subagent" {
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
