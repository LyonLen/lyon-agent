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
	// ErrNoPromptRunning is returned by Enqueue when the session is idle —
	// there is no run to inject into; Submit the line instead.
	ErrNoPromptRunning = errors.New("no prompt running: use Submit")
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
// submitReq is one queued prompt. Model carries the per-prompt override
// (#53 routing consumption): "" rides the session's config model.
type submitReq struct {
	prompt string
	model  string
}

type Session struct {
	eng       *Engine
	ctx       context.Context
	streamCtx context.Context // ctx + session value: tool handlers find their Session
	cancel    context.CancelFunc
	events    chan Event
	reqs      chan submitReq
	closeOne  sync.Once
	extObs    tools.Observer

	// serialMu is held by non-parallel-safe tools during execution so
	// mutating tools never overlap inside this session.
	serialMu  sync.Mutex
	betaTools []anthropic.BetaTool // adapted once: deterministic across runner recreations
	seq       atomic.Uint64        // shared ExecID counter across all tools
	disSeq    atomic.Uint64        // run_subagents fan-out run ids (#53 SubID attribution)

	// runner is owned exclusively by the drive goroutine.
	runner *anthropic.BetaToolRunnerStreaming
	busy   atomic.Bool

	// inbox holds Enqueue'd prompts waiting for the next turn gap (#2).
	// Guarded by mu, and the idle transition shares that critical section:
	// drive clears inbox in the same lock that drops busy, so an entry can
	// neither outlive its run (the caller still holds its copy and re-sends
	// on prompt_end) nor be delivered twice.
	inbox []string

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

	// Undo stack (rollback.go): checkpoints are captured under histMu but
	// guarded by their own mutex so listing never waits on a live prompt.
	// Lock order: histMu before cpMu.
	cpMu          sync.Mutex
	checkpoints   []checkpoint
	checkpointSeq int

	// seed is a restored history waiting for the first prompt to create a
	// runner; buildHistory falls back to it while runner is nil.
	seed []anthropic.BetaMessageParam

	// lastCompactInput is the usage total that most recently triggered
	// compaction. The next trigger must exceed it, so a compaction that
	// fails to shrink usage cannot loop.
	lastCompactInput float64

	// memoryText is the workspace memory block injected into this session's
	// prompts (memoryInjected): a creation-time snapshot that buildHistory
	// re-reads against the store on every prompt (D1 — a mid-session
	// save/forget or distill write re-arms injection with the changed text;
	// the containment check inside buildHistory still spares a restored
	// history that already carries it). memEcho asks the drive goroutine to
	// announce an injection on the event stream — set inside buildHistory
	// (which tests call without a consumer), fired by drive right after, so
	// a direct assembly test never blocks on the channel.
	memoryText     string
	memoryInjected bool
	memEcho        bool

	// skillsText is the skills-availability section (skills.go) appended
	// to this session's system prompt after the frozen base. Scanned once
	// at session creation, then byte-stable for the session — the prompt
	// cache prefix only misses once per process, not per turn. Empty when
	// no skills exist (nothing is injected).
	skillsText string

	// shellText is the one-line default-shell marker (shellEnvSection)
	// riding right after skillsText: the model writes shell commands
	// before ever executing one, so it has to know whether that means
	// bash or powershell up front. tools detects the shell once per
	// process, so the line is byte-stable — the cache prefix holds.
	shellText string

	// compactArmed defers an already-triggered compaction until the next
	// turn has run its pending tool calls, so the summary covers their
	// results before the history is replaced.
	compactArmed bool

	// iterOffset accumulates iteration counts across runner recreations
	// (compaction mid-prompt) so prompt_end reports the prompt's true API
	// call total. Drive goroutine only.
	iterOffset int

	// dumpRec is this run's dump recorder (#3): drive stores a fresh one
	// at prompt start, emit appends every event while it is open, and the
	// prompt_end path swaps it out to write $LYON_HOME/dumps/… — one file
	// per finished run (dump.go). atomic.Pointer because emit runs on
	// tool/subagent goroutines while store/swap stay on the drive
	// goroutine; nil while idle drops any straggler.
	dumpRec atomic.Pointer[runDump]
	// dumpPending counts dump file writes still on disk (finishRunDump
	// raises it before spawn, the writer goroutine drops it when done) so
	// Close can wait for them — a plain counter, not a WaitGroup: Close
	// may race a prompt that is still finishing, and Add-vs-Wait at a zero
	// counter is exactly what WaitGroup turns into a misuse panic.
	dumpPending atomic.Int64

	mu    sync.Mutex
	turns []TurnStat
	// subUsage aggregates run_subagent token usage per model id — kept
	// out of turn stats because a subagent rides inside a turn but bills
	// separately (often a cheaper model). Cleared with turns on Restore.
	subUsage    map[string]Usage
	pendingNote string // next-boundary role:"system" message (PushSystemNote)

	// mode is the session execution mode (mode.go): plan gates tools,
	// bypass auto-approves. Atomic — the UI goroutine switches while the
	// drive goroutine's preTool/approve gates read it.
	mode atomic.Pointer[Mode]

	// subNest counts in-flight runSubagent executions; OnToolStart/End
	// stamp Event.Sub while >0 so renderers can tell a subagent's inner
	// executions from the main flow. Atomic: subagent tools and parallel
	// read-only tools report concurrently. A main-flow execution that
	// merely overlaps the window may be marked too — a rare, cosmetic
	// trade for not threading parent ids through the runner.
	subNest int32

	// toolUseQ correlates harness executions with their API tool_use
	// block ids: the SDK runner executes blocks without threading the id
	// down to Execute, but tool_use_start events stream in block order
	// before execution — a per-name FIFO registered at stream time pairs
	// each OnToolStart pop with its block (pops are mutex-guarded, so
	// parallel executions each get exactly one id). Subagent-inner
	// executions (subNest>0) never register or pop.
	toolMu   sync.Mutex
	toolUseQ map[string][]string

	// Session intent title (title.go): recognized asynchronously after
	// prompt_end, persisted with snapshots. Guarded by mu.
	title       string
	firstPrompt string
	lastPrompt  string
	// recentPrompts is the rolling tail (≤titleRecentN) of submitted user
	// prompts — the interval summarizer's input (IntervalTitle). Not
	// snapshotted; a restored session falls back to firstPrompt.
	recentPrompts []string

	// uiState is an opaque renderer-side blob (the TUI's steps panel)
	// riding into snapshots so /resume restores the audit trail. The
	// engine never interprets it; SetUIState attaches it before Export.
	// Guarded by mu; cleared by Reset, carried by Restore.
	uiState json.RawMessage

	// uiTodos is the TUI's readable step plan (#66) — same lifecycle as
	// uiState, separate field so the Steps format stays untouched.
	uiTodos json.RawMessage

	// distillHash fingerprints the last rendering the extractor handled
	// successfully (distill.go): exit right after /remember skips the
	// duplicate LLM pass. Guarded by mu.
	distillHash uint64
}

func newSession(e *Engine, obs tools.Observer) *Session {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{
		eng:        e,
		ctx:        ctx,
		cancel:     cancel,
		events:     make(chan Event, e.cfg.EventBuffer),
		reqs:       make(chan submitReq, 1),
		extObs:     obs,
		memoryText: tools.MemoryFacts(e.cfg.Workspace),
		skillsText: skillsSection(),
		shellText:  shellEnvSection(),
	}
	s.streamCtx = context.WithValue(ctx, sessionCtxKey{}, s)
	// D1: save_memory/forget_memory report successes through this seam
	// (tools.WithMemoryChange) so the session can acknowledge them at the
	// next turn boundary — tool handlers reach it via their ctx.
	s.streamCtx = tools.WithMemoryChange(s.streamCtx, s.onMemoryChange)
	adaptCfg := tools.AdapterConfig{
		SerialMu:  &s.serialMu,
		Timeout:   e.cfg.ToolTimeout,
		ResultCap: e.cfg.ToolResultCap,
		Observer:  s,
		Approve:   s.wrapApprove(e.cfg.Approve),
		PreTool:   s.preTool,
		Seq:       &s.seq,
	}
	s.betaTools = e.reg.Adapt(adaptCfg)
	// run_subagent / run_subagents are whole nested tool-loops, not single
	// executions: wrapping them in the wall-clock ToolTimeout killed healthy
	// multi-round research at a fixed second (the plan-mode triple-timeout
	// bug — a 48.9s run lived only because it beat the 120s ceiling). Their
	// no-progress idle watchdog in runNested owns the budget now; every
	// other tool keeps the wall clock.
	adaptCfg.Timeout = 0
	for i, t := range s.betaTools {
		if t.Name() != "run_subagent" && t.Name() != "run_subagents" {
			continue
		}
		if raw, ok := e.reg.Get(t.Name()); ok {
			s.betaTools[i] = tools.Adapt(raw, adaptCfg)
		}
	}
	go s.loop()
	return s
}

// Events returns the session's event stream. It is closed when the session
// is closed. Consumers must drain it while a prompt is running — the buffer
// is bounded and backpressure pauses the driver.
func (s *Session) Events() <-chan Event { return s.events }

// Busy reports whether a prompt is currently running — the read-only peek
// behind the SubmitOn CAS, for status surfaces (web Session.busy) that
// must not perturb the state machine.
func (s *Session) Busy() bool { return s.busy.Load() }

// QueueLen reports how many Enqueue'd prompts are parked in the inbox
// (#2): the count that will join the running conversation at turn gaps.
func (s *Session) QueueLen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inbox)
}

// Submit queues a prompt for execution. It returns nil once the prompt is
// accepted; completion is signaled by EventPromptEnd on Events().
func (s *Session) Submit(prompt string) error {
	return s.SubmitOn(prompt, "")
}

// SubmitOn is Submit with a per-prompt model override: the routing table
// steers this run to its evidenced model while an explicit pin (or no
// evidence) stays on the session's config model (#53 — the TUI's
// consumption side of design §5).
func (s *Session) SubmitOn(prompt, model string) error {
	if s.ctx.Err() != nil {
		return ErrSessionClosed
	}
	if !s.busy.CompareAndSwap(false, true) {
		return ErrPromptInProgress
	}
	select {
	case s.reqs <- submitReq{prompt: prompt, model: model}:
		// Recorded for title intent recognition (title.go): the latest
		// prompt drives the check, the first anchors the session topic,
		// the tail feeds the interval summarizer.
		s.mu.Lock()
		s.lastPrompt = prompt
		if s.firstPrompt == "" {
			s.firstPrompt = prompt
		}
		s.recentPrompts = appendPromptTail(s.recentPrompts, prompt)
		s.mu.Unlock()
		s.eng.hooks.Fire(hooks.Record{Event: hooks.EvPromptStart, Prompt: prompt})
		return nil
	case <-s.ctx.Done():
		s.busy.Store(false)
		return ErrSessionClosed
	}
}

// Enqueue accepts a prompt while another is running (#2 gap injection):
// the line waits in the session inbox and joins the running conversation
// at the next turn boundary instead of after prompt_end — like a queued
// message in Claude Code, the working task sees it mid-flight. Returns
// ErrNoPromptRunning when idle (Submit instead) and ErrSessionClosed
// after Close. Best effort: entries still undelivered when the run ends
// are dropped in drive's exit critical section, so callers keep their own
// copy and re-send it on prompt_end — the TUI's queue does exactly that.
func (s *Session) Enqueue(prompt string) error {
	if s.ctx.Err() != nil {
		return ErrSessionClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.busy.Load() {
		return ErrNoPromptRunning
	}
	s.inbox = append(s.inbox, prompt)
	return nil
}

// injectQueuedGap drains the inbox at a turn gap (#2): each line joins the
// runner's history as its own user turn — the next round appends its
// tool_results right after, which the API combines into one turn (and the
// apiGuard preflights to the same shape) — so the running prompt picks the
// instruction up on its very next API call. The event lets the frontend
// drop its display copy, and prompt_start keeps the audit trail honest.
// Drive goroutine only; never called with the runner nil.
func (s *Session) injectQueuedGap() {
	s.mu.Lock()
	if len(s.inbox) == 0 {
		s.mu.Unlock()
		return
	}
	lines := s.inbox
	s.inbox = nil
	if last := lines[len(lines)-1]; strings.TrimSpace(last) != "" {
		// Latest prompt drives title intent — same rule as SubmitOn.
		s.lastPrompt = last
	}
	// Each injected line is a user prompt (and counts as one in the hook
	// stats below) — the summarizer tail must see them too.
	for _, line := range lines {
		s.recentPrompts = appendPromptTail(s.recentPrompts, line)
	}
	s.mu.Unlock()

	for _, line := range lines {
		s.runner.AppendMessages(anthropic.NewBetaUserMessage(
			tools.PromptBlocks(s.eng.cfg.Workspace, line)...))
		s.eng.hooks.Fire(hooks.Record{
			Event:  hooks.EvPromptStart,
			Prompt: line,
			Detail: "injected at a turn gap (#2)",
		})
		s.emit(Event{Type: EventQueueInjected, Text: line, TS: time.Now()})
	}
}

// Hooks returns the engine's hook manager through the session (audit,
// stats, security gate) — the TUI reads aggregates from here for /stats.
func (s *Session) Hooks() *hooks.Manager { return s.eng.hooks }

// preTool is the security seam handed to every adapted tool (main and
// subagent): plan mode's read-only gate first, then local policy and
// blocking hooks — all before approval.
func (s *Session) preTool(ctx context.Context, name string, input json.RawMessage) error {
	if err := s.modeGate(name, input); err != nil {
		return err
	}
	return s.eng.hooks.CheckTool(ctx, name, input)
}

// wrapApprove brackets the configured approver with audit events so every
// human decision (including "always allow" auto-answers) lands in the log.
// nil stays nil — auto-approve sessions don't fake approvals. Bypass mode
// short-circuits the human to yes, but still writes both audit events:
// an auto-grant must leave a trace. Policy and blocking hooks ran earlier
// in preTool and are NOT weakened by bypass.
func (s *Session) wrapApprove(a tools.ApproveFunc) tools.ApproveFunc {
	if a == nil {
		return nil
	}
	return func(ctx context.Context, e tools.ExecEvent) bool {
		auto := s.Mode() == ModeBypass // never prompt the human just to override them
		s.eng.hooks.Fire(hooks.Record{
			Event:  hooks.EvApproveReq,
			Tool:   e.Name,
			Input:  string(e.Input),
			Detail: detailIf(auto, "bypass mode — no human prompt"),
		})
		ok := auto
		if !auto {
			ok = a(ctx, e)
		}
		s.eng.hooks.Fire(hooks.Record{
			Event:    hooks.EvApproveRes,
			Tool:     e.Name,
			Approved: &ok,
			Detail:   detailIf(auto, "auto-approved by bypass mode"),
		})
		return ok
	}
}

func detailIf(cond bool, d string) string {
	if cond {
		return d
	}
	return ""
}

// Close cancels any in-flight work, stops the session goroutine and closes
// the event stream. Idempotent. Also waits for this session's in-flight
// dump writes (#3) and flushes the engine's audit queue, so the run's tail
// records — dump file and audit lines alike — survive process exit.
func (s *Session) Close() {
	s.closeOne.Do(func() {
		s.cancel()
		for s.dumpPending.Load() > 0 {
			time.Sleep(time.Millisecond) // dump writers are short-lived; mirrors the audit flush below
		}
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
		case r := <-s.reqs:
			s.drive(r.prompt, r.model)
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
			// Frozen base first, then this session's skills block
			// (skills.go) and shell marker — all byte-stable per
			// session, so the cache prefix holds for the whole
			// conversation.
			Text:         s.systemWithMode(cfg.SystemPrompt) + s.skillsText + s.shellText,
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

// shellEnvSection renders the one-line default-shell marker appended after
// the skills block ("Default shell: powershell — write commands in its
// syntax."). tools probes candidates eagerly on first call, so the answer
// is ready before the model produces its first command. "" when no shell
// could be probed — then nothing is injected and Description() stays bare.
func shellEnvSection() string {
	if n := tools.ShellName(); n != "" {
		return fmt.Sprintf("\nDefault shell: %s — write commands in its syntax.\n", n)
	}
	return ""
}

// drive runs one prompt through the full tool loop. Runner rule: exactly one
// goroutine, this one. History rule: the runner is recreated per human
// prompt, seeded with the previous runner's history — byte-identical prefix,
// so the cache keeps hitting — because a runner permanently latches
// completed=true after its final turn.
func (s *Session) drive(prompt, runModel string) {
	// histMu spans the whole prompt so Export/Restore (exit-time snapshot,
	// chat -c) observe one stable history: the caller Interrupts first, then
	// Export waits here until this prompt finishes bookkeeping.
	s.histMu.Lock()
	defer s.histMu.Unlock()
	defer s.busy.Store(false)
	s.iterOffset = 0
	// Wall-clock start of the whole prompt (stream + tools); reported on
	// prompt_end so every frontend can show the total execution time (#43).
	promptStart := time.Now()
	// Open this run's dump recorder (#3): emit feeds it every event below,
	// finishRunDump flushes the closed run to disk at prompt_end.
	s.beginRunDump(prompt, runModel, promptStart)

	// The prompt runs on its own cancellable context so Interrupt can stop
	// just this prompt (stream + tools) while the session survives.
	pctx, pcancel := context.WithCancel(s.streamCtx)
	s.promptCancel.Store(&pcancel)
	defer func() {
		s.promptCancel.CompareAndSwap(&pcancel, nil)
		pcancel()
	}()

	// Undo point: capture the pre-prompt state so /rollback can discard
	// this prompt (and everything after) without touching turn stats.
	s.captureCheckpoint(CheckpointPrompt, clipRunes(strings.TrimSpace(prompt), 64),
		s.currentHistoryLocked())

	hist := s.buildHistory(s.takePendingNote(), prompt)
	if s.memEcho {
		s.memEcho = false
		// Renderers show this as "memory rides into this session" — the
		// one moment the hot tier is visible from the outside.
		s.emit(Event{
			Type:   EventMemoryInjected,
			Detail: fmt.Sprintf("%d bytes of hot memory", len(s.memoryText)),
			Attrs:  map[string]string{"memory": s.memoryText},
		})
	}

	params := s.baseParams(hist)
	// Per-prompt model override (#53 routing consumption): only fires for
	// unpinned runs the routing table has evidence for. Swapping models
	// mid-history costs this conversation's prefix cache — accepted,
	// every swap is backed by ≥3 recorded samples.
	if runModel != "" {
		params.Model = runModel
	}
	s.runner = s.eng.client.Beta.Messages.NewToolRunnerStreaming(s.betaTools, anthropic.BetaToolRunnerParams{
		BetaMessageNewParams: params,
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

		// The gap between rounds (#2): parked prompts join the conversation
		// before this round's tools execute and POST — the model reads them
		// together with the tool results on the very next API call.
		s.injectQueuedGap()

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
				if e.Type == EventToolUseStart && !e.Server {
					s.trackToolUse(e.ToolID, e.ToolName)
				}
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
	repaired := s.repairInterruptedHistory()
	// Prompt-end sweep: the runner can record an empty stream response as a
	// role-less, content-less message (BetaMessage.ToParam of a response
	// with no events). Drop or repair it now, before Export, distill,
	// rollback or the next buildHistory sees or persists it. Drive
	// goroutine, histMu held — no stream is in flight here.
	if s.runner != nil {
		if fixed, ok := sanitizeMessages(s.runner.Params.Messages); ok {
			s.runner.Params.Messages = fixed
			s.auditHistoryRepair("prompt-end sweep")
		}
	}
	budgetStop := ""
	if promptErr == "" && repaired {
		// Clean end that still needed repair: the iteration budget cut the
		// loop before the last turn's tools could run. Renderers surface it
		// instead of showing a silent "done".
		budgetStop = "max_iterations"
	}

	// Release the submit lock and the interrupt handle BEFORE announcing
	// completion: events is buffered, so a consumer can receive prompt_end
	// and Submit the next prompt while this goroutine is still returning —
	// clearing busy after the emit loses that CAS (ErrPromptInProgress).
	s.promptCancel.CompareAndSwap(&pcancel, nil)
	pcancel()
	// Undelivered inbox entries drop here, in the same critical section
	// that releases busy (#2): Enqueue checks busy under mu, so nothing can
	// land after the final gap unnoticed — the caller still holds its copy
	// and re-sends on prompt_end, and no line is ever delivered twice.
	s.mu.Lock()
	s.inbox = nil
	s.busy.Store(false)
	s.mu.Unlock()

	pe := Event{
		Type:       EventPromptEnd,
		Err:        promptErr,
		StopReason: budgetStop,
		Iterations: s.iterOffset + s.runner.IterationCount(),
		DurationMS: time.Since(promptStart).Milliseconds(),
		TS:         time.Now(),
	}
	// Close this run's dump (#3) before announcing: assembly is in-memory,
	// the disk write rides its own goroutine (Close waits it out), and a
	// write failure is only audited (dump_error) — the run's outcome is
	// already settled here, success, failure or interrupt alike.
	s.finishRunDump(pe)
	s.emit(pe)
}

// repairInterruptedHistory queues synthetic error tool_results when the
// history ends mid tool-call: the last message is an assistant tool_use with
// no tool_result following it. The API rejects tool_use blocks without their
// results, so the answers are merged into the next prompt's user message by
// buildHistory. Reports whether anything was repaired — with no prompt error
// in flight that means MaxIterations cut the loop before the last turn's
// tools could run. Runs only on the drive goroutine.
func (s *Session) repairInterruptedHistory() bool {
	if s.runner == nil {
		return false
	}
	msgs := s.runner.Params.Messages
	if len(msgs) == 0 {
		return false
	}
	last := msgs[len(msgs)-1]
	if last.Role != anthropic.BetaMessageParamRoleAssistant {
		return false
	}
	repaired := false
	for _, b := range last.Content {
		if tu := b.OfToolUse; tu != nil {
			res := anthropic.NewBetaToolResultTextBlockParam(
				tu.ID, "prompt interrupted before the tool finished", true)
			s.pendingToolResults = append(s.pendingToolResults,
				anthropic.BetaContentBlockParamUnion{OfToolResult: &res})
			repaired = true
		}
	}
	return repaired
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
	// Mirror the event into the open run's dump recorder (#3) — before the
	// audit filter below, so stream noise lands in the dump too. Absent or
	// already closed: no-op, never blocks.
	if rec := s.dumpRec.Load(); rec != nil {
		rec.add(e)
	}
	// Audit everything except raw stream noise (deltas, message_start,
	// live output chunks): those would drown the log without adding
	// decisions. Fire never blocks.
	switch e.Type {
	case EventTextDelta, EventThinkingDelta, EventMessageStart, EventToolExecOutput,
		EventQueueInjected:
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

// SubUsage returns a copy of the per-model subagent usage accumulated
// so far (empty map when none). Used by the cost gauge.
func (s *Session) SubUsage() map[string]Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Usage, len(s.subUsage))
	for k, v := range s.subUsage {
		out[k] = v
	}
	return out
}

// recordSub folds one subagent run's usage into the per-model totals.
func (s *Session) recordSub(model string, u Usage) {
	s.mu.Lock()
	if s.subUsage == nil {
		s.subUsage = make(map[string]Usage, 1)
	}
	a := s.subUsage[model]
	a.Input += u.Input
	a.Output += u.Output
	a.CacheRead += u.CacheRead
	a.CacheCreation += u.CacheCreation
	s.subUsage[model] = a
	s.mu.Unlock()
}

// trackToolUse records one streamed tool_use block id against its tool
// name so the matching execution can claim it (toolUseQ).
func (s *Session) trackToolUse(id, name string) {
	if id == "" || name == "" || atomic.LoadInt32(&s.subNest) > 0 {
		return
	}
	s.toolMu.Lock()
	if s.toolUseQ == nil {
		s.toolUseQ = map[string][]string{}
	}
	s.toolUseQ[name] = append(s.toolUseQ[name], id)
	s.toolMu.Unlock()
}

// takeToolUse pops the oldest pending block id for name — the execution
// that just started claims its API tool_use id. Returns "" when there is
// nothing to claim (subagent-inner executions, replayed history).
func (s *Session) takeToolUse(name string) string {
	if name == "" || atomic.LoadInt32(&s.subNest) > 0 {
		return ""
	}
	s.toolMu.Lock()
	defer s.toolMu.Unlock()
	q := s.toolUseQ[name]
	if len(q) == 0 {
		return ""
	}
	s.toolUseQ[name] = q[1:]
	return q[0]
}

// OnToolStart / OnToolEnd implement tools.Observer: they translate harness
// execution into session events and forward to the external observer.
func (s *Session) OnToolStart(e tools.ExecEvent) {
	e.ToolID = s.takeToolUse(e.Name)
	s.emit(Event{
		Type:     EventToolExecStart,
		ExecID:   e.ExecID,
		ToolID:   e.ToolID,
		ToolName: e.Name,
		Input:    e.Input,
		Sub:      atomic.LoadInt32(&s.subNest) > 0,
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
		Sub:        atomic.LoadInt32(&s.subNest) > 0,
		TS:         time.Now(),
	})
	if s.extObs != nil {
		s.extObs.OnToolEnd(e)
	}
}

// OnToolOutput implements tools.OutputObserver: one coalesced live
// output chunk rides the event channel to whatever renders the run (the
// TUI's open step shows it in real time, #11). Stream noise like text
// deltas — never audited; forwarded only when the external observer
// opts in too (type-asserted, so plain observers keep compiling).
func (s *Session) OnToolOutput(e tools.ExecEvent) {
	s.emit(Event{
		Type:     EventToolExecOutput,
		ExecID:   e.ExecID,
		ToolName: e.Name,
		Text:     e.Output,
		Sub:      atomic.LoadInt32(&s.subNest) > 0,
	})
	if oo, ok := s.extObs.(tools.OutputObserver); ok {
		oo.OnToolOutput(e)
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

// onMemoryChange is the memory tools' success callback (D1), reached
// through tools.WithMemoryChange on the driving context: it queues the
// one-line acknowledgment for the next turn boundary — 内容可见 via
// buildHistory's re-read, 感知可见 via this note. The callback runs on a
// tool goroutine while drive holds histMu for the whole prompt, so it
// deliberately does NOT write memoryText: taking histMu here would
// deadlock, and a lock-free write would race the next buildHistory.
func (s *Session) onMemoryChange(c tools.MemoryChange) {
	switch c.Action {
	case "save":
		s.PushSystemNote("新记忆已存：" + c.Name)
	case "forget":
		s.PushSystemNote("已忘：" + c.Name)
	}
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
	// D1 hot-tier refresh: memoryText starts as a creation-time snapshot;
	// re-read the store on every prompt (the rollback.go re-read pattern)
	// so a mid-session save_memory / forget_memory — or a distill
	// promotion, which has no callback seam of its own — lands in this
	// round's injected block. Changed text re-arms injection; the
	// containment check below still spares a restored history that
	// already carries the current text. Runs on the drive goroutine with
	// histMu held (tests call it single-threaded), so the field keeps its
	// existing single-writer discipline: no atomics, no extra lock, no
	// serialization of any concurrent path.
	if fresh := tools.MemoryFacts(s.eng.cfg.Workspace); fresh != s.memoryText {
		s.memoryText = fresh
		s.memoryInjected = false
	}
	var hist []anthropic.BetaMessageParam
	switch {
	case s.runner != nil:
		hist = s.runner.Messages()
	case s.seed != nil:
		hist = append([]anthropic.BetaMessageParam(nil), s.seed...)
	}
	// Preflight after every append/merge: a role-less or empty entry in the
	// inherited history would fail the whole request with 400 (the
	// messages[69].role incident) — repair or drop it before the runner is
	// ever built from this history.
	if fixed, ok := sanitizeMessages(hist); ok {
		s.auditHistoryRepair("buildHistory before POST")
		hist = fixed
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
			s.memEcho = true // drive announces it on the event stream
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
