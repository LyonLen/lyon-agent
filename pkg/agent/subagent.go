package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
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

// OnToolOutput carries a live output chunk out of a dispatched subtask
// (#11) with the same Sub/SubID stamps as its start/end, so the panel
// files the lines under the right task node in a concurrent fan-out.
func (o subObs) OnToolOutput(e tools.ExecEvent) {
	o.s.emit(Event{
		Type:     EventToolExecOutput,
		ExecID:   e.ExecID,
		ToolName: e.Name,
		Text:     e.Output,
		Sub:      true,
		SubID:    o.id,
	})
	if oo, ok := o.s.extObs.(tools.OutputObserver); ok {
		oo.OnToolOutput(e)
	}
}

// runNested runs one nested tool-loop to completion on model. subID == ""
// is the legacy single-call attribution (subNest counter + session
// observer); a non-empty subID opens an EventSubagentStart, stamps every
// inner execution with the token and closes with a matched
// EventSubagentEnd — the run_subagents fan-out runs several of these
// concurrently (#53). preview seeds the task node's row. The answer,
// usage and iteration count come back unformatted for the caller to merge.
//
// The run is bounded by a no-progress (idle) budget, not a wall clock:
// every stream event, tool start/end and round transition resets the
// clock, so healthy multi-round research can take as long as it keeps
// moving while a genuinely stuck run (hung request, starved turn slot,
// dead gateway) is cancelled after 2×ToolTimeout of total silence. One
// ToolTimeout of silence is still explainable — an inner tool is allowed
// to run that long — so the budget is twice that. Failures come back
// through diagnose() with the task, elapsed time and stall point.
func (s *Session) runNested(ctx context.Context, model, task, subID, preview string) (string, Usage, int, error) {
	// Mark the nested run so OnToolStart/End stamp Event.Sub — renderers
	// nest these executions under this task instead of mixing them into
	// the main flow. The outer exec brackets this window (its own start/end
	// fire outside), so it stays at depth 0. The counter is shared across
	// concurrent fan-out runs: attribution inside them is by SubID.
	atomic.AddInt32(&s.subNest, 1)
	defer atomic.AddInt32(&s.subNest, -1)

	// A legacy run_subagent passes no preview (it has no task node); the
	// diagnosis still needs the task's name — derive it from the task.
	if preview == "" {
		preview = clipRunes(strings.Join(strings.Fields(task), " "), 56)
	}

	obs := tools.Observer(s)
	if subID != "" {
		obs = subObs{s: s, id: subID}
		s.emit(Event{
			Type: EventSubagentStart, Sub: true, SubID: subID,
			ToolName: model, Detail: preview, TS: time.Now(),
		})
	}
	prog := &nestProgress{
		budget: 2 * s.eng.cfg.ToolTimeout,
		last:   time.Now(),
		act:    "starting",
	}
	obs = &progressObs{p: prog, inner: obs}

	// Watchdog context: inherits the caller's cancellation (Interrupt,
	// session close) but is cancelled by the idle watchdog when the run
	// goes silent for the whole budget.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := make(chan struct{})
	defer close(stop)
	go prog.watch(runCtx, cancel, stop)

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
	// The caller's wall clock (the adapter's ToolTimeout while it still
	// wraps these tools) — named in a deadline diagnosis so the error says
	// WHICH budget was hit, not just that a context died.
	wall, hasWall := ctx.Deadline()
	iters := 0
	fail := func(err error) (string, Usage, int, error) {
		err = prog.diagnose(runCtx.Err(), err, preview, iters, start, wall, hasWall)
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
		if runCtx.Err() != nil {
			return fail(runCtx.Err())
		}
		prog.touch("waiting for a turn slot")
		if !s.eng.acquireTurn(runCtx) {
			return fail(runCtx.Err())
		}
		round := iters + 1
		prog.touch(fmt.Sprintf("api round %d", round))
		var iterErr error
		got := false
		for _, err := range runner.NextStreaming(runCtx) {
			if err != nil {
				iterErr = err
				break
			}
			got = true
			// Stream events are the heartbeat: tool executions inside this
			// round stamp progress through progressObs on their own.
			prog.touch(fmt.Sprintf("api round %d", round))
		}
		s.eng.releaseTurn()
		if iterErr == nil {
			iterErr = runner.Err()
		}
		if iterErr != nil {
			return fail(iterErr)
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

// nestProgress feeds one nested run's no-progress watchdog: every stream
// event, tool start/end and round transition stamps the last-activity
// clock, and the watchdog cancels a run whose clock has been silent for
// the whole budget. A wall clock kills healthy multi-round research at an
// arbitrary second; silence is what "stuck" actually means.
type nestProgress struct {
	budget time.Duration // no-progress bound: 2×ToolTimeout (runNested)

	mu      sync.Mutex
	last    time.Time
	act     string // where the run believes it is: "tool read_file", "api round 3", …
	stalled bool   // set by the watchdog right before it cancels
}

// touch records activity at where. Cheap and lock-short: it runs on every
// stream delta.
func (p *nestProgress) touch(where string) {
	p.mu.Lock()
	p.last = time.Now()
	p.act = where
	p.mu.Unlock()
}

// stale reports whether nothing has happened for the whole budget.
func (p *nestProgress) stale() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return time.Since(p.last) >= p.budget
}

// markStalled records that the watchdog — not a context deadline — is
// about to be the executioner, so diagnose can say "no progress" instead
// of misattributing the kill.
func (p *nestProgress) markStalled() {
	p.mu.Lock()
	p.stalled = true
	p.mu.Unlock()
}

// snapshot reads what a diagnosis needs: the stall point, how long the
// run has been silent there, and whether the watchdog fired.
func (p *nestProgress) snapshot() (where string, since time.Duration, stalled bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.act, time.Since(p.last), p.stalled
}

// watch cancels the run once it goes silent for the whole budget. It
// exits when the run does (stop) or when either side of the run's context
// ends, so one watchdog goroutine per nested run leaves no residue.
func (p *nestProgress) watch(ctx context.Context, cancel context.CancelFunc, stop <-chan struct{}) {
	interval := p.budget / 4
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			if p.stale() {
				p.markStalled()
				cancel()
				return
			}
		}
	}
}

// diagnose turns a nested-run failure into a self-contained, actionable
// error: which task, how long it ran, how many rounds it completed and
// where it was when it died. Timeouts name the actual killer — the idle
// watchdog ("no progress") or the caller's wall-clock budget — instead of
// a bare context error the model can only retry blind.
func (p *nestProgress) diagnose(rctxErr, err error, task string, rounds int, start, wall time.Time, hasWall bool) error {
	if err == nil {
		return nil
	}
	where, since, stalled := p.snapshot()
	elapsed := time.Since(start).Round(time.Second)
	switch {
	case stalled:
		return fmt.Errorf("subagent timed out with no progress for %s (task %q, elapsed %s, at %s)",
			p.budget, task, elapsed, where)
	case errors.Is(rctxErr, context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded):
		budget := time.Duration(0)
		if hasWall {
			budget = wall.Sub(start).Round(10 * time.Millisecond)
		}
		return fmt.Errorf("subagent hit the %s per-call time budget (task %q, elapsed %s, %d round(s) done, at %s, last activity %s ago) — shrink the task or raise ToolTimeout",
			budget, task, elapsed, rounds, where, since.Round(time.Second))
	default:
		return fmt.Errorf("subagent failed (task %q, elapsed %s, %d round(s) done, at %s): %w",
			task, elapsed, rounds, where, err)
	}
}

// progressObs stamps inner tool executions as the nested run's heartbeat —
// the watchdog's proof of life — and forwards to the attribution observer
// (the session, or the fan-out's subObs) unchanged. It also stands in for
// that observer as the adapter's live-output sink, so open-step streaming
// keeps flowing (#11).
type progressObs struct {
	p     *nestProgress
	inner tools.Observer
}

func (o *progressObs) OnToolStart(e tools.ExecEvent) {
	o.p.touch("tool " + e.Name)
	o.inner.OnToolStart(e)
}

func (o *progressObs) OnToolEnd(e tools.ExecEvent) {
	o.p.touch("tool " + e.Name)
	o.inner.OnToolEnd(e)
}

func (o *progressObs) OnToolOutput(e tools.ExecEvent) {
	o.p.touch("tool " + e.Name)
	if oo, ok := o.inner.(tools.OutputObserver); ok {
		oo.OnToolOutput(e)
	}
}
