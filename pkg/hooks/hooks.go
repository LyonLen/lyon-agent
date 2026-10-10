// Package hooks gives the agent lifecycle hook points and the three
// consumers that ride on them: an append-only JSONL audit log, aggregate
// statistics, and security control (a local policy gate plus external
// blocking hooks that can veto a tool before it touches the workspace).
//
// Two paths keep observability and control cleanly separated:
//
//   - Fire is the observation path. Every meaningful session event passes
//     through once — counter update + queue append + non-blocking external
//     hooks (fire-and-forget goroutines). It never touches disk and never
//     blocks: all file I/O happens on a single writer goroutine that holds
//     no lock while doing it, so the engine's event path stays allocation-
//     cheap even under concurrent load.
//   - CheckTool is the control path. Local policy rules first (cheap,
//     regex, no subprocess), then external hooks subscribed to tool_start
//     with "blocking": true. A denial returns an error the model sees as
//     the tool result, and the attempt is audited with blocked_by set.
//
// Everything lives under $LYON_HOME (default ~/.lyon): hooks.json,
// policy.json, stats.json, audit/<date>.jsonl. Missing files are fine —
// hooks default to none, policy defaults to allow-all. LYON_AUDIT=off and
// LYON_HOOKS=off disable those outputs wholesale. Call Flush before exit
// (Session.Close does) so queued audit lines and stats hit disk.
package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Event names — these double as hook.json "event" selectors. "*" matches
// every event.
const (
	EvSessionStart  = "session_start"
	EvPromptStart   = "prompt_start"
	EvPromptEnd     = "prompt_end"
	EvTurnEnd       = "turn_end"
	EvToolUseStart  = "tool_use_start"
	EvToolExecStart = "tool_exec_start"
	EvToolExecEnd   = "tool_exec_end"
	EvApproveReq    = "approve_request"
	EvApproveRes    = "approve_result"
	EvCompaction    = "compaction"
	EvSubagentEnd   = "subagent_end"
	EvError         = "error"
	EvSecurityBlock = "security_block"
	EvHookError     = "hook_error"
	EvMemorySave    = "memory_save"
	EvRollback      = "rollback"
)

// Record is one audit line and one hook payload — the JSON external hooks
// receive on stdin. Fields are truncated centrally in Fire so every sink
// (file, hook, stats) sees bounded data.
type Record struct {
	Time       time.Time `json:"time"`
	Event      string    `json:"event"`
	Session    string    `json:"session"` // workspace the events belong to
	Tool       string    `json:"tool,omitempty"`
	Input      string    `json:"input,omitempty"`  // truncated tool input
	Output     string    `json:"output,omitempty"` // truncated detail text
	Prompt     string    `json:"prompt,omitempty"` // truncated prompt text
	DurationMS int64     `json:"duration_ms,omitempty"`
	IsError    bool      `json:"is_error,omitempty"`
	Err        string    `json:"error,omitempty"`
	Approved   *bool     `json:"approved,omitempty"`
	Usage      *Usage    `json:"usage,omitempty"`
	Iterations int       `json:"iterations,omitempty"`
	StopReason string    `json:"stop_reason,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	BlockedBy  string    `json:"blocked_by,omitempty"` // policy rule or hook reason
}

// Usage mirrors the token counts the API reports.
type Usage struct {
	Input         int64 `json:"input"`
	Output        int64 `json:"output"`
	CacheRead     int64 `json:"cache_read"`
	CacheCreation int64 `json:"cache_creation"`
}

// HookCfg is one entry of hooks.json:
//
//	{"event": "tool_end", "command": "node C:/hooks/log.js", "timeout_ms": 5000}
//	{"event": "tool_start", "argv": ["gate.exe"], "blocking": true}
//
// Exactly one of command (shell string) or argv (direct exec) is used.
// The Record is written to the hook's stdin. blocking:true is honored on
// tool_start only: exit code 2 or {"deny":true} on stdout vetoes the run.
type HookCfg struct {
	Event     string   `json:"event"`
	Command   string   `json:"command,omitempty"`
	Argv      []string `json:"argv,omitempty"`
	TimeoutMS int      `json:"timeout_ms,omitempty"`
	Blocking  bool     `json:"blocking,omitempty"`
}

// auditItem is one queue entry; done is non-nil only for Flush barriers.
type auditItem struct {
	rec  Record
	done chan struct{}
}

// Manager is the process-wide hook hub: one per Engine. Safe for
// concurrent use; counters live behind mu, disk I/O lives on the writer.
type Manager struct {
	workspace string
	home      string

	mu            sync.Mutex
	hooks         []HookCfg
	policy        *Policy
	stats         Stats
	hooksOn       bool
	auditOn       bool
	statsDirty    bool
	saveRequested bool
	lastSave      time.Time

	auditCh    chan auditItem
	writerOnce sync.Once
}

// New builds the manager for a workspace. It never fails: unreadable or
// partial config degrades to defaults (no hooks, allow-all policy), and
// outputs are started lazily — a broken audit dir disables the audit log
// instead of the agent.
func New(workspace string) *Manager {
	m := &Manager{workspace: workspace, hooksOn: true, auditOn: true}
	m.home = homeDir()
	if offEnv("LYON_HOOKS") {
		m.hooksOn = false
	}
	if offEnv("LYON_AUDIT") {
		m.auditOn = false
	}
	if m.hooksOn {
		if b, err := os.ReadFile(filepath.Join(m.home, "hooks.json")); err == nil {
			var cfgs []HookCfg
			if json.Unmarshal(b, &cfgs) == nil {
				for _, h := range cfgs {
					if h.Event != "" && (h.Command != "" || len(h.Argv) > 0) {
						m.hooks = append(m.hooks, h)
					}
				}
			}
		}
	}
	m.policy = loadPolicy(m.home)
	m.stats = loadStats(filepath.Join(m.home, "stats.json"))
	m.auditCh = make(chan auditItem, 4096)
	m.lastSave = time.Now()
	return m
}

// Stats returns a copy of the current aggregates (for /stats and tests).
func (m *Manager) Stats() Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stats.copy()
}

// Save persists stats.json if anything changed since the last save.
// Disk work happens outside the lock; returns after it lands.
func (m *Manager) Save() {
	b := m.statsSnapshot()
	if b != nil {
		m.writeStatsFile(b)
	}
}

// statsSnapshot clears the dirty flags and returns the encoded stats, or
// nil when nothing changed. File writing happens outside m.mu.
func (m *Manager) statsSnapshot() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.statsDirty {
		return nil
	}
	b, err := json.Marshal(m.stats)
	if err != nil {
		return nil
	}
	m.statsDirty = false
	m.saveRequested = false
	m.lastSave = time.Now()
	return b
}

func (m *Manager) writeStatsFile(b []byte) {
	if m.home == "" {
		return
	}
	if err := os.MkdirAll(m.home, 0o755); err != nil {
		return
	}
	// Unique temp name: Flush and the writer may race the rename, and the
	// last complete write wins — counters differ by at most one event.
	tmp, err := os.CreateTemp(m.home, "stats-*.json.tmp")
	if err != nil {
		return
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil || tmp.Close() != nil {
		os.Remove(name)
		return
	}
	if os.Rename(name, filepath.Join(m.home, "stats.json")) != nil {
		os.Remove(name)
	}
}

// Flush blocks until every queued audit line is on disk and stats are
// persisted — call before process exit (Session.Close does). Safe and
// idempotent; concurrent with the writer goroutine.
func (m *Manager) Flush() {
	m.writerOnce.Do(func() { go m.writeLoop() })
	// Barrier: the writer is the queue's single consumer, so once it
	// acknowledges the barrier, everything enqueued earlier is written.
	done := make(chan struct{})
	m.auditCh <- auditItem{done: done} // blocks only if queue is full — writer drains
	<-done
	if b := m.statsSnapshot(); b != nil {
		m.writeStatsFile(b)
	}
}

// Fire records one observation. Never touches disk and never blocks on
// hooks beyond a fast enqueue; external non-blocking hooks run on their
// own goroutines.
func (m *Manager) Fire(r Record) {
	if r.Time.IsZero() {
		r.Time = time.Now()
	}
	if r.Session == "" {
		r.Session = m.workspace
	}
	r.Input = truncRunes(r.Input, 600)
	r.Output = truncRunes(r.Output, 600)
	r.Prompt = truncRunes(r.Prompt, 400)
	r.Err = truncRunes(r.Err, 300)
	r.Detail = truncRunes(r.Detail, 300)

	m.mu.Lock()
	m.updateStatsLocked(&r)
	if r.Event == EvPromptEnd {
		m.saveRequested = true
	}
	auditOn := m.auditOn // flipped off under lock if the audit dir breaks
	var async []HookCfg
	if m.hooksOn {
		for _, h := range m.hooks {
			if h.Blocking && h.Event == EvToolExecStart {
				continue // control path only — never fire-and-forget
			}
			if h.Event == r.Event || h.Event == "*" {
				async = append(async, h)
			}
		}
	}
	m.mu.Unlock()

	m.writerOnce.Do(func() { go m.writeLoop() }) // stats persist even with audit off
	if auditOn {
		select {
		case m.auditCh <- auditItem{rec: r}:
		default:
			m.mu.Lock()
			m.stats.AuditDropped++
			m.mu.Unlock()
		}
	}

	for _, h := range async {
		go m.runAsync(h, r)
	}
}

// writeLoop is the single disk-I/O consumer. It holds m.mu only for
// counter/flag reads — never across a file operation, so Fire never
// queues behind AV-slowed writes. auditDay/prune state is goroutine-local
// on purpose: this loop is the only code that writes audit files.
func (m *Manager) writeLoop() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	auditDay := ""
	for {
		select {
		case item := <-m.auditCh:
			if item.done != nil {
				close(item.done)
				continue
			}
			m.writeAuditLine(item.rec, &auditDay)
			m.saveDue()
		case <-ticker.C:
			m.saveDue()
		}
	}
}

// saveDue persists stats when a prompt just ended or at most every 2s —
// never more often, whatever the event rate. Encoding under the lock is
// microseconds; the file write runs outside it.
func (m *Manager) saveDue() {
	m.mu.Lock()
	due := m.statsDirty && (m.saveRequested || time.Since(m.lastSave) >= 2*time.Second)
	m.mu.Unlock()
	if !due {
		return
	}
	if b := m.statsSnapshot(); b != nil {
		m.writeStatsFile(b)
	}
}

// writeAuditLine appends one JSONL line for today, rotating the day file
// and pruning old ones. *day is the writer goroutine's own rotation state.
func (m *Manager) writeAuditLine(r Record, day *string) {
	m.mu.Lock()
	auditOn := m.auditOn
	m.mu.Unlock()
	if !auditOn {
		return
	}
	today := time.Now().Format("2006-01-02")
	if *day != today {
		dir := filepath.Join(m.home, "audit")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			m.mu.Lock()
			m.auditOn = false // broken output must not break the agent
			m.mu.Unlock()
			return
		}
		pruneAudit(dir, 30)
		*day = today
	}
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(m.home, "audit", today+".jsonl"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		m.mu.Lock()
		m.auditOn = false
		m.mu.Unlock()
		return
	}
	defer f.Close()
	f.Write(append(b, '\n'))
}

// CheckTool is the security seam: local policy first, then blocking
// tool_start hooks. A non-nil error must be returned to the model as the
// tool result — the attempt is audited with blocked_by set either way.
func (m *Manager) CheckTool(ctx context.Context, name string, input []byte) error {
	m.mu.Lock()
	policy := m.policy
	var blocking []HookCfg
	if m.hooksOn {
		for _, h := range m.hooks {
			if h.Blocking && (h.Event == EvToolExecStart || h.Event == "*") {
				blocking = append(blocking, h)
			}
		}
	}
	m.mu.Unlock()

	if rule, denied := policy.Check(name, input); denied {
		m.Fire(Record{
			Event:     EvSecurityBlock,
			Tool:      name,
			Input:     string(input),
			BlockedBy: "policy:" + rule,
			Err:       "denied by local security policy",
			IsError:   true,
		})
		return fmt.Errorf("blocked by security policy (%s)", rule)
	}
	for _, h := range blocking {
		rec := Record{
			Time:    time.Now(),
			Event:   EvToolExecStart,
			Session: m.workspace,
			Tool:    name,
			Input:   truncRunes(string(input), 600),
		}
		stdout, code, runErr := runHook(ctx, h, rec)
		if denied, reason := parseDeny(stdout, code, runErr); denied {
			m.Fire(Record{
				Event:     EvSecurityBlock,
				Tool:      name,
				Input:     string(input),
				BlockedBy: "hook: " + hookLabel(h),
				Err:       reason,
				IsError:   true,
			})
			return fmt.Errorf("blocked by hook: %s", reason)
		}
	}
	return nil
}

// runAsync executes a non-blocking hook; failures are counted and audited
// but never surface to the caller.
func (m *Manager) runAsync(h HookCfg, r Record) {
	stdout, code, err := runHook(context.Background(), h, r)
	if err != nil || (code != 0 && code != -1) {
		reason := fmt.Sprintf("exit=%d err=%v", code, err)
		if s := strings.TrimSpace(stdout); s != "" && err == nil {
			reason += " stdout=" + truncRunes(s, 160)
		}
		m.Fire(Record{Event: EvHookError, Tool: r.Tool, Detail: hookLabel(h) + ": " + reason})
	}
}

// updateStatsLocked folds one record into the aggregates.
func (m *Manager) updateStatsLocked(r *Record) {
	s := &m.stats
	s.LastActive = r.Time
	m.statsDirty = true
	switch r.Event {
	case EvPromptStart:
		s.Prompts++
	case EvTurnEnd:
		s.ApiCalls++
		if r.Usage != nil {
			s.Tokens.Input += r.Usage.Input
			s.Tokens.Output += r.Usage.Output
			s.Tokens.CacheRead += r.Usage.CacheRead
			s.Tokens.CacheCreation += r.Usage.CacheCreation
		}
	case EvSubagentEnd:
		s.Subagents++
		if r.Usage != nil {
			s.Tokens.Input += r.Usage.Input
			s.Tokens.Output += r.Usage.Output
			s.Tokens.CacheRead += r.Usage.CacheRead
			s.Tokens.CacheCreation += r.Usage.CacheCreation
		}
	case EvToolExecEnd:
		if s.Tools == nil {
			s.Tools = map[string]*ToolStats{}
		}
		t := s.Tools[r.Tool]
		if t == nil {
			t = &ToolStats{}
			s.Tools[r.Tool] = t
		}
		t.Calls++
		if r.IsError {
			t.Errors++
		}
		t.TotalMS += r.DurationMS
	case EvApproveReq:
		s.ApprovalsReq++
	case EvApproveRes:
		if r.Approved != nil && *r.Approved {
			s.ApprovalsYes++
		} else {
			s.ApprovalsNo++
		}
	case EvSecurityBlock:
		s.SecurityBlocks++
	case EvCompaction:
		s.Compactions++
	case EvHookError:
		s.HookFailures++
	}
}

// runHook executes one hook: Record on stdin, stdout captured for the deny
// protocol, timeout from cfg (default 10s). Returns stdout and exit code
// (-1 when the process never exited cleanly).
func runHook(ctx context.Context, h HookCfg, r Record) (stdout string, code int, err error) {
	timeout := time.Duration(h.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var c *exec.Cmd
	if len(h.Argv) > 0 {
		c = exec.CommandContext(ctx, h.Argv[0], h.Argv[1:]...)
	} else {
		c = shellCommand(ctx, h.Command)
	}
	in, _ := json.Marshal(r)
	c.Stdin = strings.NewReader(string(in))
	var out strings.Builder
	c.Stdout = &out
	c.Stderr = &out
	runErr := c.Run()
	code = 0
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = -1
			return out.String(), code, runErr
		}
	}
	return out.String(), code, nil
}

// parseDeny implements the blocking-hook protocol: exit code 2 vetoes
// (Claude Code convention), as does {"deny":true,...} on stdout. Other
// outcomes — including hook crashes — allow the tool: local policy and
// approval already gate execution, and a flaky observer must not freeze
// the agent.
func parseDeny(stdout string, code int, err error) (bool, string) {
	if code == 2 {
		reason := strings.TrimSpace(stdout)
		if reason == "" {
			reason = "hook exited 2"
		}
		return true, truncRunes(reason, 200)
	}
	var resp struct {
		Deny   bool   `json:"deny"`
		Reason string `json:"reason"`
	}
	if json.Unmarshal([]byte(stdout), &resp) == nil && resp.Deny {
		reason := resp.Reason
		if reason == "" {
			reason = "hook returned deny"
		}
		return true, truncRunes(reason, 200)
	}
	return false, ""
}

func hookLabel(h HookCfg) string {
	if len(h.Argv) > 0 {
		return strings.Join(h.Argv, " ")
	}
	return h.Command
}

// shellCommand routes a shell-string hook to the platform shell.
func shellCommand(ctx context.Context, cmd string) *exec.Cmd {
	if isWindows() {
		return exec.CommandContext(ctx, "cmd", "/C", cmd)
	}
	return exec.CommandContext(ctx, "sh", "-c", cmd)
}

func isWindows() bool { return os.PathSeparator == '\\' }

// truncRunes cuts s to at most n bytes on a rune boundary (audit payloads
// must stay valid UTF-8 JSON).
func truncRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	b := []byte(s[:n])
	for len(b) > 0 && b[len(b)-1]&0xC0 == 0x80 {
		b = b[:len(b)-1]
	}
	return string(b) + "…"
}

// offEnv reports whether an env var requests a feature be disabled.
func offEnv(key string) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "off", "0", "false", "no":
		return true
	}
	return false
}

// homeDir resolves $LYON_HOME (default ~/.lyon), mirroring pkg/agent.
func homeDir() string {
	if h := os.Getenv("LYON_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".lyon")
}

// pruneAudit deletes audit files older than keep days (best effort).
func pruneAudit(dir string, keep int) {
	entries, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -keep)
	for _, p := range entries {
		name := filepath.Base(p)
		day, err := time.Parse("2006-01-02", strings.TrimSuffix(name, ".jsonl"))
		if err == nil && day.Before(cutoff) {
			os.Remove(p)
		}
	}
}
