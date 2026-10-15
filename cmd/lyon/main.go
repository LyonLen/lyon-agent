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
	"lyon-agent/pkg/config"
	"lyon-agent/pkg/hooks"
	"lyon-agent/pkg/market"
	"lyon-agent/pkg/mcp"
	"lyon-agent/pkg/memory"
	"lyon-agent/pkg/openaix"
	"lyon-agent/pkg/routing"
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
	cBlue    = "\x1b[34m"
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
		fmt.Fprintf(os.Stderr, tr("unknown command %q\n\n", "未知命令 %q\n\n"), os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, tr(`lyon — high-performance general-purpose agent CLI

Usage:
  lyon run  [flags] "prompt"    run one task to completion (stdin piped also works)
  lyon chat [flags]             interactive chat TUI (type / for the command
                                popup, /stats /clear /resume [N] /sessions /new
                                /help /quit, ctrl+c interrupts a running prompt; esc closes output/overlays;
                                -plain for a line-based REPL; -c resumes at
                                startup)
  lyon tools [flags]            list registered tools

Run 'lyon <command> -h' for command flags.
`, `lyon — 高性能通用 AI Agent 命令行

用法:
  lyon run  [flags] "prompt"    跑完一个任务（也支持管道 stdin）
  lyon chat [flags]             交互式聊天 TUI（输入 / 唤出命令弹窗，
                                /stats /clear /resume [N] /sessions /new
                                /help /quit，ctrl+c 中断进行中的提问；esc 关闭 output/弹层；
                                -plain 切到行式 REPL；-c 启动时续上
                                上次会话）
  lyon tools [flags]            列出已注册工具

运行 'lyon <command> -h' 查看该命令的旗标。
`))
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
	// conf holds the merged lyon.toml (home + workspace layers, explicit
	// flags applied) once loadFileConfig ran; nil for flag-only callers.
	conf *config.Config
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
		maxIter:    fs.Int("max-iter", 0, "max API calls per prompt (default unlimited, env LYON_MAX_ITER)"),
		shellAllow: fs.String("shell-allow", "", "comma-separated shell command allowlist (e.g. echo,ls,git)"),
		webTools:   fs.String("web-tools", "", "web tool mode: server|client|off (default: auto)"),
		noWebTools: fs.Bool("no-web-tools", false, "disable web tools (shorthand for -web-tools=off)"),
		approve:    fs.Bool("approve", false, "prompt y/n before shell/write/edit tools (chat asks on a TTY by default; -approve=false disables; run asks only with -approve)"),
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

// loadFileConfig merges the two lyon.toml layers — $LYON_HOME/config.toml
// (else ~/.lyon/config.toml) beneath <workspace>/.lyon.toml, the workspace
// file found via the -workspace flag or the launch directory — then layers
// every explicitly-set flag on top (config.ApplyFlags, Visit-driven) and
// pushes the winners back into the flag variables buildConfig already
// reads. The effective precedence becomes flag > workspace toml > home
// toml > built-in default. With no config file on disk every pushed value
// is the flag's own zero default, so startup is unchanged; a broken file
// warns on stderr and degrades — never fatal (the mcp.json contract).
func (f *commonFlags) loadFileConfig(fs *flag.FlagSet) {
	ws := "."
	if f.workspace != nil && *f.workspace != "" {
		ws = *f.workspace
	}
	home := ""
	if p, err := config.HomePath(); err == nil {
		home = p
	} else {
		fmt.Fprintln(os.Stderr, "config: locate home config:", err)
	}
	c, err := config.Load(home, config.WorkspacePath(ws))
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
	}
	for _, w := range c.Warnings {
		fmt.Fprintln(os.Stderr, "config:", w)
	}
	explicit := map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { explicit[fl.Name] = true })
	config.ApplyFlags(&c, fs, func(name string) bool { return explicit[name] })
	f.applyFileConfig(c)
	f.conf = &c
}

// applyFileConfig writes the merged config into the flag variables
// buildConfig reads. c already carries every explicit flag override
// (ApplyFlags ran first) and an absent config layer leaves the zero values
// these flags start from — so the unconditional writes preserve both
// flag-first precedence and the no-config zero behavior.
func (f commonFlags) applyFileConfig(c config.Config) {
	set := func(p *string, v string) {
		if p != nil {
			*p = v
		}
	}
	set(f.model, c.Model)
	set(f.effort, c.Effort)
	set(f.workspace, c.Workspace)
	set(f.system, c.System)
	set(f.subagent, c.SubagentModel)
	set(f.openaiBase, c.OpenAIBase)
	if f.shellAllow != nil {
		*f.shellAllow = strings.Join(c.ShellAllowlist, ",")
	}
	if f.maxIter != nil {
		*f.maxIter = c.MaxIter
	}
	if f.compactTh != nil {
		*f.compactTh = c.CompactThreshold
	}
	if f.ctxWindow != nil {
		*f.ctxWindow = c.ContextWindow
	}
	if f.maxTurns != nil {
		*f.maxTurns = c.MaxTurns
	}
	if f.prewarm != nil {
		*f.prewarm = c.Prewarm
	}
	if f.approve != nil {
		switch c.ApprovalMode {
		case config.ApprovalAsk:
			*f.approve = true
		case config.ApprovalNever:
			*f.approve = false
		}
	}
	// provider = "openai" implies the default OpenAI base; an explicit
	// openai_base (file or -openai-base) is consumed by buildConfig first.
	if f.openai != nil && c.Provider == "openai" {
		*f.openai = true
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

// buildEngine connects every MCP server — the ~/.lyon/mcp.json config file
// (tolerant: bad entries warn and skip) plus each -mcp / -mcp-http flag
// (fatal, flags override same-name file entries), registers their tools
// (as mcp_<server>_<tool>) plus the built-ins, and returns a live engine.
// The caller must close both: defer mgr.Close() stops server subprocesses,
// defer eng.Close() retires the hooks writer goroutine.
func (f commonFlags) buildEngine(cfg agent.Config) (*agent.Engine, *mcp.Manager, error) {
	// Leader dispatch routes each subtask by class evidence (#53): the
	// routing table's confident recommendation when one exists, else ""
	// and the subagent tier runs it. routingSwap with current "" can only
	// return a model when samples/confidence gates are truly met. Wired
	// here so CLI and TUI engines share the same dispatch policy.
	cfg.RouteFor = func(task string) string {
		if m, _, ok := routingSwap("", task); ok {
			return m
		}
		return ""
	}
	reg := tools.NewRegistry()
	mgr := mcp.NewManager()
	// Parse -mcp/-mcp-http into configs up front: flags form the override
	// layer above ~/.lyon/mcp.json (E1) and keep their fatal contract — a
	// bad spec aborts startup exactly as before.
	var flagServers []mcp.Config
	for _, spec := range *f.mcpCmds {
		name, cmd, ok := strings.Cut(spec, "=")
		if !ok || strings.TrimSpace(name) == "" || strings.TrimSpace(cmd) == "" {
			mgr.Close()
			return nil, nil, fmt.Errorf("-mcp wants name=command args (got %q)", spec)
		}
		flagServers = append(flagServers, mcp.Config{
			Name:    strings.TrimSpace(name),
			Command: strings.Fields(cmd),
		})
	}
	for _, spec := range *f.mcpURLs {
		name, url, ok := strings.Cut(spec, "=")
		if !ok || strings.TrimSpace(name) == "" || strings.TrimSpace(url) == "" {
			mgr.Close()
			return nil, nil, fmt.Errorf("-mcp-http wants name=url (got %q)", spec)
		}
		flagServers = append(flagServers, mcp.Config{
			Name: strings.TrimSpace(name),
			URL:  strings.TrimSpace(url),
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Three MCP layers, weakest first: ~/.lyon/mcp.json < lyon.toml
	// mcpServers < -mcp/-mcp-http flags, same-name override at each step.
	// MergeServers stacks toml under flags, and ConnectFileServers then
	// stacks mcp.json under that whole layer — one expression carries the
	// full precedence.
	var tomlServers []mcp.Config
	if f.conf != nil {
		tomlServers = f.conf.MCPServers
	}
	layer := mcp.MergeServers(tomlServers, flagServers)

	warn := func(err error) { fmt.Fprintln(os.Stderr, "mcp:", err) }

	// E1: ~/.lyon/mcp.json loads beneath the layer — a bad entry or a dead
	// file server warns on stderr and is skipped, never fatal.
	mcp.ConnectFileServers(ctx, mgr, reg, layer, warn)

	// lyon.toml survivors (entries no flag overrides) connect here with
	// the same tolerant contract as mcp.json: a bad or dead configured
	// server warns and skips, it never blocks startup. Only an explicit
	// -mcp / -mcp-http keeps its fatal contract — the loop below.
	for _, s := range layer[:len(layer)-len(flagServers)] {
		conn, err := mgr.Connect(ctx, s)
		if err != nil {
			warn(err)
			continue
		}
		mcpTools, err := conn.Tools(ctx)
		if err != nil {
			warn(err)
			continue
		}
		for _, t := range mcpTools {
			if err := reg.Register(t); err != nil {
				warn(err)
			}
		}
	}

	if len(flagServers) == 0 && len(mgr.Names()) == 0 {
		return agent.NewEngine(cfg, reg), mgr, nil
	}
	for _, s := range flagServers {
		conn, err := mgr.Connect(ctx, s)
		if err != nil {
			mgr.Close()
			return nil, nil, err
		}
		mcpTools, err := conn.Tools(ctx)
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
	}
	return agent.NewEngine(cfg, reg), mgr, nil
}

// chatApproveOn decides whether chat injects the y/n approver. The mode
// table promises default = "ask a human" (bypass exists precisely to skip
// that ask), so an interactive TTY gets approvals unless -approve=false
// explicitly opts out; a piped stdin stays silent so scripts can never
// block on a prompt nobody will answer. Explicit -approve always wins.
func chatApproveOn(set bool, v *bool, stdinTTY bool) bool {
	if set && v != nil {
		return *v
	}
	return stdinTTY
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
	flowSlug := fs.String("flow", "", "run a frozen workflow instead of a free-form prompt (slug; lyon chat '/flow list' shows them)")
	coverage := fs.Bool("coverage", false, "knowledge-coverage pre-flight on the prompt: inject gaps when the blind quiz misses (design §2)")
	examClass := fs.String("exam", "", "run a closed-book exam first: class name or 'auto' (classified from the prompt) — §1.4")
	fs.Parse(args)
	cf.loadFileConfig(fs)

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

	// Self-evolution closes the loop here (design §5): an unpinned run
	// works on whoever the routing network recommends for this task's
	// class — exams and scores change who works next. An explicit
	// -model always wins.
	if *cf.model == "" {
		if model, e, ok := routingSwap(cfg.Model, prompt); ok {
			fmt.Printf("%s [%s] %s (%d samples, confidence %.2f)\n",
				paint(cCyan, "⇄ routing"), e.Class, model, e.Samples, e.Confidence)
			cfg.Model = model
		}
	}

	eng, mgr, err := cf.buildEngine(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	defer mgr.Close()
	defer eng.Close()
	if cf.prewarm != nil && *cf.prewarm {
		if err := eng.Warmup(context.Background()); err != nil {
			fmt.Fprintln(os.Stderr, "prewarm:", err)
		}
	}
	metrics := tools.NewMetrics()
	sess := eng.NewSession(metrics)
	defer sess.Close()
	defer memory.Open(cfg.Workspace).Flush()

	// §1.4 exam first — the score files either way; a miss warns and the
	// run proceeds (the exam grades the agent, not this prompt).
	if *examClass != "" {
		class := *examClass
		if class == "auto" {
			class = string(routing.Classify(prompt))
		}
		switch runExamCLI(sess, cfg.Model, class) {
		case 2:
			return 2
		case 1:
			fmt.Println(paint(cYellow, "  exam failed — run proceeds, the miss is on record"))
		}
	}

	// §3 frozen workflow: the state machine walks the nodes, the result
	// scores into the leaderboard and the routing table.
	if *flowSlug != "" {
		code := runFlowCLI(sess, cfg, *flowSlug, prompt)
		if *stats {
			printStats(os.Stdout, sess.Stats(), metrics, sess.Hooks(),
				newGauge(cfg, sess.Stats(), sess.SubUsage()))
		}
		return code
	}

	// §2 coverage pre-flight for a plain prompt.
	if *coverage {
		prompt = gatePromptCLI(sess, prompt)
	}

	if err := sess.Submit(prompt); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	ev, code := drain(sess, true)
	fileMarket(prompt, cfg.Model, ev)
	if code != 0 {
		if *stats {
			printStats(os.Stdout, sess.Stats(), metrics, sess.Hooks(),
				newGauge(cfg, sess.Stats(), sess.SubUsage()))
		}
		return code
	}
	// §1.2 production score: file the blind-judged card + tick the
	// exam clock, exactly like a successful prompt in the TUI.
	scorePlainCLI(sess, cfg.Model, prompt)
	if *stats {
		printStats(os.Stdout, sess.Stats(), metrics, sess.Hooks(),
			newGauge(cfg, sess.Stats(), sess.SubUsage()))
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
	cf.loadFileConfig(fs)
	approveSet := false
	fs.Visit(func(f *flag.Flag) { // did the user actually type -approve?
		if f.Name == "approve" {
			approveSet = true
		}
	})
	if cf.conf != nil && cf.conf.ApprovalExplicit() {
		// lyon.toml decided the approval mode (ask/never) — that counts as
		// explicit, so chat skips the TTY-default guess.
		approveSet = true
	}

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
	if chatApproveOn(approveSet, cf.approve, isTTY(os.Stdin)) {
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
	defer eng.Close()
	if cf.prewarm != nil && *cf.prewarm {
		if err := eng.Warmup(context.Background()); err != nil {
			fmt.Fprintln(os.Stderr, "prewarm:", err)
		}
	}
	metrics := tools.NewMetrics()
	sess := eng.NewSession(metrics)
	defer sess.Close()
	defer memory.Open(cfg.Workspace).Flush()

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
			fmt.Println(paint(cDim, tr("no saved session for this workspace — starting fresh",
				"本工作区没有已保存的会话 — 从头开始")))
		default:
			sess.Restore(*loaded)
			snap = loaded
		}
	}
	// Self-evolution: distill learnings from this conversation into the
	// memory store before the history dies with the process. LYON_DISTILL=off
	// skips it entirely.
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = sess.Distill(ctx)
	}()

	// Registered after the distill defer → runs FIRST (LIFO): the
	// conversation must hit disk before the optional distill pass, so a
	// user who kills a slow exit still resumes. Export/Save are pure
	// local I/O — they don't need the client the distill call below uses.
	defer saveChatSession(sess, cfg.Workspace)

	if !*plain && isTTY(os.Stdout) {
		return runTUI(cfg, sess, metrics, snap)
	}

	fmt.Println(paint(cBold, "lyon chat") + paint(cDim, fmt.Sprintf("  model=%s effort=%s workspace=%s", cfg.Model, cfg.Effort, cfg.Workspace)))
	fmt.Println(paint(cDim, tr("commands: /stats  /model  /mode  /resume  /sessions  /new  /memory  /remember  /lang  /quit",
		"命令: /stats  /model  /mode  /resume  /sessions  /new  /memory  /remember  /lang  /quit")))
	if snap != nil {
		fmt.Println(paint(cDim, tr(fmt.Sprintf("restored %d messages · %s", len(snap.Messages), snap.SavedAt.Format("2006-01-02 15:04")),
			fmt.Sprintf("已恢复 %d 条消息 · %s", len(snap.Messages), snap.SavedAt.Format("2006-01-02 15:04")))))
	} else if hint := resumeHint(cfg.Workspace, false); hint != "" { // plain: newest
		fmt.Println(paint(cDim, hint))
	}
	if line := newGauge(cfg, sess.Stats(), sess.SubUsage()).detailLine(); line != "" {
		fmt.Println(paint(cDim, "── ") + line) // resumed session keeps its gauge
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
				printStats(os.Stdout, sess.Stats(), metrics, sess.Hooks(),
					newGauge(cfg, sess.Stats(), sess.SubUsage()))
			case "/memory":
				handlePlainMemory(os.Stdout, cfg.Workspace, args)
			case "/remember":
				ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
				n, err := sess.Distill(ctx)
				cancel()
				switch {
				case err != nil:
					fmt.Println(paint(cYellow, tr("remember: ", "记忆失败: ")+err.Error()))
				case n == 0:
					fmt.Println(paint(cDim, tr("remember: nothing durable to add",
						"remember: 没有值得留存的收获")))
				default:
					fmt.Printf("%s\n",
						tr(fmt.Sprintf("%s saved %d fact(s) — /memory to review",
							paint(cCyan, "🧠 memory"), n),
							fmt.Sprintf("%s 已存 %d 条事实 — /memory 查看",
								paint(cCyan, "🧠 memory"), n)))
				}
			case "/model":
				for _, l := range modelLines(cfg, strings.TrimSpace(args)) {
					fmt.Println(l)
				}
			case "/mode":
				fmt.Println(modeAction(sess, args))
			case "/paste":
				// Plain-REPL twin of alt+v: same clipboard read, only the
				// delivery differs — here the token is printed to be typed
				// into the next prompt (there is no input line to fill).
				p := readPaste(cfg.Workspace)
				switch {
				case p.err != nil:
					fmt.Fprintln(os.Stderr, paint(cYellow, tr("paste: ", "粘贴失败: ")+p.err.Error()))
				case p.rel == "":
					fmt.Fprintln(os.Stderr, paint(cYellow, tr("paste: nothing to attach", "粘贴: 没有可附的内容")))
				case p.w > 0 && p.h > 0:
					fmt.Printf("%s\n",
						tr(fmt.Sprintf("%s attached @%s (%dx%d) — include it in your next prompt",
							paint(cCyan, "paste"), p.rel, p.w, p.h),
							fmt.Sprintf("%s 已附上 @%s（%dx%d）— 记得放进下一条提问",
								paint(cCyan, "paste"), p.rel, p.w, p.h)))
				default:
					fmt.Printf("%s\n",
						tr(fmt.Sprintf("%s attached @%s — include it in your next prompt",
							paint(cCyan, "paste"), p.rel),
							fmt.Sprintf("%s 已附上 @%s — 记得放进下一条提问",
								paint(cCyan, "paste"), p.rel)))
				}
			case "/sessions":
				listSessions(cfg.Workspace)
			case "/lang":
				if a := strings.TrimSpace(args); a == "" {
					fmt.Println(langLine())
				} else if setLang(a) {
					fmt.Println(langLine())
				} else {
					fmt.Println(paint(cYellow, tr("usage: /lang zh|en", "用法: /lang zh|en")))
				}
			case "/resume":
				idx, ok := parseResumeIndex(args)
				if !ok {
					fmt.Println(paint(cYellow, tr("usage: /resume [N] — /sessions lists saved sessions",
						"用法: /resume [N] — /sessions 列出已保存会话")))
					continue
				}
				// Settle any in-flight prompt first (idle in practice —
				// drain is synchronous — but Interrupt + drain makes
				// Restore's wait on histMu bounded either way).
				if sess.Interrupt() {
					_, _ = drain(sess, false)
				}
				resumeInto(sess, cfg.Workspace, idx)
			case "/new":
				if sess.Interrupt() {
					_, _ = drain(sess, false)
				}
				archived, err := sess.ArchiveIfNew()
				sess.Reset()
				switch {
				case err != nil:
					fmt.Fprintln(os.Stderr, paint(cRed, tr("archive: ", "归档失败: ")+err.Error()))
					fmt.Println(paint(cDim, tr("new session — history cleared", "新会话 — 历史已清空")))
				case archived:
					fmt.Println(paint(cDim, tr("new session — history cleared, previous conversation archived (/sessions to browse)",
						"新会话 — 历史已清空，上一段对话已归档（/sessions 浏览）")))
				default:
					fmt.Println(paint(cDim, tr("new session — history cleared", "新会话 — 历史已清空")))
				}
			default:
				fmt.Println(paint(cYellow, tr("unknown command: "+head+" — try /help",
					"未知命令: "+head+" — 试试 /help")))
			}
			continue
		}
		if err := sess.Submit(line); err != nil {
			fmt.Fprintln(os.Stderr, paint(cRed, tr("error: ", "错误: ")+err.Error()))
			continue
		}
		ev, _ := drain(sess, false)
		fileMarket(line, cfg.Model, ev)
		// Resident readout after every prompt — the REPL twin of the
		// TUI header gauge.
		if line := newGauge(cfg, sess.Stats(), sess.SubUsage()).detailLine(); line != "" {
			fmt.Println(paint(cDim, "── ") + line)
		}
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
	cf.loadFileConfig(fs)
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
	defer eng.Close()
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
		return tr(fmt.Sprintf("saved session from %s — type /resume to continue it",
			refs[0].SavedAt.Format("2006-01-02 15:04")),
			fmt.Sprintf("有 %s 的已保存会话 — 输入 /resume 继续",
				refs[0].SavedAt.Format("2006-01-02 15:04")))
	}
	if picker {
		return tr(fmt.Sprintf("%d saved sessions — /resume to choose one, /sessions lists all", len(refs)),
			fmt.Sprintf("%d 个已保存会话 — /resume 选择一个，/sessions 全部列出", len(refs)))
	}
	return tr(fmt.Sprintf("%d saved sessions — /resume continues the newest, /sessions lists all", len(refs)),
		fmt.Sprintf("%d 个已保存会话 — /resume 续上最新一条，/sessions 全部列出", len(refs)))
}

// listSessions prints the workspace's session store for plain mode.
func listSessions(workspace string) {
	refs, err := agent.ListSnapshots(workspace)
	switch {
	case err != nil:
		fmt.Fprintln(os.Stderr, paint(cRed, tr("sessions: ", "会话列表失败: ")+err.Error()))
	case len(refs) == 0:
		fmt.Println(paint(cYellow, tr("no saved sessions for this workspace",
			"本工作区没有已保存的会话")))
	default:
		fmt.Println(paint(cBold, tr(fmt.Sprintf("saved sessions (%d, newest first)", len(refs)),
			fmt.Sprintf("已保存会话（%d 个，最新在前）", len(refs)))))
		for i, r := range refs {
			title := ""
			if r.Title != "" {
				title = "  " + sanitizeText(r.Title)
			}
			fmt.Printf(tr("  %2d  %s  %d messages%s  %s\n",
				"  %2d  %s  %d 条消息%s  %s\n"), i+1,
				r.SavedAt.Format("2006-01-02 15:04"), r.Messages, title,
				paint(cDim, fmt.Sprintf("— /resume %d", i+1)))
		}
		fmt.Println(paint(cDim, tr("/resume alone restores the newest",
			"裸 /resume 恢复最新一条")))
	}
}

// resumeInto restores store slot idx into sess (settling any prompt first
// is the caller's job) and reports the outcome.
func resumeInto(sess *agent.Session, workspace string, idx int) {
	refs, err := agent.ListSnapshots(workspace)
	switch {
	case err != nil:
		fmt.Fprintln(os.Stderr, paint(cRed, tr("resume: ", "恢复失败: ")+err.Error()))
	case len(refs) == 0:
		fmt.Println(paint(cYellow, tr("no saved session for this workspace",
			"本工作区没有已保存的会话")))
	case idx < 1 || idx > len(refs):
		fmt.Println(paint(cYellow, tr(
			fmt.Sprintf("no session #%d — /sessions lists %d", idx, len(refs)),
			fmt.Sprintf("没有 #%d 号会话 — /sessions 列出 %d 个", idx, len(refs)))))
	default:
		loaded, err := agent.LoadSnapshotFrom(refs[idx-1].Path)
		switch {
		case err != nil:
			fmt.Fprintln(os.Stderr, paint(cRed, tr("resume: ", "恢复失败: ")+err.Error()))
		case loaded == nil:
			fmt.Println(paint(cYellow, tr("that session is empty", "那个会话是空的")))
		default:
			sess.Restore(*loaded)
			fmt.Println(paint(cDim, tr(
				fmt.Sprintf("restored %d messages · %s (session #%d)",
					len(loaded.Messages), loaded.SavedAt.Format("2006-01-02 15:04"), idx),
				fmt.Sprintf("已恢复 %d 条消息 · %s（会话 #%d）",
					len(loaded.Messages), loaded.SavedAt.Format("2006-01-02 15:04"), idx))))
		}
	}
}

// drain renders events until the current prompt ends. Returns the final
// prompt_end event (nil when the stream died first) and the process exit
// code (non-zero when the prompt failed).
func drain(sess *agent.Session, stopOnError bool) (*agent.Event, int) {
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
			out := ev // copy: range variable address must be stable
			if ev.Err != "" {
				return &out, bool2int(stopOnError)
			}
			return &out, 0
		}
	}
	// Channel closed without prompt_end: session was closed underneath us.
	return nil, 1
}

func bool2int(b bool) int {
	if b {
		return 1
	}
	return 0
}

// fileMarket files one CLI run's wall time into the task hour ledger —
// the REPL/one-shot twin of the TUI's prompt_end booking (#54).
func fileMarket(prompt, model string, ev *agent.Event) {
	if ev == nil || ev.DurationMS <= 0 || prompt == "" {
		return
	}
	market.Open().Record(market.Record{
		Class:  string(routing.Classify(prompt)),
		Model:  model,
		Prompt: prompt,
		DurMS:  ev.DurationMS,
		Iters:  ev.Iterations,
		Err:    ev.Err,
		TS:     time.Now(),
	})
}

func render(ev agent.Event) {
	switch ev.Type {
	case agent.EventThinkingDelta:
		os.Stdout.Write([]byte(paint(cDim+cMagenta, ev.Thinking)))

	case agent.EventTextDelta:
		os.Stdout.WriteString(ev.Text)

	case agent.EventToolUseStart:
		if ev.Server {
			fmt.Printf("\n%s%s %s %s\n", indTool, paint(cMagenta, "🌐 server"), ev.ToolName,
				paint(cDim, truncateRunes(stepArgPreview(ev.ToolName, ev.Input), 160)))
		} else {
			// Compact intent row: no API call id (noise), and the live
			// event carries no input — the exec row shows the arguments.
			fmt.Printf("\n%s%s\n", indTool, paint(cCyan, ev.ToolName))
		}

	case agent.EventServerToolResult:
		marker := paint(cMagenta, "⤷ results")
		if ev.IsError {
			marker = paint(cRed, "⤷ error")
		}
		fmt.Printf("%s%s %s %s\n", indExec, marker, ev.ToolName, paint(cDim, shortJSON(ev.Input, 240)))

	case agent.EventToolExecStart:
		// todo_write prints the plan itself (#66) — the readable block
		// replaces the exec row, same as the TUI.
		if ev.ToolName == "todo_write" && !ev.Sub {
			if items := parseTodos(ev.Input); items != nil {
				for _, r := range todoRows(items) {
					fmt.Printf("%s%s\n", indTool, r)
				}
			}
			break
		}
		ind := indExec
		if ev.Sub {
			ind = indSub
		}
		fmt.Printf("%s%s #%d %s %s\n", ind,
			paint(cCyan, "exec"), ev.ExecID, ev.ToolName,
			paint(cDim, truncateRunes(stepArgPreview(ev.ToolName, ev.Input), 160)))

	case agent.EventToolExecEnd:
		if ev.ToolName == "todo_write" && !ev.Sub {
			if ev.IsError {
				fmt.Printf("%s%s %s\n", indExec,
					paint(cRed, "todo_write failed"), paint(cRed, ev.Err))
			}
			break
		}
		ind := indExec
		if ev.Sub {
			ind = indSub
		}
		if ev.IsError {
			fmt.Printf("%s%s #%d %s %s\n", ind,
				paint(cRed, "failed"), ev.ExecID,
				paint(cDim, fmt.Sprintf("%dms", ev.DurationMS)), paint(cRed, ev.Err))
		} else {
			fmt.Printf("%s%s #%d %s\n", ind,
				paint(cGreen, "done"), ev.ExecID,
				paint(cDim, fmt.Sprintf("%dms", ev.DurationMS)))
		}

	case agent.EventTurnEnd:
		// The [turn N] accounting row is gone (usage lives in /stats and
		// the TUI gauge); keep the bare newline so the next prompt or
		// event line starts at column 0 after raw text deltas.
		fmt.Print("\n")

	case agent.EventMemoryInjected:
		fmt.Printf("\n%s %s\n", paint(cYellow, "🧠 memory"), paint(cDim, ev.Detail))

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
		dur := ""
		if ev.DurationMS > 0 {
			dur = " · " + fmtDur(ev.DurationMS)
		}
		switch {
		case ev.Err != "":
			fmt.Printf("\n%s %s\n", paint(cRed, "✗ prompt failed"), paint(cDim, ev.Err))
		case ev.StopReason == "max_iterations":
			fmt.Printf("\n%s %s\n", paint(cYellow, "⚡ budget reached"),
				paint(cDim, fmt.Sprintf("%d API calls%s — run again to continue · raise with -max-iter / LYON_MAX_ITER", ev.Iterations, dur)))
		default:
			fmt.Printf("\n%s %s\n", paint(cGreen, "✓ done"), paint(cDim, fmt.Sprintf("%d API calls%s", ev.Iterations, dur)))
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
func printStats(w io.Writer, turns []agent.TurnStat, metrics *tools.Metrics, hk *hooks.Manager, g usageGauge) {
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
	// Resident readout: window position + cumulative cost (参考价).
	if line := g.detailLine(); line != "" {
		fmt.Fprintln(w, line)
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
