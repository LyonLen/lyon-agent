package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lyon-agent/pkg/hooks"
	"lyon-agent/pkg/tools"
)

func modeTestSession(t *testing.T) *Session {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Workspace = t.TempDir()
	cfg.WebToolMode = "off" // keep the tool set deterministic
	eng := NewEngine(cfg, nil)
	t.Cleanup(eng.Close) // retire the hooks writer before TempDir cleanup
	return &Session{eng: eng}
}

// TestModeSwitching: get/set roundtrip, rejection of unknown names,
// and no-op on same-mode.
func TestModeSwitching(t *testing.T) {
	s := modeTestSession(t)
	if s.Mode() != ModeDefault {
		t.Fatalf("fresh mode = %s, want default", s.Mode())
	}
	if _, err := s.SetMode(ModePlan); err != nil {
		t.Fatalf("set plan: %v", err)
	}
	if s.Mode() != ModePlan {
		t.Fatalf("mode = %s, want plan", s.Mode())
	}
	if prev, err := s.SetMode(Mode("chaos")); err == nil || s.Mode() != ModePlan {
		t.Fatalf("invalid mode accepted (prev=%s): err=%v", prev, err)
	}
	if prev, err := s.SetMode(ModePlan); err != nil || prev != ModePlan {
		t.Fatalf("same-mode set: prev=%s err=%v", prev, err)
	}
	if _, err := s.SetMode(ModeBypass); err != nil || s.Mode() != ModeBypass {
		t.Fatalf("bypass: %v mode=%s", err, s.Mode())
	}
}

// TestPlanModeGate: preTool blocks mutations and unknowns in plan mode,
// passes read-only tools — and default mode blocks nothing by itself.
func TestPlanModeGate(t *testing.T) {
	s := modeTestSession(t)
	ctx := context.Background()
	in := json.RawMessage(`{}`)

	// Default: gate is transparent (policy/hooks are separate).
	for _, name := range []string{"run_shell", "write_file", "mcp_srv_tool"} {
		if err := s.preTool(ctx, name, in); err != nil {
			t.Fatalf("default mode blocked %s: %v", name, err)
		}
	}

	if _, err := s.SetMode(ModePlan); err != nil {
		t.Fatal(err)
	}
	blocked := []string{
		"run_shell", "write_file", "edit_file",
		"save_memory", "forget_memory",
		"mcp_srv_tool",     // MCP side effects unknown — fail closed
		"never-registered", // unknown tool — fail closed
	}
	for _, name := range blocked {
		err := s.preTool(ctx, name, in)
		if err == nil || !strings.Contains(err.Error(), "plan mode") {
			t.Errorf("plan mode must block %s, got %v", name, err)
		}
	}
	allowed := []string{"read_file", "view_image", "read_memory", "run_subagent", "run_subagents"}
	for _, name := range allowed {
		if err := s.preTool(ctx, name, in); err != nil {
			t.Errorf("plan mode must pass %s: %v", name, err)
		}
	}

	// Back to default: mutations flow again.
	if _, err := s.SetMode(ModeDefault); err != nil {
		t.Fatal(err)
	}
	if err := s.preTool(ctx, "run_shell", in); err != nil {
		t.Fatalf("default after plan: %v", err)
	}
}

// TestBypassApprove: bypass never prompts the human, always grants, and
// still writes both audit events; default keeps the human's verdict.
func TestBypassApprove(t *testing.T) {
	s := modeTestSession(t)
	calls := 0
	appr := func(ctx context.Context, e tools.ExecEvent) bool {
		calls++
		return false // a human who would deny
	}
	approved := s.wrapApprove(appr)
	ev := tools.ExecEvent{Name: "run_shell", Input: json.RawMessage(`{}`)}

	if approved(context.Background(), ev) {
		t.Fatal("default mode must keep the deny verdict")
	}
	if calls != 1 {
		t.Fatalf("human prompted %d times, want 1", calls)
	}

	if _, err := s.SetMode(ModeBypass); err != nil {
		t.Fatal(err)
	}
	if !approved(context.Background(), ev) {
		t.Fatal("bypass must auto-approve")
	}
	if calls != 1 {
		t.Fatalf("bypass touched the human (%d calls) — must short-circuit", calls)
	}

	// nil approver stays nil: bypass has nothing to short-circuit.
	if s.wrapApprove(nil) != nil {
		t.Fatal("nil approver must stay nil")
	}
}

// TestSystemPromptMode: plan mode appends its contract to the system
// prompt; default/bypass keep the frozen bytes (cache prefix stable).
func TestSystemPromptMode(t *testing.T) {
	s := modeTestSession(t)
	base := s.eng.cfg.SystemPrompt

	if got := s.baseParams(nil).System[0].Text; got != base {
		t.Fatalf("default system prompt mutated")
	}
	if _, err := s.SetMode(ModePlan); err != nil {
		t.Fatal(err)
	}
	got := s.baseParams(nil).System[0].Text
	if !strings.HasPrefix(got, base) || !strings.Contains(got, "[MODE: PLAN]") {
		t.Fatalf("plan system prompt wrong: %q", got[len(got)-120:])
	}
	if _, err := s.SetMode(ModeBypass); err != nil {
		t.Fatal(err)
	}
	if got := s.baseParams(nil).System[0].Text; got != base {
		t.Fatalf("bypass must not touch the system prompt")
	}
}

// TestModeAuditTrail: every switch lands a `mode` record, and plan's
// tool blocks land as security_block with blocked_by=mode:plan — the
// audit must answer "what changed" and "what stopped this call".
func TestModeAuditTrail(t *testing.T) {
	dir := tempDir(t)
	t.Setenv("LYON_HOME", dir) // engine's hooks manager reads this at New
	s := modeTestSession(t)

	if _, err := s.SetMode(ModePlan); err != nil {
		t.Fatal(err)
	}
	if err := s.preTool(context.Background(), "run_shell", json.RawMessage(`{}`)); err == nil {
		t.Fatal("plan mode let run_shell through")
	}
	if _, err := s.SetMode(ModeBypass); err != nil {
		t.Fatal(err)
	}
	s.eng.hooks.Flush()

	auditPath := filepath.Join(dir, "audit", time.Now().Format("2006-01-02")+".jsonl")
	data, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("audit file: %v", err)
	}
	var modes, blocks int
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var r hooks.Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("audit line invalid: %v", err)
		}
		switch r.Event {
		case hooks.EvMode:
			modes++
			if r.Detail != "default → plan" && r.Detail != "plan → bypass" {
				t.Errorf("mode record detail: %q", r.Detail)
			}
		case hooks.EvSecurityBlock:
			if r.BlockedBy == "mode:plan" {
				blocks++
				if r.Tool != "run_shell" {
					t.Errorf("blocked tool: %q", r.Tool)
				}
			}
		}
	}
	if modes != 2 {
		t.Errorf("mode audit records = %d, want 2", modes)
	}
	if blocks != 1 {
		t.Errorf("mode:plan security_block records = %d, want 1", blocks)
	}
}
