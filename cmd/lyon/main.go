// Command lyon is the CLI front-end for the lyon agent engine.
//
//	lyon run  [flags] "prompt"   one-shot task (reads stdin if no prompt arg)
//	lyon chat [flags]            interactive chat TUI (-plain for a REPL)
//	lyon tools [flags]           list registered tools
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/hooks"
	"lyon-agent/pkg/mcp"
	"lyon-agent/pkg/openaix"
	"lyon-agent/pkg/tools"
)

const (
	cReset   = "\x1b[0m"
	cDim     = "\x1b[2m"
	cBold    = "\x1b[1m"
	cCyan    = "\x1b[36m"
	cGreen   = "\x1b[32m"
	cYellow  = "\x1b[33m"
	cRed     = "\x1b[31m"
	cMagenta = "\x1b[35m"
)

var color = os.Getenv("NO_COLOR") == ""

func paint(c, s string) string {
	if !color || s == "" {
		return s
	}
	return c + s + cReset
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		os.Exit(cmdRun(os.Args[2:]))
	case "chat":
		os.Exit(cmdChat(os.Args[2:]))
	case "tools":
		os.Exit(cmdTools(os.Args[2:]))
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `lyon — high-performance general-purpose agent CLI

Usage:
  lyon run  [flags] "prompt"    run one task to completion (stdin piped also works)
  lyon chat [flags]             interactive chat TUI (type / for the command
                                popup, /stats /clear /resume [N] /sessions /new
                                /help /quit, esc interrupts a running prompt;
                                -plain for a line-based REPL; -c resumes at
                                startup)
  lyon tools [flags]            list registered tools

Run 'lyon <command> -h' for command flags.
`)
}

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, " | ") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

type commonFlags struct {
	model      *string
	effort     *string
	workspace  *string
	system     *string
	maxIter    *int
	shellAllow *string
	webTools   *string
	noWebTools *bool
	approve    *bool
	mcpCmds    *multiFlag
	mcpURLs    *multiFlag
	subagent   *string
	compactTh  *float64
	ctxWindow  *int64
	maxTurns   *int
	prewarm    *bool
	openai     *bool
	openaiBase *string
}

func registerFlags(fs *flag.FlagSet) commonFlags {
	mcpCmds := &multiFlag{}
	mcpURLs := &multiFlag{}
	fs.Var(mcpCmds, "mcp", "connect a stdio MCP server: name=command args (repeatable)")
	fs.Var(mcpURLs, "mcp-http", "connect a streamable HTTP MCP server: name=url (repeatable)")
	return commonFlags{
		model:      fs.String("model", "", "model id (default claude-opus-5-5)"),
		effort:     fs.String("effort", "", "effort level: low|medium|high|xhigh|max (default medium)"),
		workspace:  fs.String("workspace", "", "workspace root for shell/file tools (default .)"),
		system:     fs.String("system", "", "path to a custom system prompt file (kept frozen)"),
		maxIter:    fs.Int("max-iter", 0, "max API calls per prompt (default 20)"),
		shellAllow: fs.String("shell-allow", "", "comma-separated shell command allowlist (e.g. echo,ls,git)"),
		webTools:   fs.String("web-tools", "", "web tool mode: server|client|off (default: auto)"),
		noWebTools: fs.Bool("no-web-tools", false, "disable web tools (shorthand for -web-tools=off)"),
		approve:    fs.Bool("approve", false, "prompt y/n before shell/write/edit tools (y/N overlay in TUI)"),
		mcpCmds:    mcpCmds,
		mcpURLs:    mcpURLs,
		subagent:   fs.String("subagent-model", "", "model for run_subagent (default LYON_SUBAGENT_MODEL/ANTHROPIC_MODEL/claude-haiku-5-5)"),
		compactTh:  fs.Float64("compact-threshold", 0, "compaction trigger as context fraction, e.g. 0.7 (negative disables; default 0.7)"),
		ctxWindow:  fs.Int64("context-window", 0, "context window size for the compaction ratio (default 200000)"),
		maxTurns:   fs.Int("max-turns", 0, "global cap on in-flight API turns (default unlimited)"),
		prewarm:    fs.Bool("prewarm", false, "warm the prompt cache before the first prompt"),
		openai:     fs.Bool("openai", false, "speak OpenAI protocol to the gateway at "+openaix.DefaultBase+" (same API key as Bearer auth)"),
		openaiBase: fs.String("openai-base", "", "OpenAI-compatible base URL, e.g. https://host/v1 (implies -openai; env LYON_OPENAI_BASE)"),
	}
}

// buildConfig resolves flags into an engine config. approvalIn is the reader
// the approval hook consumes (pass os.Stdin for run; nil disables approval).
func (f commonFlags) buildConfig() (agent.Config, error) {
	cfg := agent.DefaultConfig()
	if f.model != nil && *f.model != "" {
		cfg.Model = *f.model
	}
	if f.effort != nil && *f.effort != "" {
		cfg.Effort = *f.effort
	}
	if f.workspace != nil && *f.workspace != "" {
		cfg.Workspace = *f.workspace
	}
	if f.maxIter != nil && *f.maxIter > 0 {
		cfg.MaxIterations = *f.maxIter
	}
	if f.shellAllow != nil && *f.shellAllow != "" {
		for _, name := range strings.Split(*f.shellAllow, ",") {
			if name = strings.TrimSpace(name); name != "" {
				cfg.ShellAllow = append(cfg.ShellAllow, name)
			}
		}
	}
	if f.noWebTools != nil && *f.noWebTools {
		cfg.WebToolMode = "off"
	}
	if f.webTools != nil && *f.webTools != "" {
		switch *f.webTools {
		case "server", "client", "off":
			cfg.WebToolMode = *f.webTools
		default:
			return cfg, fmt.Errorf("-web-tools must be server, client, or off (got %q)", *f.webTools)
		}
	}
	if f.system != nil && *f.system != "" {
		data, err := os.ReadFile(*f.system)
		if err != nil {
			return cfg, fmt.Errorf("read system prompt: %w", err)
		}
		cfg.SystemPrompt = string(data)
	}
	if f.subagent != nil && *f.subagent != "" {
		cfg.SubagentModel = *f.subagent
	}
	if f.compactTh != nil && *f.compactTh != 0 {
		cfg.CompactThreshold = *f.compactTh
	}
	if f.ctxWindow != nil && *f.ctxWindow > 0 {
		cfg.ContextWindow = *f.ctxWindow
	}
	if f.maxTurns != nil && *f.maxTurns > 0 {
		cfg.MaxConcurrentTurns = *f.maxTurns
	}
	if f.openaiBase != nil && *f.openaiBase != "" {
		cfg.OpenAIBase = *f.openaiBase
	} else if f.openai != nil && *f.openai {
		cfg.OpenAIBase = openaix.DefaultBase
	}
	return cfg, nil
}

// buildEngine connects every -mcp / -mcp-http server, registers their tools
// (as mcp_<server>_<tool>) plus the built-ins, and returns a live engine.
// The caller must close the manager (defer mgr.Close()) to stop server
// subprocesses.
func (f commonFlags) buildEngine(cfg agent.Config) (*agent.Engine, *mcp.Manager, error) {
	reg := tools.NewRegistry()
	mgr := mcp.NewManager()
	if len(*f.mcpCmds) == 0 && len(*f.mcpURLs) == 0 {
		return agent.NewEngine(cfg, reg), mgr, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, spec := range *f.mcpCmds {
		name, cmd, ok := strings.Cut(spec, "=")
		if !ok || strings.TrimSpace(name) == "" || strings.TrimSpace(cmd) == "" {
			mgr.Close()
			return nil, nil, fmt.Errorf("-mcp wants name=command args (got %q)", spec)
		}
		if _, err := mgr.Connect(ctx, mcp.Config{
			Name:    strings.TrimSpace(name),
			Command: strings.Fields(cmd),
		}); err != nil {
			mgr.Close()
			return nil, nil, err
		}
	}
	for _, spec := range *f.mcpURLs {
		name, url, ok := strings.Cut(spec, "=")
		if !ok || strings.TrimSpace(name) == "" || strings.TrimSpace(url) == "" {
			mgr.Close()
			return nil, nil, fmt.Errorf("-mcp-http wants name=url (got %q)", spec)
		}
		if _, err := mgr.Connect(ctx, mcp.Config{
			Name: strings.TrimSpace(name),
			URL:  strings.TrimSpace(url),
		}); err != nil {
			mgr.Close()
			return nil, nil, err
		}
	}
	mcpTools, err := mgr.Tools(ctx)
	if err != nil {
		mgr.Close()
		return nil, nil, err
	}
	for _, t := range mcpTools {
		if err := reg.Register(t); err != nil {
			mgr.Close()
			return nil, nil, err
		}
	}
	return agent.NewEngine(cfg, reg), mgr, nil
}

// newApprovalHook returns an interactive y/n ApproveFunc serializing prompts
// (tool handlers run concurrently; two humans answers must not interleave).
// Only wired for `run`: the chat REPL owns stdin.
func newApprovalHook() tools.ApproveFunc {
	var mu sync.Mutex
	in := bufio.NewReader(os.Stdin)
	return func(_ context.Context, e tools.ExecEvent) bool {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(os.Stderr, "\n%s %s %s ? [y/N] ",
			paint(cYellow, "approve?"), e.Name, shortJSON(e.Input, 200))
		line, err := in.ReadString('\n')
		if err != nil {
			return false // stdin closed → deny (fail closed)
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return true
		default:
			return false
		}
	}
}

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cf := registerFlags(fs)
	stats := fs.Bool("stats", false, "print cumulative usage + tool metrics after the run")
	fs.Parse(args)

	cfg, err := cf.buildConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	if *cf.approve {
		cfg.Approve = newApprovalHook()
	}

	prompt := strings.Join(fs.Args(), " ")
	if strings.TrimSpace(prompt) == "" {
		data, rerr := io.ReadAll(os.Stdin)
		if rerr != nil {
			fmt.Fprintln(os.Stderr, "error: read stdin:", rerr)
			return 2
		}
		prompt = string(data)
	}
	if strings.TrimSpace(prompt) == "" {
		fmt.Fprintln(os.Stderr, "error: no prompt given")
		return 2
	}

	eng, mgr, err := cf.buildEngine(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	defer mgr.Close()
	if cf.prewarm != nil && *cf.prewarm {
		if err := eng.Warmup(context.Background()); err != nil {
			fmt.Fprintln(os.Stderr, "prewarm:", err)
		}
	}
	metrics := tools.NewMetrics()
	sess := eng.NewSession(metrics)
	defer sess.Close()

	if err := sess.Submit(prompt); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if code := drain(sess, true); code != 0 {
		if *stats {
			printStats(os.Stdout, sess.Stats(), metrics, sess.Hooks())
		}
		return code
	}
	if *stats {
		printStats(os.Stdout, sess.Stats(), metrics, sess.Hooks())
	}
	return 0
}

func cmdChat(args []string) int {
	fs := flag.NewFlagSet("chat", flag.ExitOnError)
	cf := registerFlags(fs)
	plain := fs.Bool("plain", false, "line-based REPL instead of the full-screen TUI")
	var resume bool
	fs.BoolVar(&resume, "c", false, "resume this workspace's saved conversation")
	fs.BoolVar(&resume, "continue", false, "resume this workspace's saved conversation (alias of -c)")
	fs.Parse(args)

	cfg, err := cf.buildConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	if cfg.Workspace == "" {
		// Anchor an unset workspace to the launch directory: session store,
		// memory and the header all agree on one explicit path.
		if abs, err := os.Getwd(); err == nil {
			cfg.Workspace = abs
		}
	}
	if cf.approve != nil && *cf.approve {
		// TUI mode routes approvals through the overlay (tuiApprove reads
		// the program pointer lazily); the plain REPL shares stdin with a
		// line-hook instead.
		if *plain {
			cfg.Approve = newApprovalHook()
		} else {
			cfg.Approve = tuiApprove
		}
	}

	eng, mgr, err := cf.buildEngine(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	defer mgr.Close()
	if cf.prewarm != nil && *cf.prewarm {
		if err := eng.Warmup(context.Background()); err != nil {
			fmt.Fprintln(os.Stderr, "prewarm:", err)
		}
	}
	metrics := tools.NewMetrics()
	sess := eng.NewSession(metrics)
	defer sess.Close()

	// Session resume + autosave: -c seeds the conversation from the last
	// exit; every exit (TUI quit, /quit, EOF) persists it back. Defers run
	// LIFO — save first (Interrupt + Export see a settled history), close after.
	var snap *agent.Snapshot
	if resume {
		loaded, err := agent.LoadSnapshot(cfg.Workspace)
		switch {
		case err != nil:
			fmt.Fprintln(os.Stderr, "resume:", err)
		case loaded == nil:
			fmt.Println(paint(cDim, "no saved session for this workspace — starting fresh"))
		default:
			sess.Restore(*loaded)
			snap = loaded
		}
	}
	defer saveChatSession(sess, cfg.Workspace)

	// Self-evolution: distill learnings from this conversation into the
	// memory store before the history dies with the process. Registered
	// last → runs first in the LIFO chain, while the client, hooks and
	// the TUI event pump are all still alive. 20s cap: a slow endpoint
	// must never hang exit; LYON_DISTILL=off skips it entirely.
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = sess.Distill(ctx)
	}()

	if !*plain && isTTY(os.Stdout) {
		return runTUI(cfg, sess, metrics, snap)
	}

	fmt.Println(paint(cBold, "lyon chat") + paint(cDim, fmt.Sprintf("  model=%s effort=%s workspace=%s", cfg.Model, cfg.Effort, cfg.Workspace)))
	fmt.Println(paint(cDim, "commands: /stats  /model  /resume  /sessions  /new  /memory  /remember  /quit"))
	if snap != nil {
		fmt.Println(paint(cDim, fmt.Sprintf("restored %d messages · %s", len(snap.Messages), snap.SavedAt.Format("2006-01-02 15:04"))))
	} else if hint := resumeHint(cfg.Workspace, false); hint != "" { // plain: newest
		fmt.Println(paint(cDim, hint))
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for {
		fmt.Print(paint(cGreen, "> "))
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "/") {
			head, args, _ := strings.Cut(line, " ")
			switch head {
			case "/quit", "/exit", ":q":
				return 0
			case "/stats":
				printStats(os.Stdout, sess.Stats(), metrics, sess.Hooks())
			case "/memory":
				handlePlainMemory(os.Stdout, cfg.Workspace, args)
			case "/remember":
				ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
				n, err := sess.Distill(ctx)
				cancel()
				switch {
				case err != nil:
					fmt.Println(paint(cYellow, "remember: "+err.Error()))
				case n == 0:
					fmt.Println(paint(cDim, "remember: nothing durable to add"))
				default:
					fmt.Printf("%s saved %d fact(s) — /memory to review\n",
						paint(cCyan, "🧠 memory"), n)
				}
			case "/model":
				for _, l := range modelLines(cfg, strings.TrimSpace(args)) {
					fmt.Println(l)
				}
			case "/sessions":
				listSessions(cfg.Workspace)
			case "/resume":
				idx, ok := parseResumeIndex(args)
				if !ok {
					fmt.Println(paint(cYellow, "usage: /resume [N] — /sessions lists saved sessions"))
					continue
				}
				// Settle any in-flight prompt first (idle in practice —
				// drain is synchronous — but Interrupt + drain makes
				// Restore's wait on histMu bounded either way).
				if sess.Interrupt() {
					drain(sess, false)
				}
				resumeInto(sess, cfg.Workspace, idx)
			case "/new":
				if sess.Interrupt() {
					drain(sess, false)
				}
				archived, err := sess.ArchiveIfNew()
				sess.Reset()
				switch {
				case err != nil:
					fmt.Fprintln(os.Stderr, paint(cRed, "archive: "+err.Error()))
					fmt.Println(paint(cDim, "new session — history cleared"))
				case archived:
					fmt.Println(paint(cDim, "new session — history cleared, previous conversation archived (/sessions to browse)"))
				default:
					fmt.Println(paint(cDim, "new session — history cleared"))
				}
			default:
				fmt.Println(paint(cYellow, "unknown command: "+head+" — try /help"))
			}
			continue
		}
		if err := sess.Submit(line); err != nil {
			fmt.Fprintln(os.Stderr, paint(cRed, "error: "+err.Error()))
			continue
		}
		drain(sess, false)
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

// saveChatSession persists the conversation so the next `lyon chat -c`
// resumes it. Interrupt settles an in-flight prompt; Export then waits on
// the history mutex for one stable snapshot (the TUI event pump keeps
// draining, so a finishing prompt cannot block on a full event buffer).
// Save failures are reported, never fatal — a broken ~/.lyon must not fail
// the CLI exit path.
func saveChatSession(sess *agent.Session, workspace string) {
	sess.Interrupt()
	snap := sess.Export()
	if len(snap.Messages) == 0 {
		return // nothing worth restoring
	}
	if err := agent.SaveSnapshot(snap); err != nil {
		fmt.Fprintln(os.Stderr, "save session:", err)
	}
}

func cmdTools(args []string) int {
	fs := flag.NewFlagSet("tools", flag.ExitOnError)
	cf := registerFlags(fs)
	fs.Parse(args)
	cfg, err := cf.buildConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	eng, mgr, err := cf.buildEngine(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	defer mgr.Close()
	for _, name := range eng.Registry().Names() {
		t, _ := eng.Registry().Get(name)
		parallel := "serial"
		if t.ParallelSafe() {
			parallel = "parallel-safe"
		}
		fmt.Printf("%s  [%s]\n    %s\n", paint(cCyan, name), parallel, t.Description())
	}
	if cfg.WebToolMode == "server" {
		for _, name := range agent.ServerToolNames() {
			fmt.Printf("%s  [%s]\n    %s\n", paint(cCyan, name), "server-side", "Executed by Anthropic inside the API request.")
		}
	}
	return 0
}

// parseResumeIndex turns /resume's argument into a 1-based store slot
// (empty → 1, the newest). Non-numeric or <1 input is a usage error.
func parseResumeIndex(arg string) (int, bool) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return 1, true
	}
	n, err := strconv.Atoi(arg)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// resumeHint is the startup nudge shown when a fresh session opens over an
// existing store: one session → how to continue it; several → that a store
// exists at all. picker selects the wording for the multi-session case —
// the TUI's bare /resume opens a picker, the plain REPL restores the newest.
// "" when nothing is saved.
func resumeHint(workspace string, picker bool) string {
	refs, err := agent.ListSnapshots(workspace)
	if err != nil || len(refs) == 0 {
		return ""
	}
	if len(refs) == 1 {
		return fmt.Sprintf("saved session from %s — type /resume to continue it",
			refs[0].SavedAt.Format("2006-01-02 15:04"))
	}
	if picker {
		return fmt.Sprintf("%d saved sessions — /resume to choose one, /sessions lists all", len(refs))
	}
	return fmt.Sprintf("%d saved sessions — /resume continues the newest, /sessions lists all", len(refs))
}

// listSessions prints the workspace's session store for plain mode.
func listSessions(workspace string) {
	refs, err := agent.ListSnapshots(workspace)
	switch {
	case err != nil:
		fmt.Fprintln(os.Stderr, paint(cRed, "sessions: "+err.Error()))
	case len(refs) == 0:
		fmt.Println(paint(cYellow, "no saved sessions for this workspace"))
	default:
		fmt.Println(paint(cBold, fmt.Sprintf("saved sessions (%d, newest first)", len(refs))))
		for i, r := range refs {
			title := ""
			if r.Title != "" {
				title = "  " + sanitizeText(r.Title)
			}
			fmt.Printf("  %2d  %s  %d messages%s  %s\n", i+1,
				r.SavedAt.Format("2006-01-02 15:04"), r.Messages, title,
				paint(cDim, fmt.Sprintf("— /resume %d", i+1)))
		}
		fmt.Println(paint(cDim, "/resume alone restores the newest"))
	}
}

// resumeInto restores store slot idx into sess (settling any prompt first
// is the caller's job) and reports the outcome.
func resumeInto(sess *agent.Session, workspace string, idx int) {
	refs, err := agent.ListSnapshots(workspace)
	switch {
	case err != nil:
		fmt.Fprintln(os.Stderr, paint(cRed, "resume: "+err.Error()))
	case len(refs) == 0:
		fmt.Println(paint(cYellow, "no saved session for this workspace"))
	case idx < 1 || idx > len(refs):
		fmt.Println(paint(cYellow, fmt.Sprintf("no session #%d — /sessions lists %d", idx, len(refs))))
	default:
		loaded, err := agent.LoadSnapshotFrom(refs[idx-1].Path)
		switch {
		case err != nil:
			fmt.Fprintln(os.Stderr, paint(cRed, "resume: "+err.Error()))
		case loaded == nil:
			fmt.Println(paint(cYellow, "that session is empty"))
		default:
			sess.Restore(*loaded)
			fmt.Println(paint(cDim, fmt.Sprintf("restored %d messages · %s (session #%d)",
				len(loaded.Messages), loaded.SavedAt.Format("2006-01-02 15:04"), idx)))
		}
	}
}

// drain renders events until the current prompt ends. Returns the process
// exit code (non-zero when the prompt failed).
func drain(sess *agent.Session, stopOnError bool) int {
	for ev := range sess.Events() {
		render(ev)
		if ev.Type == agent.EventPromptEnd {
			// Async intent check: refresh the session title when the
			// topic shifted; print only when it actually changes. The
			// goroutine owns no session state and outliving drain is
			// harmless (its ctx is independent).
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				if title, changed, err := sess.RefreshTitle(ctx); err == nil && changed && title != "" {
					fmt.Printf("\n%s %s\n", paint(cCyan, "🏷 title"), paint(cDim, title))
				}
			}()
			if ev.Err != "" {
				if stopOnError {
					return 1
				}
				return 0
			}
			return 0
		}
	}
	// Channel closed without prompt_end: session was closed underneath us.
	return 1
}

func render(ev agent.Event) {
	switch ev.Type {
	case agent.EventThinkingDelta:
		os.Stdout.Write([]byte(paint(cDim+cMagenta, ev.Thinking)))

	case agent.EventTextDelta:
		os.Stdout.WriteString(ev.Text)

	case agent.EventToolUseStart:
		if ev.Server {
			fmt.Printf("\n%s %s %s\n", paint(cMagenta, "🌐 server"), ev.ToolName,
				paint(cDim, shortJSON(ev.Input, 160)))
		} else {
			fmt.Printf("\n%s %s %s\n", paint(cCyan, "⚒ tool_use"), ev.ToolName, paint(cDim, ev.ToolID))
		}

	case agent.EventServerToolResult:
		marker := paint(cMagenta, "⤷ results")
		if ev.IsError {
			marker = paint(cRed, "⤷ error")
		}
		fmt.Printf("%s %s %s\n", marker, ev.ToolName, paint(cDim, shortJSON(ev.Input, 240)))

	case agent.EventToolExecStart:
		fmt.Printf("%s #%d %s %s\n",
			paint(cCyan, "▸ exec"), ev.ExecID, ev.ToolName, paint(cDim, shortJSON(ev.Input, 160)))

	case agent.EventToolExecEnd:
		if ev.IsError {
			fmt.Printf("%s #%d %s %s %s\n",
				paint(cRed, "✘ done"), ev.ExecID, ev.ToolName,
				paint(cDim, fmt.Sprintf("%dms", ev.DurationMS)), paint(cRed, ev.Err))
		} else {
			fmt.Printf("%s #%d %s %s\n",
				paint(cGreen, "✔ done"), ev.ExecID, ev.ToolName,
				paint(cDim, fmt.Sprintf("%dms", ev.DurationMS)))
		}

	case agent.EventTurnEnd:
		u := ev.Usage
		if u == nil {
			u = &agent.Usage{}
		}
		fmt.Printf("\n%s in=%d out=%d cache_read=%d cache_create=%d stop=%s %s\n",
			paint(cYellow, fmt.Sprintf("[turn %d]", ev.Turn)),
			u.Input, u.Output, u.CacheRead, u.CacheCreation,
			ev.StopReason, paint(cDim, fmt.Sprintf("%dms", ev.DurationMS)))

	case agent.EventCompaction:
		u := ev.Usage
		line := ev.Detail
		if u != nil {
			line = fmt.Sprintf("%s (in=%d out=%d)", ev.Detail, u.Input, u.Output)
		}
		fmt.Printf("\n%s %s\n", paint(cYellow, "📦 compaction"), paint(cDim, line))

	case agent.EventSubagentEnd:
		u := ev.Usage
		if u == nil {
			u = &agent.Usage{}
		}
		fmt.Printf("\n%s %s in=%d out=%d cache_read=%d %s %s\n",
			paint(cCyan, "🧠 subagent"), ev.ToolName,
			u.Input, u.Output, u.CacheRead,
			ev.Detail, paint(cDim, fmt.Sprintf("%dms", ev.DurationMS)))

	case agent.EventError:
		fmt.Printf("\n%s %s\n", paint(cRed, "error:"), ev.Err)

	case agent.EventPromptEnd:
		if ev.Err != "" {
			fmt.Printf("\n%s %s\n", paint(cRed, "✗ prompt failed"), paint(cDim, ev.Err))
		} else {
			fmt.Printf("\n%s %s\n", paint(cGreen, "✓ done"), paint(cDim, fmt.Sprintf("%d API calls", ev.Iterations)))
		}
	}
}

func shortJSON(raw json.RawMessage, max int) string {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return ""
	}
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// printStats writes the usage + tool-metric tables to w (stdout for the
// plain modes, a transcript buffer for the TUI's /stats).
func printStats(w io.Writer, turns []agent.TurnStat, metrics *tools.Metrics, hk *hooks.Manager) {
	if len(turns) == 0 {
		fmt.Fprintln(w, paint(cDim, "no turns yet"))
	} else {
		var total agent.Usage
		var wall time.Duration
		fmt.Fprintln(w, paint(cBold, "turn  stop             in    out   cache_r  cache_c   ms"))
		for _, t := range turns {
			total.Input += t.Usage.Input
			total.Output += t.Usage.Output
			total.CacheRead += t.Usage.CacheRead
			total.CacheCreation += t.Usage.CacheCreation
			wall += t.Duration
			fmt.Fprintf(w, "%4d  %-16s %5d  %5d  %7d  %7d  %4d\n",
				t.Turn, t.StopReason, t.Usage.Input, t.Usage.Output,
				t.Usage.CacheRead, t.Usage.CacheCreation, t.Duration.Milliseconds())
		}
		fmt.Fprintf(w, paint(cBold, "total")+"  in=%d out=%d cache_read=%d cache_create=%d wall=%s\n",
			total.Input, total.Output, total.CacheRead, total.CacheCreation,
			paint(cDim, wall.Round(time.Millisecond).String()))
		if total.CacheRead > 0 {
			share := float64(total.CacheRead) / float64(total.CacheRead+total.CacheCreation+total.Input) * 100
			fmt.Fprintf(w, paint(cDim, "cache share of input tokens: %.1f%%\n"), share)
		}
	}

	if metrics == nil {
		return
	}
	snap := metrics.Snapshot()
	if len(snap) == 0 {
		return
	}
	fmt.Fprintln(w, paint(cBold, "tool              calls  errors    avg      p50      p95     max"))
	for _, m := range snap {
		fmt.Fprintf(w, "%-16s %5d  %6d  %6dms  %6dms  %6dms  %6dms\n",
			m.Name, m.Calls, m.Errors,
			m.Avg().Milliseconds(), m.P50.Milliseconds(),
			m.P95.Milliseconds(), m.Max.Milliseconds())
	}
	printHookStats(w, hk)
}

// printHookStats appends the persistent cross-run aggregates (audit +
// stats layer): unlike the per-session tables above, these survive restarts
// via $LYON_HOME/stats.json and cover approvals, security and hooks.
func printHookStats(w io.Writer, hk *hooks.Manager) {
	if hk == nil {
		return
	}
	s := hk.Stats()
	fmt.Fprintln(w, paint(cBold, "totals (persistent across runs)"))
	fmt.Fprintf(w, "  prompts %d · api calls %d · compactions %d · subagents %d\n",
		s.Prompts, s.ApiCalls, s.Compactions, s.Subagents)
	fmt.Fprintf(w, "  approvals %d asked / %d yes / %d no · security blocks %d · hook failures %d\n",
		s.ApprovalsReq, s.ApprovalsYes, s.ApprovalsNo, s.SecurityBlocks, s.HookFailures)
	fmt.Fprintf(w, "  tokens in=%d out=%d cache_read=%d cache_create=%d\n",
		s.Tokens.Input, s.Tokens.Output, s.Tokens.CacheRead, s.Tokens.CacheCreation)
	if !s.StartedAt.IsZero() {
		fmt.Fprintln(w, paint(cDim, "  tracked since "+s.StartedAt.Format("2006-01-02 15:04")))
	}
}
