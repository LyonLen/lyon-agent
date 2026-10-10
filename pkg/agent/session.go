package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"lyon-agent/pkg/hooks"
	"lyon-agent/pkg/tools"
)

var (
	// ErrSessionClosed is returned by Submit after Close.
	ErrSessionClosed = errors.New("session closed")
	// ErrPromptInProgress is returned by Submit while a prompt is still running.
	ErrPromptInProgress = errors.New("a prompt is already in progress")
	// ErrInterrupted is the prompt_end error reported when Interrupt stops
	// an in-flight prompt. The session stays usable for the next prompt.
	ErrInterrupted = errors.New("interrupted")
)

// TurnStat is the accounting of one completed API turn.
type TurnStat struct {
	Turn       int
	StopReason string
	Usage      Usage
	Duration   time.Duration
	At         time.Time
}

// Session owns one conversation: a single goroutine drives its tool runner
// (the SDK runner is not goroutine-safe), a bounded channel carries Events to
// the consumer, and a session-scoped mutex serializes mutating tools.
//
// Lifecycle: Submit(prompt) once at a time; drain Events() until the matching
// EventPromptEnd; repeat; Close() when done.
type Session struct {
	eng       *Engine
	ctx       context.Context
	streamCtx context.Context // ctx + session value: tool handlers find their Session
	cancel    context.CancelFunc
	events    chan Event
	reqs      chan string
	closeOne  sync.Once
	extObs    tools.Observer

	// serialMu is held by non-parallel-safe tools during execution so
	// mutating tools never overlap inside this session.
	serialMu  sync.Mutex
	betaTools []anthropic.BetaTool // adapted once: deterministic across runner recreations
	seq       atomic.Uint64        // shared ExecID counter across all tools

	// runner is owned exclusively by the drive goroutine.
	runner *anthropic.BetaToolRunnerStreaming
	busy   atomic.Bool

	// promptCancel cancels the in-flight prompt's derived context. Set by
	// drive at prompt start, consumed by Interrupt; nil while idle.
	promptCancel atomic.Pointer[context.CancelFunc]

	// pendingToolResults holds synthetic tool_result blocks queued by
	// repairInterruptedHistory, merged into the next prompt's user message
	// by buildHistory. Drive goroutine only — no lock needed.
	pendingToolResults []anthropic.BetaContentBlockParamUnion

	// histMu guards runner, seed and pendingToolResults against Export and
	// Restore: drive holds it for a whole prompt, so a snapshot taken at
	// exit (after Interrupt) sees one stable history. Lock order: histMu
	// before mu.
	histMu sync.Mutex

	// seed is a restored history waiting for the first prompt to create a
	// runner; buildHistory falls back to it while runner is nil.
	seed []anthropic.BetaMessageParam

	// lastCompactInput is the usage total that most recently triggered
	// compaction. The next trigger must exceed it, so a compaction that
	// fails to shrink usage cannot loop.
	lastCompactInput float64

	// memoryText is the workspace memory block injected into this session's
	// first prompt, exactly once (memoryInjected).
	memoryText     string
	memoryInjected bool

	// compactArmed defers an already-triggered compaction until the next
	// turn has run its pending tool calls, so the summary covers their
	// results before the history is replaced.
	compactArmed bool

	// iterOffset accumulates iteration counts across runner recreations
	// (compaction mid-prompt) so prompt_end reports the prompt's true API
	// call total. Drive goroutine only.
	iterOffset int

	mu          sync.Mutex
	turns       []TurnStat
	pendingNote string // next-boundary role:"system" message (PushSystemNote)

	// Session intent title (title.go): recognized asynchronously after
	// prompt_end, persisted with snapshots. Guarded by mu.
	title       string
	firstPrompt string
	lastPrompt  string
}

func newSession(e *Engine, obs tools.Observer) *Session {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{
		eng:        e,
		ctx:        ctx,
		cancel:     cancel,
		events:     make(chan Event, e.cfg.EventBuffer),
		reqs:       make(chan string, 1),
		extObs:     obs,
		memoryText: tools.MemoryFacts(e.cfg.Workspace),
	}
	s.streamCtx = context.WithValue(ctx, sessionCtxKey{}, s)
	s.betaTools = e.reg.Adapt(tools.AdapterConfig{
		SerialMu:  &s.serialMu,
		Timeout:   e.cfg.ToolTimeout,
		ResultCap: e.cfg.ToolResultCap,
		Observer:  s,
		Approve:   s.wrapApprove(e.cfg.Approve),
		PreTool:   s.preTool,
		Seq:       &s.seq,
	})
	go s.loop()
	return s
}

// Events returns the session's event stream. It is closed when the session
// is closed. Consumers must drain it while a prompt is running — the buffer
// is bounded and backpressure pauses the driver.
func (s *Session) Events() <-chan Event { return s.events }

// Submit queues a prompt for execution. It returns nil once the prompt is
// accepted; completion is signaled by EventPromptEnd on Events().
func (s *Session) Submit(prompt string) error {
	if s.ctx.Err() != nil {
		return ErrSessionClosed
	}
	if !s.busy.CompareAndSwap(false, true) {
		return ErrPromptInProgress
	}
	select {
	case s.reqs <- prompt:
		// Recorded for title intent recognition (title.go): the latest
		// prompt drives the check, the first anchors the session topic.
		s.mu.Lock()
		s.lastPrompt = prompt
		if s.firstPrompt == "" {
			s.firstPrompt = prompt
		}
		s.mu.Unlock()
		s.eng.hooks.Fire(hooks.Record{Event: hooks.EvPromptStart, Prompt: prompt})
		return nil
	case <-s.ctx.Done():
		s.busy.Store(false)
		return ErrSessionClosed
	}
}

// Hooks returns the engine's hook manager through the session (audit,
// stats, security gate) — the TUI reads aggregates from here for /stats.
func (s *Session) Hooks() *hooks.Manager { return s.eng.hooks }

// preTool is the security seam handed to every adapted tool (main and
// subagent): local policy and blocking hooks veto before approval.
func (s *Session) preTool(ctx context.Context, name string, input json.RawMessage) error {
	return s.eng.hooks.CheckTool(ctx, name, input)
}

// wrapApprove brackets the configured approver with audit events so every
// human decision (including "always allow" auto-answers) lands in the log.
// nil stays nil — auto-approve sessions don't fake approvals.
func (s *Session) wrapApprove(a tools.ApproveFunc) tools.ApproveFunc {
	if a == nil {
		return nil
	}
	return func(ctx context.Context, e tools.ExecEvent) bool {
		s.eng.hooks.Fire(hooks.Record{
			Event: hooks.EvApproveReq,
			Tool:  e.Name,
			Input: string(e.Input),
		})
		ok := a(ctx, e)
		s.eng.hooks.Fire(hooks.Record{
			Event:    hooks.EvApproveRes,
			Tool:     e.Name,
			Approved: &ok,
		})
		return ok
	}
}

// Close cancels any in-flight work, stops the session goroutine and closes
// the event stream. Idempotent. Also flushes the engine's audit queue so
// the session's tail records survive process exit.
func (s *Session) Close() {
	s.closeOne.Do(func() {
		s.cancel()
		s.eng.hooks.Flush()
	})
}

// Interrupt cancels the in-flight prompt without closing the session: the
// current API stream and any running tools are canceled, a dangling
// tool-call in history is repaired for the next prompt, and the prompt ends
// with EventPromptEnd{Err: "interrupted"}. Returns false when no prompt is
// running. Safe to call from any goroutine.
func (s *Session) Interrupt() bool {
	pc := s.promptCancel.Swap(nil)
	if pc == nil {
		return false
	}
	(*pc)()
	return true
}

// Stats returns a copy of the completed-turn accounting for this session.
func (s *Session) Stats() []TurnStat {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TurnStat, len(s.turns))
	copy(out, s.turns)
	return out
}

// loop is the session's single goroutine: it receives prompts and drives the
// tool runner to completion for each one.
func (s *Session) loop() {
	defer close(s.events)
	for {
		select {
		case <-s.ctx.Done():
			return
		case prompt := <-s.reqs:
			s.drive(prompt)
		}
	}
}

// baseParams builds the request parameters shared by every turn of this
// session. Byte-stability here is what makes the prompt cache hit: frozen
// system prompt first with an explicit breakpoint, top-level automatic
// caching for the conversation tail, pinned effort, server-side refusal
// fallbacks enabled.
func (s *Session) baseParams(msgs []anthropic.BetaMessageParam) anthropic.BetaMessageNewParams {
	cfg := s.eng.cfg
	return anthropic.BetaMessageNewParams{
		Model:     cfg.Model,
		MaxTokens: cfg.MaxTokens,
		Messages:  msgs,
		System: []anthropic.BetaTextBlockParam{{
			Text:         cfg.SystemPrompt,
			CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
		}},
		OutputConfig: anthropic.BetaOutputConfigParam{
			Effort: anthropic.BetaOutputConfigEffort(cfg.Effort),
		},
		CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
		Betas:        []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
		Fallbacks:    anthropic.BetaFallbacksParamOfDefault(),
	}
}

// drive runs one prompt through the full tool loop. Runner rule: exactly one
// goroutine, this one. History rule: the runner is recreated per human
// prompt, seeded with the previous runner's history — byte-identical prefix,
// so the cache keeps hitting — because a runner permanently latches
// completed=true after its final turn.
func (s *Session) drive(prompt string) {
	// histMu spans the whole prompt so Export/Restore (exit-time snapshot,
	// chat -c) observe one stable history: the caller Interrupts first, then
	// Export waits here until this prompt finishes bookkeeping.
	s.histMu.Lock()
	defer s.histMu.Unlock()
	defer s.busy.Store(false)
	s.iterOffset = 0

	// The prompt runs on its own cancellable context so Interrupt can stop
	// just this prompt (stream + tools) while the session survives.
	pctx, pcancel := context.WithCancel(s.streamCtx)
	s.promptCancel.Store(&pcancel)
	defer func() {
		s.promptCancel.CompareAndSwap(&pcancel, nil)
		pcancel()
	}()

	hist := s.buildHistory(s.takePendingNote(), prompt)

	s.runner = s.eng.client.Beta.Messages.NewToolRunnerStreaming(s.betaTools, anthropic.BetaToolRunnerParams{
		BetaMessageNewParams: s.baseParams(hist),
		MaxIterations:        s.eng.cfg.MaxIterations,
	})
	// A deferred compaction died with the old runner (a failed prompt may
	// have armed it); this prompt re-evaluates the threshold from its own
	// turns instead.
	s.compactArmed = false
	// Server-side web tools ride along in the tools array. The runner built
	// that array from the client tools; appending here keeps client tools
	// first and in sorted order, so the prefix stays byte-stable across
	// runner recreations. Server tools never enter the runner's tool map —
	// the API executes them and streams their results within one turn.
	if s.eng.cfg.WebToolMode == "server" {
		s.runner.Params.Tools = append(s.runner.Params.Tools, serverToolDefs()...)
	}

	promptErr := ""
	// Continue turn numbering across a restored session: record() numbers by
	// len(turns)+1 the same way.
	turnIdx := len(s.turns)
	empty := 0 // consecutive iterator rounds with zero events

	// stopReason distinguishes a closed session from an Interrupt: both
	// cancel pctx, which derives from the session context.
	stopReason := func() string {
		if s.ctx.Err() != nil {
			return s.ctx.Err().Error()
		}
		return ErrInterrupted.Error()
	}

	for !s.runner.IsCompleted() {
		if pctx.Err() != nil {
			promptErr = stopReason()
			break
		}

		// Global turn admission: bounds in-flight API+tool iterations across
		// every session of this engine when MaxConcurrentTurns is set.
		if !s.eng.acquireTurn(pctx) {
			promptErr = stopReason()
			break
		}

		turnStart := time.Now()
		got := false
		var turnErr error

		for ev, err := range s.runner.NextStreaming(pctx) {
			if err != nil {
				turnErr = err
				break
			}
			got = true
			if e := normalize(ev); e != nil {
				s.emit(*e)
			}
		}
		s.eng.releaseTurn()

		if turnErr == nil {
			turnErr = s.runner.Err()
		}
		if turnErr != nil {
			if pctx.Err() != nil && s.ctx.Err() == nil {
				// Interrupted on purpose: not a failure — the prompt_end
				// below reports ErrInterrupted without a red error event.
				promptErr = ErrInterrupted.Error()
			} else {
				s.emit(Event{Type: EventError, Err: turnErr.Error(), TS: time.Now()})
				promptErr = turnErr.Error()
			}
			break
		}

		if !got {
			// The runner returned no request: either it completed (final
			// turn consumed) or nothing progressed — bail rather than spin.
			empty++
			if s.runner.IsCompleted() || empty > 2 {
				if !s.runner.IsCompleted() && promptErr == "" {
					promptErr = "runner made no progress"
					s.emit(Event{Type: EventError, Err: promptErr, TS: time.Now()})
				}
				break
			}
			continue
		}
		empty = 0

		turnIdx++
		if msg := s.runner.LastMessage(); msg != nil {
			u := Usage{
				Input:         msg.Usage.InputTokens,
				Output:        msg.Usage.OutputTokens,
				CacheRead:     msg.Usage.CacheReadInputTokens,
				CacheCreation: msg.Usage.CacheCreationInputTokens,
			}
			stop := string(msg.StopReason)
			compactionTurn := isCompactionMessage(msg)
			if compactionTurn {
				// The runner consumed a compaction request: history was just
				// replaced by the summary. Surface it as its own turn kind.
				stop = "compaction"
				s.compactArmed = false
				s.emit(Event{
					Type:       EventCompaction,
					Usage:      &u,
					DurationMS: time.Since(turnStart).Milliseconds(),
					Detail:     "summary replaced the conversation history",
					TS:         time.Now(),
				})
			}
			stat := TurnStat{
				Turn:       turnIdx,
				StopReason: stop,
				Usage:      u,
				Duration:   time.Since(turnStart),
				At:         time.Now(),
			}
			s.record(stat)
			s.emit(Event{
				Type:       EventTurnEnd,
				Turn:       turnIdx,
				StopReason: stat.StopReason,
				Usage:      &u,
				DurationMS: stat.Duration.Milliseconds(),
				TS:         stat.At,
			})
			// Compaction runs client-side (compactNow), so it works on any
			// endpoint. A turn that just requested tools defers it: the
			// summary must cover the results those tools are about to write.
			// A compaction turn's own usage covers the history it just
			// summarized — feeding it back into the ratio would re-loop.
			if !compactionTurn {
				if s.compactArmed {
					s.compactArmed = false
					if s.maybeCompact(u) {
						s.compactNow(pctx, prompt, u, &turnIdx)
					}
				} else if s.maybeCompact(u) {
					if msg.StopReason == anthropic.BetaStopReasonToolUse {
						s.compactArmed = true
					} else {
						s.compactNow(pctx, prompt, u, &turnIdx)
					}
				}
			}
		}
	}

	// Before the next prompt can observe it, close a history that ends mid
	// tool-call (interrupt raced the tool dispatch, or MaxIterations cut it).
	s.repairInterruptedHistory()

	// Release the submit lock and the interrupt handle BEFORE announcing
	// completion: events is buffered, so a consumer can receive prompt_end
	// and Submit the next prompt while this goroutine is still returning —
	// clearing busy after the emit loses that CAS (ErrPromptInProgress).
	s.promptCancel.CompareAndSwap(&pcancel, nil)
	pcancel()
	s.busy.Store(false)

	s.emit(Event{
		Type:       EventPromptEnd,
		Err:        promptErr,
		Iterations: s.iterOffset + s.runner.IterationCount(),
		TS:         time.Now(),
	})
}

// repairInterruptedHistory queues synthetic error tool_results when the
// history ends mid tool-call: the last message is an assistant tool_use with
// no tool_result following it. The API rejects tool_use blocks without their
// results, so the answers are merged into the next prompt's user message by
// buildHistory. Runs only on the drive goroutine.
func (s *Session) repairInterruptedHistory() {
	if s.runner == nil {
		return
	}
	msgs := s.runner.Params.Messages
	if len(msgs) == 0 {
		return
	}
	last := msgs[len(msgs)-1]
	if last.Role != anthropic.BetaMessageParamRoleAssistant {
		return
	}
	for _, b := range last.Content {
		if tu := b.OfToolUse; tu != nil {
			res := anthropic.NewBetaToolResultTextBlockParam(
				tu.ID, "prompt interrupted before the tool finished", true)
			s.pendingToolResults = append(s.pendingToolResults,
				anthropic.BetaContentBlockParamUnion{OfToolResult: &res})
		}
	}
}

// maybeCompact reports whether the latest turn's usage crosses the
// compaction threshold; the compaction itself runs in compactNow. The beta
// CompactBeforeNextTurn path is deliberately not used: gateways that never
// return a compaction block leave the history untouched forever while
// silently paying for the request. lastCompactInput makes the trigger fire
// at most once per genuine growth step — a compaction that failed to shrink
// usage cannot re-loop at the same total.
func (s *Session) maybeCompact(u Usage) bool {
	th := s.eng.cfg.CompactThreshold // negative disables
	if th < 0 {
		return false
	}
	window := float64(s.eng.cfg.ContextWindow)
	if window <= 0 {
		return false
	}
	total := float64(u.Input + u.CacheRead + u.CacheCreation)
	if total/window < th {
		return false
	}
	if s.lastCompactInput > 0 && total <= s.lastCompactInput {
		return false
	}
	return true
}

// isCompactionMessage reports whether a completed turn is the compaction
// response (a single compaction block and no stop reason to answer).
func isCompactionMessage(msg *anthropic.BetaMessage) bool {
	if msg == nil || len(msg.Content) != 1 {
		return false
	}
	return msg.Content[0].Type == "compaction"
}

// emit pushes an event, abandoning the drop if the session is closing.
// Events without a timestamp (raw stream deltas) get stamped here so every
// consumer — CLI renderer, SSE client — sees a usable TS.
func (s *Session) emit(e Event) {
	if e.TS.IsZero() {
		e.TS = time.Now()
	}
	// Audit everything except raw stream noise (deltas and message_start):
	// those would drown the log without adding decisions. Fire never blocks.
	switch e.Type {
	case EventTextDelta, EventThinkingDelta, EventMessageStart:
	default:
		var u *hooks.Usage
		if e.Usage != nil {
			u = &hooks.Usage{
				Input:         e.Usage.Input,
				Output:        e.Usage.Output,
				CacheRead:     e.Usage.CacheRead,
				CacheCreation: e.Usage.CacheCreation,
			}
		}
		s.eng.hooks.Fire(hooks.Record{
			Event:      string(e.Type),
			Tool:       e.ToolName,
			Input:      string(e.Input),
			DurationMS: e.DurationMS,
			IsError:    e.IsError,
			Err:        e.Err,
			Usage:      u,
			Iterations: e.Iterations,
			StopReason: e.StopReason,
			Detail:     e.Detail,
		})
	}
	select {
	case s.events <- e:
	case <-s.ctx.Done():
	}
}

func (s *Session) record(t TurnStat) {
	s.mu.Lock()
	t.Turn = len(s.turns) + 1
	s.turns = append(s.turns, t)
	s.mu.Unlock()
}

// OnToolStart / OnToolEnd implement tools.Observer: they translate harness
// execution into session events and forward to the external observer.
func (s *Session) OnToolStart(e tools.ExecEvent) {
	s.emit(Event{
		Type:     EventToolExecStart,
		ExecID:   e.ExecID,
		ToolName: e.Name,
		Input:    e.Input,
		TS:       e.Start,
	})
	if s.extObs != nil {
		s.extObs.OnToolStart(e)
	}
}

func (s *Session) OnToolEnd(e tools.ExecEvent) {
	s.emit(Event{
		Type:       EventToolExecEnd,
		ExecID:     e.ExecID,
		ToolName:   e.Name,
		DurationMS: e.Duration.Milliseconds(),
		IsError:    e.Err != "",
		Err:        e.Err,
		TS:         time.Now(),
	})
	if s.extObs != nil {
		s.extObs.OnToolEnd(e)
	}
}

// PushSystemNote queues dynamic context (time, environment, reminders) as a
// role:"system" message inserted at the next prompt's turn boundary — the
// frozen top-level system prompt stays byte-identical, so the cache prefix
// only pays for the appended note. The note is consumed by the next Submit.
func (s *Session) PushSystemNote(text string) {
	s.mu.Lock()
	s.pendingNote = text
	s.mu.Unlock()
}

// takePendingNote returns and clears the queued system note.
func (s *Session) takePendingNote() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	note := s.pendingNote
	s.pendingNote = ""
	return note
}

// buildHistory assembles the message list for one prompt: previous runner
// history (or a restored seed), the workspace memory block on first use, an
// optional system note at the turn boundary, any queued tool_results from an
// interrupted tool-call, then the user prompt. When history already ends
// with a user message — a restored snapshot or a compaction summary — the
// new material joins that message as extra blocks instead of becoming a
// second consecutive user message, so role alternation stays intact.
// Split out so assembly is testable without an API call.
func (s *Session) buildHistory(note, prompt string) []anthropic.BetaMessageParam {
	var hist []anthropic.BetaMessageParam
	switch {
	case s.runner != nil:
		hist = s.runner.Messages()
	case s.seed != nil:
		hist = append([]anthropic.BetaMessageParam(nil), s.seed...)
	}
	if strings.TrimSpace(note) != "" {
		hist = append(hist, anthropic.NewBetaSystemMessage(
			anthropic.BetaSystemMessageOutputConfigParam{},
			anthropic.NewBetaTextBlock(note),
		))
	}
	blocks := make([]anthropic.BetaContentBlockParamUnion, 0, len(s.pendingToolResults)+2)
	blocks = append(blocks, s.pendingToolResults...)
	s.pendingToolResults = nil
	// Memory rides as user-message blocks (never role:"system" — that path
	// is unproven on every gateway this agent targets), injected exactly
	// once per session unless the restored history already carries it.
	// Containment compares the JSON-escaped forms: raw newlines never
	// survive into the marshaled history.
	if !s.memoryInjected && s.memoryText != "" {
		inject := true
		if memRaw, err := json.Marshal(s.memoryText); err == nil {
			if histRaw, err := json.Marshal(hist); err == nil {
				inject = !bytes.Contains(histRaw, memRaw[1:len(memRaw)-1])
			}
		}
		if inject {
			blocks = append(blocks, anthropic.NewBetaTextBlock(s.memoryText))
		}
		s.memoryInjected = true
	}
	// @path tokens resolving to workspace images become image blocks, so a
	// user can paste a picture into any prompt (CLI, TUI, HTTP). Plain
	// prompts come back as one verbatim text block — cache prefix unchanged.
	blocks = append(blocks, tools.PromptBlocks(s.eng.cfg.Workspace, prompt)...)

	// Trailing-user merge: hist ends with a user message (restored snapshot
	// or compaction summary) → append to it. Copy its Content first — the
	// element header came from runner.Params (or seed) and its backing
	// array is shared with the live history.
	if n := len(hist); n > 0 && hist[n-1].Role == anthropic.BetaMessageParamRoleUser {
		merged := hist[n-1]
		merged.Content = append([]anthropic.BetaContentBlockParamUnion(nil), merged.Content...)
		merged.Content = append(merged.Content, blocks...)
		hist[n-1] = merged
		return hist
	}
	return append(hist, anthropic.NewBetaUserMessage(blocks...))
}

// String renders the session identity for logs.
func (s *Session) String() string {
	return fmt.Sprintf("session(turns=%d)", len(s.Stats()))
}
