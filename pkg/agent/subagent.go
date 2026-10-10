package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/toolrunner"

	"lyon-agent/pkg/tools"
)

// sessionCtxKey marks the context a session drives its runner with, so tool
// handlers (which receive that context) can find their session.
type sessionCtxKey struct{}

// SubagentSystemPrompt is the frozen system prompt of subagent runs. Kept
// separate from the main prompt: the subagent's cache prefix is its own and
// never contends with the main loop's.
const SubagentSystemPrompt = `You are a fast research subagent of lyon. Complete exactly the task you are given, using the read-only tools available when they help. Reply with the findings only — no preamble, no narration of your process.`

// subagentInput is the typed input of run_subagent; its jsonschema tags are
// the schema the model validates against.
type subagentInput struct {
	Task    string `json:"task" jsonschema:"clear, self-contained task for the subagent to complete"`
	Context string `json:"context,omitempty" jsonschema:"optional background or data the subagent needs"`
}

// subagentTool is run_subagent: it spins a nested tool runner on the
// configured cost-tier model and returns its findings plus an independent
// usage line.
type subagentTool struct {
	inner anthropic.BetaTool
}

// newSubagentTool builds run_subagent. Schema generation failure means the
// struct tags are malformed — a developer error, so it panics loudly.
func newSubagentTool() tools.Tool {
	desc := "Delegate a self-contained research or analysis task to a fast subagent " +
		"that has read-only tools (file reads, web search/fetch, other safe tools). " +
		"The result includes the subagent's token usage. Use it for background lookups " +
		"and fan-out questions so the main conversation stays small."
	inner, err := toolrunner.NewBetaToolFromJSONSchema[subagentInput](
		"run_subagent", desc,
		func(ctx context.Context, _ subagentInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			// Unreachable: Execute below handles the raw input itself so it
			// can reach the driving session. The runner factory requires a
			// handler; the adapter calls this only if Execute were replaced.
			return anthropic.BetaToolResultBlockParamContentUnion{}, fmt.Errorf("run_subagent: use Execute")
		},
	)
	if err != nil {
		panic(fmt.Sprintf("agent: run_subagent schema: %v", err))
	}
	return &subagentTool{inner: inner}
}

// Name implements tools.Tool.
func (t *subagentTool) Name() string { return t.inner.Name() }

// Description implements tools.Tool.
func (t *subagentTool) Description() string { return t.inner.Description() }

// InputSchema implements tools.Tool.
func (t *subagentTool) InputSchema() anthropic.BetaToolInputSchemaParam { return t.inner.InputSchema() }

// ParallelSafe is false: subagent runs are whole API loops; serializing them
// per session bounds cost and concurrency. Read-only tools still run
// alongside, since only serial tools take the session mutex.
func (t *subagentTool) ParallelSafe() bool { return false }

// Execute implements tools.Tool.
func (t *subagentTool) Execute(ctx context.Context, input json.RawMessage) ([]anthropic.BetaToolResultBlockParamContentUnion, error) {
	sess, ok := ctx.Value(sessionCtxKey{}).(*Session)
	if !ok || sess == nil {
		return nil, fmt.Errorf("run_subagent: no driving session in context")
	}
	return sess.runSubagent(ctx, input)
}

// runSubagent drives one nested tool-loop on the subagent model. Nested tool
// executions flow through this session's observer (shared ExecID sequence),
// and each nested iteration takes a turn-semaphore slot like the main loop.
func (s *Session) runSubagent(ctx context.Context, input json.RawMessage) ([]anthropic.BetaToolResultBlockParamContentUnion, error) {
	var args subagentInput
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, fmt.Errorf("run_subagent: invalid input: %w", err)
	}
	if strings.TrimSpace(args.Task) == "" {
		return nil, fmt.Errorf("run_subagent: task is required")
	}
	task := strings.TrimSpace(args.Task)
	if c := strings.TrimSpace(args.Context); c != "" {
		task += "\n\nContext:\n" + c
	}

	// The subagent gets every parallel-safe (read-only) tool of this engine
	// except run_subagent itself — no recursion, no workspace mutation.
	var subTools []anthropic.BetaTool
	for _, name := range s.eng.reg.Names() {
		if name == "run_subagent" {
			continue
		}
		tool, ok := s.eng.reg.Get(name)
		if !ok || !tool.ParallelSafe() {
			continue
		}
		subTools = append(subTools, tools.Adapt(tool, tools.AdapterConfig{
			Timeout:   s.eng.cfg.ToolTimeout,
			ResultCap: s.eng.cfg.ToolResultCap,
			Observer:  s,
			Seq:       &s.seq,
		}))
	}

	cfg := s.eng.cfg
	runner := s.eng.client.Beta.Messages.NewToolRunnerStreaming(subTools, anthropic.BetaToolRunnerParams{
		BetaMessageNewParams: anthropic.BetaMessageNewParams{
			Model:     cfg.SubagentModel,
			MaxTokens: cfg.SubagentMaxTokens,
			System: []anthropic.BetaTextBlockParam{{
				Text:         SubagentSystemPrompt,
				CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
			}},
			Messages:     []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(task))},
			OutputConfig: anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffort(cfg.SubagentEffort)},
			CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
			Betas:        []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
			Fallbacks:    anthropic.BetaFallbacksParamOfDefault(),
		},
		MaxIterations: 6,
	})

	start := time.Now()
	iters := 0
	for !runner.IsCompleted() {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !s.eng.acquireTurn(ctx) {
			return nil, ctx.Err()
		}
		var iterErr error
		got := false
		for _, err := range runner.NextStreaming(ctx) {
			if err != nil {
				iterErr = err
				break
			}
			got = true
		}
		s.eng.releaseTurn()
		if iterErr == nil {
			iterErr = runner.Err()
		}
		if iterErr != nil {
			return nil, fmt.Errorf("subagent: %w", iterErr)
		}
		if !got {
			break
		}
		iters++
	}

	var answer strings.Builder
	usage := Usage{}
	if msg := runner.LastMessage(); msg != nil {
		usage = Usage{
			Input:         msg.Usage.InputTokens,
			Output:        msg.Usage.OutputTokens,
			CacheRead:     msg.Usage.CacheReadInputTokens,
			CacheCreation: msg.Usage.CacheCreationInputTokens,
		}
		for _, block := range msg.Content {
			if block.Type == "text" && block.Text != "" {
				answer.WriteString(block.Text)
			}
		}
	}
	if answer.Len() == 0 {
		answer.WriteString("(subagent finished without a final answer)")
	}

	s.emit(Event{
		Type:       EventSubagentEnd,
		ToolName:   cfg.SubagentModel,
		Usage:      &usage,
		DurationMS: time.Since(start).Milliseconds(),
		Detail:     fmt.Sprintf("%d API calls", iters),
		TS:         time.Now(),
	})

	return tools.TextResult(fmt.Sprintf(
		"[subagent model=%s in=%d out=%d cache_read=%d]\n%s",
		cfg.SubagentModel, usage.Input, usage.Output, usage.CacheRead, answer.String(),
	)), nil
}
