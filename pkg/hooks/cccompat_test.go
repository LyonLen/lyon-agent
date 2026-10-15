package hooks

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeClaudeSettings drops a settings file into dir and returns its path.
func writeClaudeSettings(t *testing.T, dir string, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// ccSettings builds a Claude Code settings object with a hooks section
// keyed by event name; each value is one outer matcher group.
func ccSettings(hooks map[string]any) map[string]any {
	return map[string]any{"hooks": hooks, "model": "ignored-sibling"}
}

// captureStderr runs fn with os.Stderr redirected and returns what it
// printed — the load-time unmapped warning is the only output.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	fn()
	os.Stderr = old
	w.Close()
	b, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestParseClaudeMapping: every Claude Code event we claim to support
// maps onto the intended lyon event; events without an equivalent come
// back unmapped (LyonEvent "") instead of failing the parse.
func TestParseClaudeMapping(t *testing.T) {
	want := map[string]string{
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
	unmapped := []string{"SessionEnd", "Notification", "PostToolUseFailure", "TeammateIdle"}

	sections := map[string]any{}
	for ev := range want {
		sections[ev] = []map[string]any{{"hooks": []map[string]any{{"type": "command", "command": "true"}}}}
	}
	for _, ev := range unmapped {
		sections[ev] = []map[string]any{{"hooks": []map[string]any{{"type": "command", "command": "true"}}}}
	}

	data, err := json.Marshal(ccSettings(sections))
	if err != nil {
		t.Fatal(err)
	}
	groups, err := ParseClaudeSettingsHooks(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(groups) != len(want)+len(unmapped) {
		t.Fatalf("groups = %d, want %d", len(groups), len(want)+len(unmapped))
	}
	seen := 0
	for _, g := range groups {
		if exp, ok := want[g.Event]; ok {
			seen++
			if g.LyonEvent != exp {
				t.Errorf("%s → %q, want %q", g.Event, g.LyonEvent, exp)
			}
		} else if containsStr(unmapped, g.Event) {
			if g.LyonEvent != "" {
				t.Errorf("%s must be unmapped, got %q", g.Event, g.LyonEvent)
			}
		} else {
			t.Errorf("unexpected event %q in groups", g.Event)
		}
	}
	if seen != len(want) {
		t.Errorf("mapped groups = %d, want %d", seen, len(want))
	}
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// TestParseMatcherPreserved: the matcher survives parsing verbatim, both
// when present (regex form) and when absent.
func TestParseMatcherPreserved(t *testing.T) {
	data, err := json.Marshal(ccSettings(map[string]any{
		"PreToolUse": []map[string]any{
			{"matcher": "Bash|mcp__.*", "hooks": []map[string]any{{"type": "command", "command": "a"}}},
			{"hooks": []map[string]any{{"type": "command", "command": "b"}}},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	groups, err := ParseClaudeSettingsHooks(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(groups))
	}
	if groups[0].Matcher != "Bash|mcp__.*" {
		t.Errorf("matcher = %q, want it preserved verbatim", groups[0].Matcher)
	}
	if groups[1].Matcher != "" {
		t.Errorf("absent matcher = %q, want empty", groups[1].Matcher)
	}
}

// TestParseBadJSONNoPanic: malformed or mistyped input returns an error
// (both from the parser and from the loader) without panicking or
// registering anything; a settings file without a hooks section parses
// to nothing and loads cleanly.
func TestParseBadJSONNoPanic(t *testing.T) {
	for _, in := range []string{
		`{broken`,
		`{"hooks": "nope"}`,
		`{"hooks": {"PreToolUse": 42}}`,
		`[1,2,3]`,
		`"just a string"`,
	} {
		if groups, err := ParseClaudeSettingsHooks([]byte(in)); err == nil {
			t.Errorf("input %s: expected error, got %d groups", in, len(groups))
		}
	}

	dir := tempDir(t)
	t.Setenv("LYON_HOME", dir)
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{broken`), 0o644); err != nil {
		t.Fatal(err)
	}
	m := New("ws")
	t.Cleanup(m.Close)
	loaded, skipped, err := LoadClaudeSettings(m, path)
	if err == nil || loaded != 0 || skipped != 0 {
		t.Errorf("bad JSON: loaded=%d skipped=%d err=%v", loaded, skipped, err)
	}
	m.mu.Lock()
	n := len(m.hooks)
	m.mu.Unlock()
	if n != 0 {
		t.Errorf("bad JSON registered %d hooks", n)
	}

	// No hooks section at all (the shape of this repo's own .claude/settings.json),
	// and a blank file — both are clean no-ops.
	empty, err := ParseClaudeSettingsHooks([]byte(`{"worktree":{"bgIsolation":"none"}}`))
	if err != nil || len(empty) != 0 {
		t.Errorf("hooks-less settings: groups=%d err=%v, want none/nil", len(empty), err)
	}
	if groups, err := ParseClaudeSettingsHooks(nil); err != nil || len(groups) != 0 {
		t.Errorf("blank settings: groups=%d err=%v, want none/nil", len(groups), err)
	}
}

// TestLoadCountsSkipsAndWarnsOnce: mappable command entries load;
// unsupported types and entries under unmapped events are counted as
// skipped; the unmapped events are reported on stderr once per load.
func TestLoadCountsSkipsAndWarnsOnce(t *testing.T) {
	dir := tempDir(t)
	t.Setenv("LYON_HOME", dir)
	path := writeClaudeSettings(t, dir, ccSettings(map[string]any{
		"PreToolUse": []map[string]any{
			{"matcher": "Bash|Edit", "hooks": []map[string]any{{"type": "command", "command": "exit 0"}}},
		},
		"PostToolUse": []map[string]any{
			{"hooks": []map[string]any{{"type": "http", "command": "https://x/h"}}},
		},
		"Notification": []map[string]any{
			{"hooks": []map[string]any{{"type": "command", "command": "notify"}}},
		},
		"SessionEnd": []map[string]any{
			{"hooks": []map[string]any{
				{"type": "command", "command": "bye"},
				{"type": "prompt", "command": "summarize"},
			}},
		},
	}))

	m := New("ws")
	t.Cleanup(m.Close)
	var loaded, skipped int
	var err error
	warn := captureStderr(t, func() {
		loaded, skipped, err = LoadClaudeSettings(m, path)
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded != 1 {
		t.Errorf("loaded = %d, want 1 (only the PreToolUse command)", loaded)
	}
	if skipped != 4 { // http + Notification(1) + SessionEnd(2)
		t.Errorf("skipped = %d, want 4", skipped)
	}
	if !strings.Contains(warn, "Notification(1)") || !strings.Contains(warn, "SessionEnd(2)") {
		t.Errorf("unmapped warning missing: %q", warn)
	}
	if strings.Contains(warn, "PreToolUse") || strings.Contains(warn, "PostToolUse") {
		t.Errorf("mapped events must not be warned about: %q", warn)
	}
	if n := strings.Count(warn, "unmapped Claude Code event"); n != 1 {
		t.Errorf("warning printed %d times, want exactly 1", n)
	}

	// Registration details: PreToolUse rides the blocking control path,
	// PostToolUse-style entries (here the mapped ones) stay async.
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.hooks) != 1 {
		t.Fatalf("registered hooks = %d, want 1", len(m.hooks))
	}
	h := m.hooks[0]
	if h.Event != EvToolExecStart || !h.Blocking {
		t.Errorf("PreToolUse → %+v, want tool_exec_start blocking", h)
	}
}

// TestLoadHonorsTimeoutsAndExecForm: Claude Code's seconds convert to
// runHook's milliseconds (default 60s when absent), and the args exec
// form lands in Argv.
func TestLoadHonorsTimeoutsAndExecForm(t *testing.T) {
	dir := tempDir(t)
	t.Setenv("LYON_HOME", dir)
	path := writeClaudeSettings(t, dir, ccSettings(map[string]any{
		"Stop": []map[string]any{
			{"hooks": []map[string]any{
				{"type": "command", "command": "slow.sh", "timeout": 30},
				{"type": "command", "command": "quick.sh"},
				{"type": "command", "args": []string{"gate.exe", "--x"}},
			}},
		},
	}))
	m := New("ws")
	t.Cleanup(m.Close)
	loaded, skipped, err := LoadClaudeSettings(m, path)
	if err != nil || loaded != 3 || skipped != 0 {
		t.Fatalf("loaded=%d skipped=%d err=%v", loaded, skipped, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hooks[0].TimeoutMS != 30_000 {
		t.Errorf("timeout = %d, want 30000 (seconds → ms)", m.hooks[0].TimeoutMS)
	}
	if m.hooks[1].TimeoutMS != ccDefaultTimeoutMS {
		t.Errorf("default timeout = %d, want %d", m.hooks[1].TimeoutMS, ccDefaultTimeoutMS)
	}
	if m.hooks[2].Command != "" || len(m.hooks[2].Argv) != 2 || m.hooks[2].Argv[0] != "gate.exe" {
		t.Errorf("exec form not registered as argv: %+v", m.hooks[2])
	}
}

// TestLoadMissingFileAndHooksOff: a missing settings file is a clean
// no-op, and LYON_HOOKS=off refuses to load even a valid one.
func TestLoadMissingFileAndHooksOff(t *testing.T) {
	dir := tempDir(t)
	t.Setenv("LYON_HOME", dir)

	m := New("ws")
	t.Cleanup(m.Close)
	loaded, skipped, err := LoadClaudeSettings(m, filepath.Join(dir, "nope", "settings.json"))
	if loaded != 0 || skipped != 0 || err != nil {
		t.Errorf("missing file: loaded=%d skipped=%d err=%v, want clean no-op", loaded, skipped, err)
	}

	path := writeClaudeSettings(t, dir, ccSettings(map[string]any{
		"Stop": []map[string]any{{"hooks": []map[string]any{{"type": "command", "command": "x"}}}},
	}))

	t.Setenv("LYON_HOOKS", "off")
	m2 := New("ws")
	t.Cleanup(m2.Close)
	loaded, skipped, err = LoadClaudeSettings(m2, path)
	if loaded != 0 || skipped != 0 || err != nil {
		t.Errorf("hooks off: loaded=%d skipped=%d err=%v, want refusal", loaded, skipped, err)
	}
	m2.mu.Lock()
	n := len(m2.hooks)
	m2.mu.Unlock()
	if n != 0 {
		t.Errorf("LYON_HOOKS=off registered %d hooks", n)
	}
}

// requireQuoteFreeDir guards marker files written through a shell
// string: cmd /C and sh -c both choke on embedded quoting once Go's argv
// escaping is applied (and a space in the path would swallow the
// redirection target), so these tests keep the command quote-free and
// skip on spaced temp roots.
func requireQuoteFreeDir(t *testing.T, dir string) {
	t.Helper()
	if strings.Contains(dir, " ") {
		t.Skipf("temp dir %q contains a space; shell marker out of scope", dir)
	}
}

// TestLoadedClaudeHookRunsOnFire: end-to-end — two mapped events load,
// Fire runs each registered command (marker files prove execution through
// the normal async runHook path).
func TestLoadedClaudeHookRunsOnFire(t *testing.T) {
	dir := tempDir(t)
	t.Setenv("LYON_HOME", dir)
	requireQuoteFreeDir(t, dir)
	m1 := filepath.Join(dir, "m1")
	m2 := filepath.Join(dir, "m2")
	path := writeClaudeSettings(t, dir, ccSettings(map[string]any{
		"UserPromptSubmit": []map[string]any{
			{"hooks": []map[string]any{{"type": "command", "command": "echo ok > " + m1}}},
		},
		"PostToolUse": []map[string]any{
			{"matcher": ".*", "hooks": []map[string]any{{"type": "command", "command": "echo ok > " + m2}}},
		},
	}))

	m := New("ws")
	t.Cleanup(m.Close)
	loaded, _, err := LoadClaudeSettings(m, path)
	if err != nil || loaded != 2 {
		t.Fatalf("loaded=%d err=%v, want 2", loaded, err)
	}
	m.Fire(Record{Event: EvPromptStart, Prompt: "hi"})
	m.Fire(Record{Event: EvToolExecEnd, Tool: "shell", DurationMS: 1})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, e1 := os.Stat(m1)
		_, e2 := os.Stat(m2)
		if e1 == nil && e2 == nil {
			return // both migrated hooks executed
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, e1 := os.Stat(m1)
	_, e2 := os.Stat(m2)
	t.Fatalf("loaded claude hooks did not run: prompt_start marker err=%v, tool_exec_end marker err=%v", e1, e2)
}

// TestLoadedPreToolUseBlocks: a PreToolUse entry registers as blocking —
// CheckTool runs it before execution; exit 2 vetoes the tool (Claude
// Code's convention, honored by parseDeny) and the command really ran,
// while an observer entry (exit 0) lets the tool through.
func TestLoadedPreToolUseBlocks(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		blocked bool
	}{
		{"veto", `echo ran > %MARKER% && exit 2`, true},
		{"observer", `echo ran > %MARKER% && exit 0`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := tempDir(t)
			t.Setenv("LYON_HOME", dir)
			requireQuoteFreeDir(t, dir)
			marker := filepath.Join(dir, "ran")
			cmd := strings.ReplaceAll(tc.command, "%MARKER%", marker)
			path := writeClaudeSettings(t, dir, ccSettings(map[string]any{
				"PreToolUse": []map[string]any{
					{"matcher": "shell", "hooks": []map[string]any{{"type": "command", "command": cmd}}},
				},
			}))
			m := New("ws")
			t.Cleanup(m.Close)
			if _, _, err := LoadClaudeSettings(m, path); err != nil {
				t.Fatal(err)
			}
			err := m.CheckTool(context.Background(), "shell", []byte(`{"command":"ls"}`))
			if tc.blocked {
				if err == nil || !strings.Contains(err.Error(), "blocked by hook") {
					t.Fatalf("exit-2 hook must veto the tool, got err=%v", err)
				}
			} else if err != nil {
				t.Fatalf("observer hook must not block: %v", err)
			}
			if _, serr := os.Stat(marker); serr != nil {
				t.Errorf("hook command never ran: %v", serr)
			}
		})
	}
}
