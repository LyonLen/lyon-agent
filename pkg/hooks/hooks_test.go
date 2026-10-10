package hooks

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHookHelperProcess is not a real test: it is the child process that
// blocking/async hooks exec (see the argv pattern in hook tests). It does
// nothing unless LYON_HOOK_HELPER selects a behavior.
func TestHookHelperProcess(t *testing.T) {
	mode := os.Getenv("LYON_HOOK_HELPER")
	if mode == "" {
		return
	}
	buf := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buf) // consume the Record
		if n == 0 || err != nil {
			break
		}
	}
	switch mode {
	case "deny":
		t.Logf(`{"deny":true,"reason":"helper says no"}`)
		os.Stdout.WriteString(`{"deny":true,"reason":"helper says no"}`)
		os.Exit(0)
	case "exit2":
		os.Stdout.WriteString("blocked by helper policy\n")
		os.Exit(2)
	case "touch":
		if p := os.Getenv("LYON_HOOK_TOUCH"); p != "" {
			os.WriteFile(p, []byte("ok"), 0o644)
		}
		os.Exit(0)
	}
	os.Exit(0)
}

func helperArgv() []string {
	return []string{os.Args[0], "-test.run=^TestHookHelperProcess$"}
}

// TestPolicyCheck: allowlist mode, named denials, case-insensitive input
// patterns, and malformed policy degrading to allow-all.
func TestPolicyCheck(t *testing.T) {
	dir := t.TempDir()
	p := loadPolicy(dir) // missing file: allow-all
	if _, denied := p.Check("bash", []byte(`{"command":"rm -rf /"}`)); denied {
		t.Error("missing policy must allow all")
	}

	write := func(s string) {
		if err := os.WriteFile(filepath.Join(dir, "policy.json"), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"deny_tools":["bash"],"deny_patterns":["(?i)rm\\s+-rf\\s+/"]}`)
	p = loadPolicy(dir)
	if rule, denied := p.Check("bash", nil); !denied || !strings.Contains(rule, "bash") {
		t.Errorf("deny_tools: got rule=%q denied=%v", rule, denied)
	}
	if _, denied := p.Check("read", []byte(`{"path":"x"}`)); denied {
		t.Error("clean read must pass")
	}
	if rule, denied := p.Check("shell", []byte(`{"cmd":"RM -RF /"}`)); !denied || !strings.Contains(rule, "deny pattern") {
		t.Errorf("pattern (case-insensitive): rule=%q denied=%v", rule, denied)
	}

	write(`{"allow_tools":["read","grep"]}`)
	p = loadPolicy(dir)
	if _, denied := p.Check("shell", nil); !denied {
		t.Error("allowlist mode must deny unnamed tools")
	}
	if _, denied := p.Check("read", nil); denied {
		t.Error("allowlist mode must allow named tools")
	}

	write(`{broken json`)
	p = loadPolicy(dir)
	if _, denied := p.Check("bash", nil); denied {
		t.Error("malformed policy must degrade to allow-all")
	}
}

// TestFireAuditsAndStats: Fire writes one parseable JSONL line per event,
// updates the aggregates, and persists stats.json on prompt_end.
func TestFireAuditsAndStats(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LYON_HOME", dir)
	m := New(`D:\some\workspace`)

	yes := true
	m.Fire(Record{Event: EvPromptStart, Prompt: "hi"})
	m.Fire(Record{Event: EvTurnEnd, Usage: &Usage{Input: 100, Output: 50, CacheRead: 1000}})
	m.Fire(Record{Event: EvToolExecEnd, Tool: "shell", DurationMS: 42})
	m.Fire(Record{Event: EvToolExecEnd, Tool: "shell", DurationMS: 8, IsError: true})
	m.Fire(Record{Event: EvApproveReq, Tool: "shell"})
	m.Fire(Record{Event: EvApproveRes, Tool: "shell", Approved: &yes})
	m.Fire(Record{Event: EvSecurityBlock, Tool: "shell", BlockedBy: "policy: tool denied: shell"})
	m.Fire(Record{Event: EvPromptEnd, Iterations: 3})
	m.Flush() // audit/stats hit disk asynchronously — flush before asserting

	// Aggregates.
	s := m.Stats()
	if s.Prompts != 1 || s.ApiCalls != 1 || s.SecurityBlocks != 1 {
		t.Errorf("counters: %+v", s)
	}
	if s.ApprovalsReq != 1 || s.ApprovalsYes != 1 || s.ApprovalsNo != 0 {
		t.Errorf("approvals: %+v", s)
	}
	if s.Tokens.Input != 100 || s.Tokens.CacheRead != 1000 {
		t.Errorf("tokens: %+v", s.Tokens)
	}
	ts := s.Tools["shell"]
	if ts == nil || ts.Calls != 2 || ts.Errors != 1 || ts.TotalMS != 50 {
		t.Errorf("tool stats: %+v", ts)
	}

	// Audit file: one line per event, valid JSON, session stamped.
	auditPath := filepath.Join(dir, "audit", time.Now().Format("2006-01-02")+".jsonl")
	f, err := os.Open(auditPath)
	if err != nil {
		t.Fatalf("audit file: %v", err)
	}
	defer f.Close()
	var lines int
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("audit line %d invalid: %v", lines, err)
		}
		if r.Session != `D:\some\workspace` {
			t.Errorf("audit line %d missing session: %q", lines, r.Session)
		}
		lines++
	}
	if lines != 8 {
		t.Errorf("audit lines = %d, want 8", lines)
	}

	// Persistence happened on prompt_end.
	b, err := os.ReadFile(filepath.Join(dir, "stats.json"))
	if err != nil {
		t.Fatalf("stats.json: %v", err)
	}
	var persisted Stats
	if err := json.Unmarshal(b, &persisted); err != nil || persisted.Prompts != 1 {
		t.Errorf("persisted stats: err=%v prompts=%d", err, persisted.Prompts)
	}
}

// TestBlockingHookDenies: exit 2 and {"deny":true} both veto the tool via
// CheckTool, and the denial lands in the audit with blocked_by set.
func TestBlockingHookDenies(t *testing.T) {
	for _, mode := range []string{"exit2", "deny"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("LYON_HOME", dir)
			t.Setenv("LYON_HOOK_HELPER", mode)
			cfg, _ := json.Marshal([]HookCfg{{
				Event: EvToolExecStart, Argv: helperArgv(), Blocking: true, TimeoutMS: 10000,
			}})
			if err := os.WriteFile(filepath.Join(dir, "hooks.json"), cfg, 0o644); err != nil {
				t.Fatal(err)
			}
			m := New("ws")
			err := m.CheckTool(context.Background(), "shell", []byte(`{"command":"ls"}`))
			if err == nil {
				t.Fatal("blocking hook must veto the tool")
			}
			if mode == "deny" && !strings.Contains(err.Error(), "helper says no") {
				t.Errorf("deny reason not surfaced: %v", err)
			}
			m.Flush()
			// Audit has the security_block line with blocked_by.
			b, rerr := os.ReadFile(filepath.Join(dir, "audit", time.Now().Format("2006-01-02")+".jsonl"))
			if rerr != nil || !strings.Contains(string(b), `"blocked_by":"hook`) {
				t.Errorf("security_block not audited: err=%v data=%s", rerr, b)
			}
		})
	}
}

// TestPolicyDenyBeatsHooks: policy denies before any subprocess runs, and
// a clean tool passes CheckTool when nothing objects.
func TestPolicyDenyBeatsHooks(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LYON_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "policy.json"),
		[]byte(`{"deny_tools":["shell"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m := New("ws")
	if err := m.CheckTool(context.Background(), "shell", nil); err == nil {
		t.Error("policy deny must return an error")
	}
	if err := m.CheckTool(context.Background(), "read", []byte(`{"path":"x"}`)); err != nil {
		t.Errorf("clean tool blocked: %v", err)
	}
}

// TestAsyncHookRuns: a non-blocking hook fires in the background and its
// Record arrives on stdin (the helper proves both by touching a file).
func TestAsyncHookRuns(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LYON_HOME", dir)
	t.Setenv("LYON_HOOK_HELPER", "touch")
	marker := filepath.Join(dir, "touched")
	t.Setenv("LYON_HOOK_TOUCH", marker)
	cfg, _ := json.Marshal([]HookCfg{{
		Event: EvPromptStart, Argv: helperArgv(), TimeoutMS: 10000,
	}})
	if err := os.WriteFile(filepath.Join(dir, "hooks.json"), cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	m := New("ws")
	m.Fire(Record{Event: EvPromptStart, Prompt: "hello"})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return // async hook ran
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("async hook never ran")
}

// TestTruncRunes: byte budget cuts on rune boundaries — audit lines must
// stay valid UTF-8.
func TestTruncRunes(t *testing.T) {
	s := strings.Repeat("中", 100) // 3 bytes each
	got := truncRunes(s, 10)      // lands inside a rune
	if !json.Valid([]byte(`"` + got + `"`)) {
		t.Fatalf("truncation broke UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("missing ellipsis: %q", got)
	}
	if got := truncRunes("short", 10); got != "short" {
		t.Errorf("short string modified: %q", got)
	}
}
