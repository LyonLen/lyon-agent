// Package doctor implements `lyon doctor`: a one-shot health check whose
// results cmd/lyon renders as a paste-able report. Run is pure data
// collection — it never writes to stdout and never mutates state beyond
// the probe itself (a scratch file for the writability check). Every
// check is independent I/O, so they all run concurrently: the report
// lands in the time of the slowest check, not the sum.
package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/config"
	"lyon-agent/pkg/eval"
	"lyon-agent/pkg/mcp"
	"lyon-agent/pkg/memory"
	"lyon-agent/pkg/routing"
)

// Status is one check's verdict. OK counts toward the exit code 0; Warn
// is informational (exit 0); Fail is the only status that fails doctor.
type Status int

const (
	StatusOK Status = iota
	StatusWarn
	StatusFail
)

func (s Status) String() string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusWarn:
		return "warn"
	case StatusFail:
		return "fail"
	}
	return "?"
}

// Check is one line of the report: a name, a verdict, a one-line detail
// and — for Warn/Fail — a suggested fix.
type Check struct {
	Name   string
	Status Status
	Detail string
	Fix    string
}

// Options tunes a Run. Workspace defaults to the current directory;
// Timeout overrides each network check's own deadline when positive.
type Options struct {
	Workspace string
	Timeout   time.Duration
}

// timeout returns the per-check deadline: the caller's override when
// set, else the check's default (gateway 8s, mcp 15s, lyond 2s).
func (o Options) timeout(d time.Duration) time.Duration {
	if o.Timeout > 0 {
		return o.Timeout
	}
	return d
}

// lyondAddr is the daemon doctor probes. A var so tests can point it at
// a local listener without touching the real 8080.
var lyondAddr = "127.0.0.1:8080"

// Run executes every check concurrently and returns them in report
// order. A panicking check degrades to a Fail line instead of taking
// the whole report down — doctor must always produce output.
func Run(ctx context.Context, opts Options) []Check {
	if strings.TrimSpace(opts.Workspace) == "" {
		if wd, err := os.Getwd(); err == nil {
			opts.Workspace = wd
		}
	}
	type named struct {
		name string
		fn   func(context.Context, Options) Check
	}
	all := []named{
		{"gateway", checkGateway},
		{"mcp", checkMCP},
		{"memory", checkMemory},
		{"config", checkConfig},
		{"workspace", checkWorkspace},
		{"git", checkGit},
		{"sessions", checkSessions},
		{"lyond", checkLyond},
		{"logs", checkLogs},
		{"leaderboard", checkLeaderboard},
		{"routing", checkRouting},
	}
	out := make([]Check, len(all))
	var wg sync.WaitGroup
	for i, n := range all {
		wg.Add(1)
		go func(i int, n named) {
			defer wg.Done()
			out[i] = guard(n.name, func() Check { return n.fn(ctx, opts) })
		}(i, n)
	}
	wg.Wait()
	return out
}

// guard converts a panicking check into a Fail line with the panic text
// as the detail — a diagnostic tool must not crash on its own probe.
func guard(name string, fn func() Check) (c Check) {
	defer func() {
		if r := recover(); r != nil {
			c = Check{
				Name:   name,
				Status: StatusFail,
				Detail: oneline(fmt.Sprintf("check panicked: %v", r)),
				Fix:    "please report this as a bug with the full doctor output",
			}
		}
	}()
	return fn()
}

// --- gateway -------------------------------------------------------------

// gatewayFix is the shared remediation line for endpoint failures.
const gatewayFix = "check LYON_BASE_URL / LYON_API_KEY (or ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN)"

// envFirst returns the first of keys set to a non-empty value — the same
// LYON_*-before-ANTHROPIC_* precedence the engine uses (#54), copied
// here so doctor stays free of pkg/agent internals.
func envFirst(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// gatewayBaseURL is the endpoint: LYON_BASE_URL, else ANTHROPIC_BASE_URL.
func gatewayBaseURL() string {
	return envFirst("LYON_BASE_URL", "ANTHROPIC_BASE_URL")
}

// gatewayKey is the credential the engine would send: an API key wins
// over an auth token, LYON_* over ANTHROPIC_* (mirrors agent.endpointOptions).
func gatewayKey() string {
	if k := envFirst("LYON_API_KEY", "ANTHROPIC_API_KEY"); k != "" {
		return k
	}
	return envFirst("LYON_AUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN")
}

// modelsURL resolves the model-list URL. An OpenAI-style base already
// carries /v1, so models hang off it directly; a bare host gets the
// Anthropic-style /v1/models appended. No base at all means the
// Anthropic default host.
func modelsURL(base string) string {
	b := strings.TrimRight(strings.TrimSpace(base), "/")
	if b == "" {
		b = "https://api.anthropic.com"
	}
	if strings.HasSuffix(b, "/v1") {
		return b + "/models"
	}
	return b + "/v1/models"
}

// checkGateway probes GET {base}/v1/models with both credential headers
// riding along so one path serves Anthropic-protocol gateways (x-api-key)
// and OpenAI-protocol ones (Authorization: Bearer).
func checkGateway(ctx context.Context, opts Options) Check {
	url := modelsURL(gatewayBaseURL())
	key := gatewayKey()
	to := opts.timeout(8 * time.Second)
	cctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return Check{Name: "gateway", Status: StatusFail,
			Detail: oneline(err.Error()), Fix: gatewayFix}
	}
	req.Header.Set("Accept", "application/json")
	if key != "" {
		req.Header.Set("x-api-key", key)
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		detail := fmt.Sprintf("GET %s: %v", url, err)
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			detail = fmt.Sprintf("GET %s: timeout after %s", url, to)
		}
		return Check{Name: "gateway", Status: StatusFail,
			Detail: oneline(detail), Fix: gatewayFix}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Check{Name: "gateway", Status: StatusFail,
			Detail: oneline(fmt.Sprintf("GET %s: HTTP %d %s", url, resp.StatusCode, firstLine(body))),
			Fix:    gatewayFix}
	}
	var doc struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return Check{Name: "gateway", Status: StatusFail,
			Detail: oneline(fmt.Sprintf("decode model list: %v", err)), Fix: gatewayFix}
	}
	return Check{Name: "gateway", Status: StatusOK,
		Detail: fmt.Sprintf("%d models", len(doc.Data))}
}

// --- mcp -----------------------------------------------------------------

// mcpFix is the remediation line for MCP failures.
const mcpFix = "fix the mcp.json / lyon.toml mcpServers entry (command, url, env) or install the server binary"

// checkMCP merges the two config layers doctor can see — ~/.lyon/mcp.json
// beneath the lyon.toml mcpServers (same precedence the engine applies:
// mcp.json < toml < flags, and doctor has no flags) — then dials every
// server in parallel and counts its tools. Failures land in doctor's own
// ledger; pkg/mcp's Manager only records successes, so it cannot report
// what broke.
func checkMCP(ctx context.Context, opts Options) Check {
	var problems []string
	var servers []mcp.Config

	if path, err := mcp.ConfigPath(); err != nil {
		problems = append(problems, "locate mcp.json: "+err.Error())
	} else {
		fileServers, ps := mcp.LoadServers(path)
		servers = fileServers
		for _, p := range ps {
			problems = append(problems, p.Error())
		}
	}
	home := ""
	if p, err := config.HomePath(); err == nil {
		home = p
	}
	// A broken toml must not hide the mcp.json layer — its own error is
	// the config check's job; here only the servers matter.
	cfg, _ := config.Load(home, config.WorkspacePath(opts.Workspace))
	servers = mcp.MergeServers(servers, cfg.MCPServers)

	type result struct {
		name  string
		ok    bool
		err   error
		tools int
	}
	results := make([]result, len(servers))
	var wg sync.WaitGroup
	for i, s := range servers {
		wg.Add(1)
		go func(i int, s mcp.Config) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, opts.timeout(15*time.Second))
			defer cancel()
			conn, err := mcp.Connect(cctx, s)
			if err != nil {
				results[i] = result{name: s.Name, err: err}
				return
			}
			defer conn.Close()
			ts, err := conn.Tools(cctx)
			if err != nil {
				results[i] = result{name: s.Name, err: err}
				return
			}
			results[i] = result{name: s.Name, ok: true, tools: len(ts)}
		}(i, s)
	}
	wg.Wait()

	var parts []string
	var failed []string
	up, tools := 0, 0
	for _, r := range results {
		if r.ok {
			up++
			tools += r.tools
			continue
		}
		failed = append(failed, fmt.Sprintf("%s: %s", r.name, oneline(r.err.Error())))
	}
	if len(problems) > 0 {
		parts = append(parts, "config: "+strings.Join(problems, "; "))
	}
	if len(failed) > 0 {
		parts = append(parts, "failed: "+strings.Join(failed, "; "))
	}
	switch {
	case len(servers) == 0 && len(problems) == 0:
		return Check{Name: "mcp", Status: StatusOK, Detail: "no servers configured"}
	case len(failed) == 0 && len(problems) == 0:
		return Check{Name: "mcp", Status: StatusOK,
			Detail: fmt.Sprintf("%d server(s), %d tool(s)", up, tools)}
	default:
		return Check{Name: "mcp", Status: StatusFail,
			Detail: oneline(strings.Join(parts, " · ")), Fix: mcpFix}
	}
}

// --- memory --------------------------------------------------------------

// checkMemory reports the three memory scopes plus the hot tier count.
// memory.Open never fails by design, so this check cannot Fail.
func checkMemory(_ context.Context, opts Options) Check {
	st := memory.Open(opts.Workspace).StoreStats()
	return Check{Name: "memory", Status: StatusOK,
		Detail: fmt.Sprintf("team %d · personal %d · project %d · hot %d",
			st.Team, st.Personal, st.Project, st.Hot)}
}

// --- config --------------------------------------------------------------

// checkConfig surfaces config.Load's non-fatal warnings as Warn lines —
// the "unknown key" typos that silently do nothing — and a parse failure
// as Fail.
func checkConfig(_ context.Context, opts Options) Check {
	home := ""
	if p, err := config.HomePath(); err == nil {
		home = p
	}
	cfg, err := config.Load(home, config.WorkspacePath(opts.Workspace))
	if err != nil {
		return Check{Name: "config", Status: StatusFail,
			Detail: oneline(err.Error()),
			Fix:    "fix the syntax in config.toml / .lyon.toml"}
	}
	if len(cfg.Warnings) > 0 {
		return Check{Name: "config", Status: StatusWarn,
			Detail: oneline(strings.Join(cfg.Warnings, "; ")),
			Fix:    "remove or fix the flagged keys in your config files"}
	}
	return Check{Name: "config", Status: StatusOK,
		Detail: fmt.Sprintf("%d source file(s), no warnings", len(cfg.Sources))}
}

// --- workspace -----------------------------------------------------------

// checkWorkspace proves both roots the engine writes to are writable:
// the workspace itself and the store root ($LYON_HOME or ~/.lyon, created
// on demand the same way the engine creates it).
func checkWorkspace(_ context.Context, opts Options) Check {
	wsErr := probeDir(opts.Workspace, false)
	homeErr := probeDir(storeRoot(), true)
	switch {
	case wsErr == nil && homeErr == nil:
		return Check{Name: "workspace", Status: StatusOK,
			Detail: "workspace and LYON_HOME writable"}
	case wsErr != nil && homeErr != nil:
		return Check{Name: "workspace", Status: StatusFail,
			Detail: oneline(fmt.Sprintf("workspace: %v; LYON_HOME: %v", wsErr, homeErr)),
			Fix:    "check that -workspace points at an existing directory and both roots are writable"}
	case wsErr != nil:
		return Check{Name: "workspace", Status: StatusFail,
			Detail: oneline("workspace: " + wsErr.Error()),
			Fix:    "check that -workspace points at an existing writable directory"}
	default:
		return Check{Name: "workspace", Status: StatusFail,
			Detail: oneline("LYON_HOME: " + homeErr.Error()),
			Fix:    "check that $LYON_HOME is writable"}
	}
}

// probeDir writes and removes one scratch file in dir. mkdir creates a
// missing store root first — the engine would anyway.
func probeDir(dir string, mkdir bool) error {
	if strings.TrimSpace(dir) == "" {
		return errors.New("empty directory")
	}
	if mkdir {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := os.CreateTemp(dir, ".lyon-doctor-*.tmp")
	if err != nil {
		return err
	}
	name := f.Name()
	_, werr := f.WriteString("ok")
	cerr := f.Close()
	rerr := os.Remove(name)
	for _, e := range []error{werr, cerr, rerr} {
		if e != nil {
			return e
		}
	}
	return nil
}

// --- git -----------------------------------------------------------------

// checkGit runs porcelain status. A non-repository is a normal answer,
// not a failure — doctor must work in a bare folder. Anything else
// (git missing, dubious ownership, corrupt index) is a Warn: the report
// still prints, the exit code stays 0.
func checkGit(ctx context.Context, opts Options) Check {
	cmd := exec.CommandContext(ctx, "git", "-C", opts.Workspace,
		"-c", "core.quotepath=false", "status", "--porcelain=v1", "-b")
	out, err := cmd.CombinedOutput()
	if err != nil {
		text := string(out)
		switch {
		case strings.Contains(text, "not a git repository"):
			return Check{Name: "git", Status: StatusOK, Detail: "not a git repo"}
		case errors.Is(err, exec.ErrNotFound):
			return Check{Name: "git", Status: StatusWarn,
				Detail: "git not found in PATH", Fix: "install git to track workspace changes"}
		default:
			return Check{Name: "git", Status: StatusWarn,
				Detail: oneline(strings.TrimSpace(text + " " + err.Error())),
				Fix:    "run `git status` in the workspace to see the full error"}
		}
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	branch, changed := "?", len(lines)
	if len(lines) > 0 && strings.HasPrefix(lines[0], "## ") {
		b := strings.TrimPrefix(lines[0], "## ")
		if i := strings.Index(b, "..."); i >= 0 {
			b = b[:i] // drop upstream...origin/main [ahead 1]
		}
		if rest, ok := strings.CutPrefix(b, "No commits yet on "); ok {
			b = rest
		}
		if i := strings.IndexAny(b, " \t"); i >= 0 {
			b = b[:i]
		}
		branch, changed = b, len(lines)-1
	}
	if changed == 0 {
		return Check{Name: "git", Status: StatusOK,
			Detail: fmt.Sprintf("branch %s · clean", branch)}
	}
	return Check{Name: "git", Status: StatusOK,
		Detail: fmt.Sprintf("branch %s · %d changed", branch, changed)}
}

// --- sessions ------------------------------------------------------------

// checkSessions counts saved conversations for this workspace — the
// "did anything even happen here" line of a first-run report.
func checkSessions(_ context.Context, opts Options) Check {
	refs, err := agent.ListSnapshots(opts.Workspace)
	if err != nil {
		return Check{Name: "sessions", Status: StatusWarn,
			Detail: oneline(err.Error()),
			Fix:    "check that $LYON_HOME is readable"}
	}
	return Check{Name: "sessions", Status: StatusOK,
		Detail: fmt.Sprintf("%d session(s)", len(refs))}
}

// --- lyond ---------------------------------------------------------------

// checkLyond probes the local daemon's liveness endpoint. A closed port
// is the normal first-run state — OK with "not running (skip)", never a
// failure. A listener that answers but not with 200 means something is
// wrong with a daemon that should be healthy.
func checkLyond(ctx context.Context, opts Options) Check {
	to := opts.timeout(2 * time.Second)
	conn, err := net.DialTimeout("tcp", lyondAddr, to)
	if err != nil {
		return Check{Name: "lyond", Status: StatusOK, Detail: "not running (skip)"}
	}
	conn.Close()

	cctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet,
		"http://"+lyondAddr+"/healthz", nil)
	if err != nil {
		return Check{Name: "lyond", Status: StatusFail,
			Detail: oneline(err.Error()), Fix: lyondFix}
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		detail := fmt.Sprintf("GET /healthz: %v", err)
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			detail = fmt.Sprintf("GET /healthz: timeout after %s", to)
		}
		return Check{Name: "lyond", Status: StatusFail,
			Detail: oneline(detail), Fix: lyondFix}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusOK {
		return Check{Name: "lyond", Status: StatusFail,
			Detail: oneline(fmt.Sprintf("GET /healthz: HTTP %d %s", resp.StatusCode, firstLine(body))),
			Fix:    lyondFix}
	}
	var h struct {
		Sessions *int `json:"sessions"`
	}
	detail := "healthy"
	if json.Unmarshal(body, &h) == nil && h.Sessions != nil {
		detail = fmt.Sprintf("healthy · %d session(s)", *h.Sessions)
	}
	return Check{Name: "lyond", Status: StatusOK, Detail: detail}
}

// lyondFix is the remediation line for a daemon that is up but unhealthy.
const lyondFix = "check the lyond logs under $LYON_HOME/logs or restart the daemon"

// --- logs ----------------------------------------------------------------

// LogPath is today's run log: $LYON_HOME/logs/lyon-YYYY-MM-DD.log —
// the file the final report line points at.
func LogPath() string {
	return filepath.Join(storeRoot(), "logs", "lyon-"+time.Now().Format("2006-01-02")+".log")
}

// checkLogs prints today's log and audit paths with an exists/missing
// marker — the answer to "where do I look" without hunting through
// directories. Informational: always OK.
func checkLogs(_ context.Context, _ Options) Check {
	audit := filepath.Join(storeRoot(), "audit", time.Now().Format("2006-01-02")+".jsonl")
	return Check{Name: "logs", Status: StatusOK,
		Detail: fmt.Sprintf("log %s · audit %s", markExists(LogPath()), markExists(audit))}
}

// markExists appends an (exists)/(missing) marker to a path.
func markExists(p string) string {
	if _, err := os.Stat(p); err == nil {
		return p + " (exists)"
	}
	return p + " (missing)"
}

// --- evolve --------------------------------------------------------------

// checkLeaderboard summarizes the elimination ledger: how many task
// classes exist and how many seats are filled. The loader never fails
// (missing file → empty board), so this cannot Fail either.
func checkLeaderboard(_ context.Context, _ Options) Check {
	views := eval.OpenLeaderboard().Views(nil)
	onDuty := 0
	for _, v := range views {
		if v.OnDuty != "" {
			onDuty++
		}
	}
	if len(views) == 0 {
		return Check{Name: "leaderboard", Status: StatusOK, Detail: "no classes yet"}
	}
	return Check{Name: "leaderboard", Status: StatusOK,
		Detail: fmt.Sprintf("%d class(es), %d on-duty", len(views), onDuty)}
}

// checkRouting summarizes the classification routing network. Same
// never-fail contract as the leaderboard: missing file → empty table.
func checkRouting(_ context.Context, _ Options) Check {
	n := len(routing.Open().All())
	if n == 0 {
		return Check{Name: "routing", Status: StatusOK, Detail: "no routes yet"}
	}
	return Check{Name: "routing", Status: StatusOK,
		Detail: fmt.Sprintf("%d route(s)", n)}
}

// --- shared helpers ------------------------------------------------------

// storeRoot is the session/config/log root: $LYON_HOME when set
// (containers, CI, tests), else ~/.lyon — the same convention as
// pkg/logx, pkg/hooks and pkg/agent.
func storeRoot() string {
	if h := os.Getenv("LYON_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".lyon"
	}
	return filepath.Join(home, ".lyon")
}

// oneline collapses a message to a single whitespace-normalized line
// (error texts love newlines) and clips it so one check cannot swallow
// the whole report.
func oneline(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 240 {
		return string(r[:240]) + "…"
	}
	return s
}

// firstLine trims an error body to its first readable line.
func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return oneline(s)
}
