package server

// draw_event_map — the model's event-diagram pen (mindmap §14). When the
// user describes a complex event (a handover, an architecture, a process
// with many parties) or asks 画成图/流程图/架构图, the model calls this
// tool with positioned shapes + labeled arrows; the map lands on the
// mindmap canvas through Manager.ApplyDraw and the pane renders it as a
// ProcessOn-style diagram (the user can drag shapes and double-click to
// retext afterwards).
//
// The tool lives here, not in pkg/agent: pkg/mindmap imports pkg/agent
// (events), so an agent-package tool would close the import cycle. This
// package already imports both, and the session rides ctx the same way
// spacetool reads it (agent.SessionFromContext).

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/toolrunner"

	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/mindmap"
	"lyon-agent/pkg/tools"
)

// drawEventMap implements tools.Tool over a typed runner (the flowtool
// pattern): the runner owns Name/Description/InputSchema, Execute
// handles the raw input itself.
type drawEventMap struct {
	inner anthropic.BetaTool
}

// newDrawEventMap builds the tool. Schema generation failure means the
// struct tags are malformed — a developer error, so it panics loudly.
func newDrawEventMap() tools.Tool {
	desc := "Draw a structured event/system diagram onto this session's mindmap. " +
		"Use it when the user describes a complex event — a handover, an architecture, a process " +
		"spanning teams/systems — or asks to 画成图 / 画个图 / 流程图 / 架构图 / 示意图. " +
		"Shapes land on the mindmap canvas at x/y; arrows carry multi-line labels; the user can " +
		"drag shapes and double-click to retext afterwards. Input: optional title; nodes (each: " +
		"id stable latin slug a-z0-9_-, label, shape, x, y top-left px, optional w/h/parent) and " +
		"edges (id optional — defaults to from-to; from/to node ids; label supports \\n line breaks " +
		"for numbered points like '1 …\\n2 …\\n3 …'; style solid|dashed — dashed marks unresolved / " +
		"假设 / 风险). Shapes: rect (default), round, circle, diamond, triangle, pentagon, hexagon, " +
		"star, cloud, speech, note (sticky), text (bare label), parallelogram, trapezoid. Layout: " +
		"left-to-right flow on a ~1600×1200 canvas, ≥60px gaps between shapes, hang edge notes on " +
		"the arrow. Labels may repeat — a diagram names the same system twice on purpose. Every " +
		"drawn node is a proposal until the user confirms it on the card."
	inner, err := toolrunner.NewBetaToolFromJSONSchema[mindmap.DrawSpec](
		"draw_event_map", desc,
		func(_ context.Context, _ mindmap.DrawSpec) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			// Unreachable: Execute below handles the raw input itself. The
			// runner factory requires a handler; the adapter never calls it.
			return anthropic.BetaToolResultBlockParamContentUnion{}, fmt.Errorf("draw_event_map: use Execute")
		},
	)
	if err != nil {
		panic(fmt.Sprintf("server: draw_event_map schema: %v", err))
	}
	return &drawEventMap{inner: inner}
}

func (t *drawEventMap) Name() string { return t.inner.Name() }

func (t *drawEventMap) Description() string { return t.inner.Description() }

func (t *drawEventMap) InputSchema() anthropic.BetaToolInputSchemaParam {
	return t.inner.InputSchema()
}

// ParallelSafe is false: ApplyDraw mutates the seat's map (serialized
// with every other map write behind the Manager mutex, but the tool
// should not race its own siblings either).
func (t *drawEventMap) ParallelSafe() bool { return false }

// Execute applies the spec through the same Manager every other writer
// uses — authority rules, caps and persistence included.
func (t *drawEventMap) Execute(ctx context.Context, input json.RawMessage) ([]anthropic.BetaToolResultBlockParamContentUnion, error) {
	var spec mindmap.DrawSpec
	if err := json.Unmarshal(input, &spec); err != nil {
		return nil, fmt.Errorf("invalid input: %w", err)
	}
	sess := agent.SessionFromContext(ctx)
	if sess == nil {
		return nil, fmt.Errorf("draw_event_map: no session in context")
	}
	if mindmap.Off() {
		return nil, fmt.Errorf("mindmap is off (LYON_MINDMAP=off)")
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	res, err := mindmap.Open().ApplyDraw(sess.SeatID(), string(specJSON))
	if err != nil {
		return nil, err
	}
	return tools.TextResult(fmt.Sprintf(
		"已画入 mindmap：%d 个图形、%d 条连线（revision %d）。用户可在 mindmap 面板拖动调整位置、双击改文字；新图形是 proposed 状态，等用户确认。",
		len(spec.Nodes), len(spec.Edges), res.Revision,
	)), nil
}
