package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lyon-agent/internal/mockapi"
	"lyon-agent/pkg/tools"
)

// TestSecurityPolicyBlocksTool: policy.json denying a tool vetoes execution
// inside the adapter — before approval — so the tool body never runs, the
// model receives the block as a tool result, the attempt lands in the
// audit log with blocked_by set, and the stats counter moves.
func TestSecurityPolicyBlocksTool(t *testing.T) {
	home := tempDir(t) // delete-with-retry: the writer and %TEMP% both race RemoveAll
	t.Setenv("LYON_HOME", home)
	newCompactMock(t, []mockapi.Turn{
		{Tool: &mockapi.ToolCall{Name: "noop", Input: "{}"}},
		{Text: []string{"after-block"}},
	})
	if err := os.WriteFile(filepath.Join(home, "policy.json"),
		[]byte(`{"deny_tools":["noop"]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	var ran atomic.Bool
	reg := tools.NewRegistry()
	if err := tools.RegisterFunc(reg, "noop", "does nothing", false,
		func(ctx context.Context, _ struct{}) (string, error) {
			ran.Store(true)
			return "ok", nil
		}); err != nil {
		t.Fatal(err)
	}

	eng := NewEngine(Config{Workspace: tempDir(t), CompactThreshold: 0.9}, reg)
	// Registered after home := t.TempDir(), so it runs before that
	// RemoveAll: the hooks writer must stop first or it recreates the
	// audit/stats files mid-delete ("directory is not empty").
	t.Cleanup(eng.Close)
	sess := eng.NewSession(nil)
	defer sess.Close()
	if err := sess.Submit("run the denied tool"); err != nil {
		t.Fatal(err)
	}
	pe, _ := waitPromptEndCollect(t, sess)
	if pe.Err != "" {
		t.Fatalf("prompt failed: %s", pe.Err)
	}
	if ran.Load() {
		t.Error("denied tool executed — the security gate did not hold")
	}

	if s := eng.Hooks().Stats(); s.SecurityBlocks != 1 {
		t.Errorf("stats security_blocks = %d, want 1", s.SecurityBlocks)
	}
	eng.Hooks().Flush() // audit lines land asynchronously

	auditPath := filepath.Join(home, "audit", time.Now().Format("2006-01-02")+".jsonl")
	b, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("audit file missing: %v", err)
	}
	audit := string(b)
	if !strings.Contains(audit, `"event":"security_block"`) {
		t.Errorf("security_block not audited:\n%s", audit)
	}
	if !strings.Contains(audit, `"blocked_by":"policy:`) {
		t.Errorf("blocked_by missing:\n%s", audit)
	}
	if !strings.Contains(audit, `"event":"prompt_start"`) {
		t.Errorf("prompt_start not audited:\n%s", audit)
	}
	if !strings.Contains(audit, `"event":"prompt_end"`) {
		t.Errorf("prompt_end not audited:\n%s", audit)
	}
	if !strings.Contains(audit, `"event":"tool_exec_end"`) {
		t.Errorf("blocked attempt should still record tool_exec_end:\n%s", audit)
	}
}

// TestPolicyAllowsCleanTools: without a policy file the same flow runs the
// tool normally — the gate defaults to allow-all and stays invisible.
func TestPolicyAllowsCleanTools(t *testing.T) {
	home := tempDir(t)
	t.Setenv("LYON_HOME", home)
	newCompactMock(t, []mockapi.Turn{
		{Tool: &mockapi.ToolCall{Name: "noop", Input: "{}"}},
		{Text: []string{"done"}},
	})

	var ran atomic.Bool
	reg := tools.NewRegistry()
	if err := tools.RegisterFunc(reg, "noop", "does nothing", false,
		func(ctx context.Context, _ struct{}) (string, error) {
			ran.Store(true)
			return "ok", nil
		}); err != nil {
		t.Fatal(err)
	}

	eng := NewEngine(Config{Workspace: tempDir(t), CompactThreshold: 0.9}, reg)
	// Registered after home := t.TempDir(), so it runs before that
	// RemoveAll: the hooks writer must stop first or it recreates the
	// audit/stats files mid-delete ("directory is not empty").
	t.Cleanup(eng.Close)
	sess := eng.NewSession(nil)
	defer sess.Close()
	if err := sess.Submit("run the tool"); err != nil {
		t.Fatal(err)
	}
	pe, _ := waitPromptEndCollect(t, sess)
	if pe.Err != "" {
		t.Fatalf("prompt failed: %s", pe.Err)
	}
	if !ran.Load() {
		t.Error("clean tool did not execute")
	}
	if s := eng.Hooks().Stats(); s.SecurityBlocks != 0 {
		t.Errorf("security_blocks = %d, want 0", s.SecurityBlocks)
	}
}
