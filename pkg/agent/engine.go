package agent

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"lyon-agent/pkg/hooks"
	"lyon-agent/pkg/logx"
	"lyon-agent/pkg/openaix"
	"lyon-agent/pkg/tools"
)

// Engine is the shared, long-lived engine: one Anthropic client (reusing a
// tuned connection pool for the whole process), one frozen config, one tool
// registry. Sessions created from an Engine are independent and cheap.
type Engine struct {
	client anthropic.Client
	cfg    Config
	reg    *tools.Registry
	// hooks is the observability/control hub: audit log, statistics and
	// the security gate, shared by every session of this engine.
	hooks *hooks.Manager
	// turnSem, when non-nil, bounds in-flight runner iterations across all
	// sessions of this engine.
	turnSem chan struct{}
}

// acquireTurn takes a turn slot, blocking until one frees or ctx ends.
// Returns false when ctx cancelled while waiting.
func (e *Engine) acquireTurn(ctx context.Context) bool {
	if e.turnSem == nil {
		return true
	}
	select {
	case e.turnSem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

// releaseTurn returns a slot. No-op when the semaphore is disabled.
func (e *Engine) releaseTurn() {
	if e.turnSem != nil {
		<-e.turnSem
	}
}

// NewEngine builds an engine. A nil registry creates one pre-populated with
// the built-in workspace tools (run_shell, read_file, write_file, edit_file,
// view_image); a provided registry gets the built-ins added where missing, so
// register custom tools first and still keep the defaults.
func NewEngine(cfg Config, reg *tools.Registry) *Engine {
	cfg = cfg.withDefaults()
	if reg == nil {
		reg = tools.NewRegistry()
	}

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   128,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	// The hooks manager exists before the transport so the apiGuard below
	// can file its repair decisions into the same audit log.
	hm := hooks.New(cfg.Workspace)
	// ROADMAP 1.5: adopt the workspace's Claude Code hooks section so a
	// migrated project keeps its hooks working (best-effort mapping).
	if _, _, err := hooks.LoadClaudeSettings(hm, filepath.Join(cfg.Workspace, ".claude", "settings.json")); err != nil {
		log.Printf("hooks: %v", err)
	}
	// One tuned HTTP client for the whole process: the Anthropic API and the
	// client-side web tools share its keep-alive pool. With OpenAIBase set,
	// a translating RoundTripper fronts the pool: Messages API calls become
	// OpenAI chat/completions requests, everything else passes through.
	var rt http.RoundTripper = transport
	if cfg.OpenAIBase != "" {
		rt = &openaix.Transport{Base: cfg.OpenAIBase, Inner: transport}
	}
	// Outermost: validate every /v1/messages body before it leaves the
	// process and recover from 400s citing a malformed message — outside
	// openaix so the guard always sees the Anthropic-protocol payload.
	rt = &apiGuard{inner: rt, hooks: hm}
	httpClient := &http.Client{Transport: rt}

	builtins := []tools.Tool{
		&tools.Shell{Root: cfg.Workspace, Allow: cfg.ShellAllow},
		&tools.ReadFile{Root: cfg.Workspace},
		&tools.SearchFiles{Root: cfg.Workspace},
		&tools.WriteFile{Root: cfg.Workspace},
		&tools.EditFile{Root: cfg.Workspace},
		&tools.ViewImage{Root: cfg.Workspace},
		&tools.TodoWrite{},
	}
	if cfg.WebToolMode == "client" {
		builtins = append(builtins,
			&tools.WebSearch{Client: httpClient},
			&tools.WebFetch{Client: httpClient},
		)
	}
	for _, t := range builtins {
		if _, exists := reg.Get(t.Name()); !exists {
			_ = reg.Register(t)
		}
	}
	// Durable memory (save_memory / read_memory / forget_memory) lives at
	// <workspace>/.lyon/memory.json; registered as one set.
	if _, exists := reg.Get("save_memory"); !exists {
		_ = tools.RegisterMemory(reg, cfg.Workspace)
	}
	// run_subagent delegates to the cost-tier model; run_subagents is the
	// leader's concurrent fan-out (#53). Registered last so nestedTools
	// (built at run time) sees the final tool set for its read-only subset.
	if _, exists := reg.Get("run_subagent"); !exists {
		_ = reg.Register(newSubagentTool())
	}
	if _, exists := reg.Get("run_subagents"); !exists {
		_ = reg.Register(newDispatchTool())
	}

	// LYON_DEBUG=1 logs every API request/response to stderr — request-body
	// capture for diagnosing gateway quirks (cache, betas, rewrites).
	clientOpts := []option.RequestOption{option.WithHTTPClient(httpClient)}
	if os.Getenv("LYON_DEBUG") != "" {
		clientOpts = append(clientOpts, option.WithDebugLog(log.New(os.Stderr, "[api] ", 0)))
	}
	client := anthropic.NewClient(clientOpts...)
	e := &Engine{client: client, cfg: cfg, reg: reg, hooks: hm}
	if cfg.MaxConcurrentTurns > 0 {
		e.turnSem = make(chan struct{}, cfg.MaxConcurrentTurns)
	}
	return e
}

// Config returns the engine's effective (defaults-applied) configuration.
func (e *Engine) Config() Config { return e.cfg }

// Registry returns the engine's tool registry.
func (e *Engine) Registry() *tools.Registry { return e.reg }

// Hooks returns the engine's hook manager (audit log, statistics, security
// gate). Never nil.
func (e *Engine) Hooks() *hooks.Manager { return e.hooks }

// Close tears down engine-owned resources: the hooks writer goroutine and
// its pending audit/stats writes. Call it once when the engine is done
// (after the sessions built on it are closed) — without it every Engine
// leaks one goroutine for the life of the process. Safe on a zero or
// hand-built Engine, and idempotent.
func (e *Engine) Close() {
	if e.hooks != nil {
		e.hooks.Close()
	}
	// Run log last: the hooks close path may itself log write failures,
	// and Close both flushes the log and releases its file handle (the
	// next log call reopens lazily, so this stays idempotent).
	logx.Close()
}

// NewSession starts a session: its own event stream, its own serial-tool
// lock, its own tool runner lifecycle. The observer may be nil.
// The caller must Close the session to reap its goroutine.
func (e *Engine) NewSession(obs tools.Observer) *Session {
	return newSession(e, obs)
}

// Warmup sends one tiny request carrying the exact tools+system prefix real
// turns use, creating the prompt-cache entry so the first real prompt of the
// process starts warm. Safe to call once at startup; a failure is returned
// but never fatal — the engine works fine cold.
func (e *Engine) Warmup(ctx context.Context) error {
	apiTools := make([]anthropic.BetaToolUnionParam, 0)
	for _, t := range e.reg.Adapt(tools.AdapterConfig{}) {
		apiTools = append(apiTools, anthropic.BetaToolUnionParam{
			OfTool: &anthropic.BetaToolParam{
				Name:        t.Name(),
				Description: anthropic.String(t.Description()),
				InputSchema: t.InputSchema(),
			},
		})
	}
	params := anthropic.BetaMessageNewParams{
		Model:     e.cfg.Model,
		MaxTokens: 1,
		System: []anthropic.BetaTextBlockParam{{
			Text:         e.cfg.SystemPrompt,
			CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
		}},
		Tools:        apiTools,
		Messages:     []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("ping"))},
		OutputConfig: anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffort(e.cfg.Effort)},
		CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
	}
	_, err := e.client.Beta.Messages.New(ctx, params)
	if err != nil {
		logx.Warn("engine", "warmup request failed (engine continues cold)", "model", e.cfg.Model, "err", err)
		return fmt.Errorf("warmup: %w", err)
	}
	return nil
}
