// Package tools defines the tool abstraction for the lyon agent engine:
// the Tool interface, the per-session adapter that wraps tools with policy
// (serial execution, timeouts, result capping, observability), and the
// built-in tools.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"lyon-agent/pkg/logx"
)

// Tool is a capability the model can invoke through the agent loop.
type Tool interface {
	Name() string
	Description() string
	InputSchema() anthropic.BetaToolInputSchemaParam
	// ParallelSafe reports whether this tool may run concurrently with other
	// tools in the same turn. Tools that mutate shared state return false and
	// are serialized per session by the adapter.
	ParallelSafe() bool
	Execute(ctx context.Context, input json.RawMessage) ([]anthropic.BetaToolResultBlockParamContentUnion, error)
}

// ExecEvent describes one tool execution, reported to an Observer at start
// and again at end (with Duration and Err filled in).
type ExecEvent struct {
	ExecID       uint64
	Name         string
	Input        json.RawMessage
	ParallelSafe bool
	Start        time.Time
	Duration     time.Duration
	Err          string
	// ToolID is the API tool_use block id, stamped by the Observer when
	// it can correlate the execution with its block (the SDK runner does
	// not thread the id down to Execute). Empty when unknown — renderers
	// then fall back to appending at the tail.
	ToolID string
	// Output carries one live output chunk — only on OnToolOutput
	// callbacks (streaming tools, #11); start/end events leave it empty.
	Output string
}

// Observer receives tool execution events. Callbacks run on the goroutine
// that executes the tool and must not block for long.
type Observer interface {
	OnToolStart(e ExecEvent)
	OnToolEnd(e ExecEvent)
}

// OutputObserver is an optional Observer extension: implementors that
// want live execution output (the TUI's open-step terminal view, #11)
// receive coalesced chunks WHILE the tool runs. Type-asserted, so plain
// Observer implementations — and third-party ones — keep compiling.
type OutputObserver interface {
	OnToolOutput(e ExecEvent)
}

// WithOutputSink threads a live-output callback through one tool call's
// context: the adapter installs it when the session observer opts in via
// OutputObserver, and streaming tools (shell) publish to it. fn is
// called from the tool's goroutine and coalescing timer goroutines —
// implementations must not block. fn == nil returns ctx unchanged.
func WithOutputSink(ctx context.Context, fn func(string)) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, outputSinkKey{}, fn)
}

// OutputSink returns the live-output callback the adapter installed, or
// nil when nobody is listening (probes, metrics-only sessions).
func OutputSink(ctx context.Context) func(string) {
	fn, _ := ctx.Value(outputSinkKey{}).(func(string))
	return fn
}

type outputSinkKey struct{}

// ObserverFuncs adapts plain functions to the Observer interface.
type ObserverFuncs struct {
	Start  func(ExecEvent)
	End    func(ExecEvent)
	Output func(ExecEvent) // optional: live chunks, when set
}

func (o ObserverFuncs) OnToolStart(e ExecEvent) {
	if o.Start != nil {
		o.Start(e)
	}
}
func (o ObserverFuncs) OnToolEnd(e ExecEvent) {
	if o.End != nil {
		o.End(e)
	}
}
func (o ObserverFuncs) OnToolOutput(e ExecEvent) {
	if o.Output != nil {
		o.Output(e)
	}
}

// Approvable is an optional interface a Tool can implement to participate in
// the approval hook. Only tools whose execution mutates state or leaves the
// workspace should return true.
type Approvable interface {
	NeedsApproval() bool
}

// ApproveFunc decides whether one execution may proceed. It runs on the
// goroutine that would execute the tool, before any serial lock or timeout
// is taken. Returning false turns the call into an is_error tool result
// ("Error: user declined") so the model can adapt.
type ApproveFunc func(ctx context.Context, e ExecEvent) bool

// AdapterConfig carries the per-session policy applied when wrapping tools.
type AdapterConfig struct {
	// SerialMu is shared by every serial tool of one session so mutating
	// tools never overlap within the session. Parallel-safe tools never take it.
	SerialMu *sync.Mutex
	// Timeout bounds a single tool execution. Zero means no bound.
	Timeout time.Duration
	// ResultCap truncates text results to this many bytes (0 = no cap).
	ResultCap int
	// Observer receives execution events; may be nil.
	Observer Observer
	// Approve gates tools that implement Approvable; nil means auto-approve.
	Approve ApproveFunc
	// PreTool is the security seam: it runs after the exec-start observer
	// event but BEFORE approval, so a denied invocation never reaches the
	// human or the workspace. A returned error fails the tool (the model
	// sees it as the tool result); nil means no gate.
	PreTool func(ctx context.Context, name string, input json.RawMessage) error
	// Seq, when set, is the shared ExecID counter for the whole session so
	// ids are unique across tools (nil falls back to a per-tool counter).
	Seq *atomic.Uint64
}

// Adapt wraps a Tool as an anthropic.BetaTool, applying the session policy:
// approval hooks, the session serial lock for non-parallel-safe tools, an
// execution timeout, result capping, panic recovery, and observer events.
func Adapt(t Tool, cfg AdapterConfig) anthropic.BetaTool {
	return &adaptedTool{t: t, cfg: cfg}
}

type adaptedTool struct {
	t       Tool
	cfg     AdapterConfig
	execSeq atomic.Uint64
}

func (a *adaptedTool) Name() string        { return a.t.Name() }
func (a *adaptedTool) Description() string { return a.t.Description() }

// InputSchema returns the schema in canonical, byte-stable form — see
// stableSchema. Every adapted tool goes through here, so the tools array
// marshals identically on every request and the prompt cache keeps hitting.
func (a *adaptedTool) InputSchema() anthropic.BetaToolInputSchemaParam {
	return stableSchema(a.t.InputSchema())
}

func (a *adaptedTool) Execute(ctx context.Context, input json.RawMessage) (content []anthropic.BetaToolResultBlockParamContentUnion, err error) {
	seq := &a.execSeq
	if a.cfg.Seq != nil {
		seq = a.cfg.Seq
	}
	ev := ExecEvent{
		ExecID:       seq.Add(1),
		Name:         a.t.Name(),
		Input:        input,
		ParallelSafe: a.t.ParallelSafe(),
		Start:        time.Now(),
	}
	if a.cfg.Observer != nil {
		a.cfg.Observer.OnToolStart(ev)
	}
	// Live output (#11): when the observer opts in, stream chunks ride
	// the ctx down to the tool — no observer plumbing inside tools, and
	// an observer that doesn't care never gets the sink at all.
	if oo, ok := a.cfg.Observer.(OutputObserver); ok {
		ctx = WithOutputSink(ctx, func(text string) {
			oo.OnToolOutput(ExecEvent{ExecID: ev.ExecID, Name: ev.Name, Output: text})
		})
	}
	defer func() {
		r := recover()
		if r != nil {
			content = nil
			err = fmt.Errorf("tool panic: %v", r)
		}
		ev.Duration = time.Since(ev.Start)
		if err != nil {
			ev.Err = err.Error()
			// Run-log seam (#9): every failed or panicking tool execution
			// leaves a trace, whatever the caller does with the error.
			if r != nil {
				logx.Error("tools", "tool panicked", "tool", ev.Name, "duration_ms", ev.Duration.Milliseconds(), "panic", fmt.Sprint(r))
			} else {
				logx.Error("tools", "tool execution failed", "tool", ev.Name, "duration_ms", ev.Duration.Milliseconds(), "err", err)
			}
		}
		if a.cfg.Observer != nil {
			a.cfg.Observer.OnToolEnd(ev)
		}
	}()

	// Security gate first: local policy and blocking hooks decide before
	// the human is bothered — a blocked tool must never prompt for approval.
	if a.cfg.PreTool != nil {
		if err := a.cfg.PreTool(ctx, ev.Name, input); err != nil {
			return nil, err
		}
	}

	// Approval runs before the serial lock: prompting a human must not hold
	// the session's mutating-tool mutex. Denial becomes an is_error result.
	if a.cfg.Approve != nil {
		if p, ok := a.t.(Approvable); ok && p.NeedsApproval() && !a.cfg.Approve(ctx, ev) {
			return nil, errors.New("user declined")
		}
	}

	// Serialize mutating tools within the session. The lock is taken before
	// the timeout so queueing time does not eat the execution budget.
	if !a.t.ParallelSafe() && a.cfg.SerialMu != nil {
		a.cfg.SerialMu.Lock()
		defer a.cfg.SerialMu.Unlock()
	}
	if a.cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.cfg.Timeout)
		defer cancel()
	}

	content, err = a.t.Execute(ctx, input)
	if err != nil {
		return nil, err
	}
	return capResults(content, a.cfg.ResultCap), nil
}

// capResults truncates oversized text blocks, keeping a marker so the model
// knows content was elided.
func capResults(content []anthropic.BetaToolResultBlockParamContentUnion, limit int) []anthropic.BetaToolResultBlockParamContentUnion {
	if limit <= 0 {
		return content
	}
	for i := range content {
		if content[i].OfText == nil {
			continue
		}
		text := content[i].OfText.Text
		if len(text) <= limit {
			continue
		}
		kept := text[:limit]
		marker := fmt.Sprintf("\n... [truncated: %d of %d bytes shown]", limit, len(text))
		cp := *content[i].OfText
		cp.Text = kept + marker
		content[i].OfText = &cp
	}
	return content
}

// TextResult builds a single text result block — the standard tool output.
func TextResult(s string) []anthropic.BetaToolResultBlockParamContentUnion {
	return []anthropic.BetaToolResultBlockParamContentUnion{
		{OfText: &anthropic.BetaTextBlockParam{Text: s}},
	}
}

// ImageResult builds a single image result block — how a tool hands a
// picture (screenshot, chart, photo) to the model. data is standard base64;
// mediaType must be image/png, image/jpeg, image/gif or image/webp.
func ImageResult(mediaType, data string) []anthropic.BetaToolResultBlockParamContentUnion {
	return []anthropic.BetaToolResultBlockParamContentUnion{
		{OfImage: &anthropic.BetaImageBlockParam{
			Source: anthropic.BetaImageBlockParamSourceUnion{
				OfBase64: &anthropic.BetaBase64ImageSourceParam{
					MediaType: anthropic.BetaBase64ImageSourceMediaType(mediaType),
					Data:      data,
				},
			},
		}},
	}
}
