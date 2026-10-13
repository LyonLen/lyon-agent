package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
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

	cfg := s.eng.cfg
	answer, usage, _, err := s.runNested(ctx, cfg.SubagentModel, task, "", "")
	if err != nil {
		return nil, err
	}
	return tools.TextResult(fmt.Sprintf(
		"[subagent model=%s in=%d out=%d cache_read=%d]\n%s",
		cfg.SubagentModel, usage.Input, usage.Output, usage.CacheRead, answer,
	)), nil
}

// nestedTools adapts this engine's parallel-safe (read-only) tools for one
// nested run: everything except the subagent tools themselves — no
// recursion, no workspace mutation. obs is the run's attribution channel:
// the session itself for the legacy single call, a per-task subObs stamp
// inside a run_subagents fan-out (#53).
func (s *Session) nestedTools(obs tools.Observer) []anthropic.BetaTool {
	var subTools []anthropic.BetaTool
	for _, name := range s.eng.reg.Names() {
		if name == "run_subagent" || name == "run_subagents" {
			continue
		}
		tool, ok := s.eng.reg.Get(name)
		if !ok || !tool.ParallelSafe() {
			continue
		}
		subTools = append(subTools, tools.Adapt(tool, tools.AdapterConfig{
			Timeout:   s.eng.cfg.ToolTimeout,
			ResultCap: s.eng.cfg.ToolResultCap,
			Observer:  obs,
			PreTool:   s.preTool, // subagent tools pass the same security gate
			Seq:       &s.seq,
		}))
	}
	return subTools
}

// subObs attributes one dispatched subtask's inner executions to ITS task
// node (#53): concurrent fan-out runs all stamp Event.Sub, and the SubID
// token is what keeps their step rows from collapsing into a single
// "active subagent". It bypasses takeToolUse — inner executions never
// claim main-flow tool_use ids.
type subObs struct {
	s  *Session
	id string
}

func (o subObs) OnToolStart(e tools.ExecEvent) {
	o.s.emit(Event{
		Type:     EventToolExecStart,
		ExecID:   e.ExecID,
		ToolName: e.Name,
		Input:    e.Input,
		Sub:      true,
		SubID:    o.id,
		TS:       e.Start,
	})
	if o.s.extObs != nil {
		o.s.extObs.OnToolStart(e)
	}
}

func (o subObs) OnToolEnd(e tools.ExecEvent) {
	o.s.emit(Event{
		Type:       EventToolExecEnd,
		ExecID:     e.ExecID,
		ToolName:   e.Name,
		DurationMS: e.Duration.Milliseconds(),
		IsError:    e.Err != "",
		Err:        e.Err,
		Sub:        true,
		SubID:      o.id,
		TS:         time.Now(),
	})
	if o.s.extObs != nil {
		o.s.extObs.OnToolEnd(e)
	}
}

// runNested runs one nested tool-loop to completion on model. subID == ""
// is the legacy single-call attribution (subNest counter + session
// observer); a non-empty subID opens an EventSubagentStart, stamps every
// inner execution with the token and closes with a matched
// EventSubagentEnd — the run_subagents fan-out runs several of these
// concurrently (#53). preview seeds the task node's row. The answer,
// usage and iteration count come back unformatted for the caller to merge.
func (s *Session) runNested(ctx context.Context, model, task, subID, preview string) (string, Usage, int, error) {
	// Mark the nested run so OnToolStart/End stamp Event.Sub — renderers
	// nest these executions under this task instead of mixing them into
	// the main flow. The outer exec brackets this window (its own start/end
	// fire outside), so it stays at depth 0. The counter is shared across
	// concurrent fan-out runs: attribution inside them is by SubID.
	atomic.AddInt32(&s.subNest, 1)
	defer atomic.AddInt32(&s.subNest, -1)

	obs := tools.Observer(s)
	if subID != "" {
		obs = subObs{s: s, id: subID}
		s.emit(Event{
			Type: EventSubagentStart, Sub: true, SubID: subID,
			ToolName: model, Detail: preview, TS: time.Now(),
		})
	}

	runner := s.eng.client.Beta.Messages.NewToolRunnerStreaming(s.nestedTools(obs), anthropic.BetaToolRunnerParams{
		BetaMessageNewParams: anthropic.BetaMessageNewParams{
			Model:     model,
			MaxTokens: s.eng.cfg.SubagentMaxTokens,
			System: []anthropic.BetaTextBlockParam{{
				Text:         SubagentSystemPrompt,
				CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
			}},
			Messages:     []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(task))},
			OutputConfig: anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffort(s.eng.cfg.SubagentEffort)},
			CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
			Betas:        []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
			Fallbacks:    anthropic.BetaFallbacksParamOfDefault(),
		},
		// No MaxIterations cap: 0 (the SDK default) runs until the model
		// stops calling tools — same unlimited-rounds stance as the main
		// loop's default MaxIterations=0.
	})

	start := time.Now()
	iters := 0
	fail := func(err error) (string, Usage, int, error) {
		if subID != "" {
			// Close this task's node — a fan-out has no parent exec row to
			// fall back on, a dangling spinner would lie about the state.
			s.emit(Event{
				Type: EventSubagentEnd, Sub: true, SubID: subID,
				ToolName: model, IsError: true, Err: err.Error(),
				DurationMS: time.Since(start).Milliseconds(), TS: time.Now(),
			})
		}
		return "", Usage{}, iters, err
	}
	for !runner.IsCompleted() {
		if ctx.Err() != nil {
			return fail(ctx.Err())
		}
		if !s.eng.acquireTurn(ctx) {
			return fail(ctx.Err())
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
			return fail(fmt.Errorf("subagent: %w", iterErr))
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

	s.recordSub(model, usage) // cost gauge: billed separately
	s.emit(Event{
		Type:       EventSubagentEnd,
		Sub:        subID != "",
		SubID:      subID,
		ToolName:   model,
		Usage:      &usage,
		DurationMS: time.Since(start).Milliseconds(),
		Detail:     fmt.Sprintf("%d API calls", iters),
		TS:         time.Now(),
	})
	return answer.String(), usage, iters, nil
}
