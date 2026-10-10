package openaix

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// streamState accumulates an OpenAI response (streaming chunks or a single
// JSON document) while emitting the Anthropic event sequence the SDK's
// decoder and BetaMessage.Accumulate expect:
//
//	message_start → (content_block_start/delta/stop)* → message_delta → message_stop
//
// Block indexes are strictly sequential from 0 (Accumulate rejects gaps),
// message_delta carries the full usage (overwriting message_start's zeros),
// and the event: line is mandatory — the SDK dispatches on it, silently
// dropping data-only frames.
type streamState struct {
	reqModel string
	id       string
	model    string
	started  bool
	blocks   []*streamBlock
	cur      int // index of the open block; -1 when none
	// lastToolIdx tracks the tool_call index of the previous fragment so
	// gateways that omit `index` on continuation chunks still line up.
	lastToolIdx int
	stopReason  string
	finished    bool
	usage       anthropicUsage
	emit        func(event string, data any) error
}

type streamBlock struct {
	kind     string // "text" | "tool"
	text     string
	toolID   string
	toolName string
	toolOrd  int // this block's tool_call index (delta matching)
	args     string
}

func newStreamState(reqModel string, emit func(event string, data any) error) *streamState {
	return &streamState{reqModel: reqModel, cur: -1, emit: emit}
}

// streamAPIError carries an in-band gateway error frame to the pump
// goroutine, which surfaces it as an Anthropic `event: error`.
type streamAPIError struct {
	payload any
}

func (e *streamAPIError) Error() string {
	b, _ := json.Marshal(e.payload)
	return "openaix: upstream stream error: " + string(b)
}

func (st *streamState) ensureStart() error {
	if st.started {
		return nil
	}
	st.started = true
	if st.model == "" {
		st.model = st.reqModel
	}
	if st.id == "" {
		st.id = msgID("")
	}
	return st.emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            st.id,
			"type":          "message",
			"role":          "assistant",
			"model":         st.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         anthropicUsage{},
		},
	})
}

func (st *streamState) closeCur() error {
	if st.cur < 0 {
		return nil
	}
	idx := st.cur
	st.cur = -1
	return st.emit("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": idx,
	})
}

// onText opens or extends the current text block.
func (st *streamState) onText(s string) error {
	if st.finished || s == "" {
		return nil
	}
	if err := st.ensureStart(); err != nil {
		return err
	}
	if st.cur >= 0 && st.blocks[st.cur].kind == "text" {
		st.blocks[st.cur].text += s
		return st.emitTextDelta(st.cur, s)
	}
	if err := st.closeCur(); err != nil {
		return err
	}
	st.blocks = append(st.blocks, &streamBlock{kind: "text", text: s})
	st.cur = len(st.blocks) - 1
	if err := st.emit("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": st.cur,
		"content_block": map[string]any{
			"type": "text",
			"text": "",
		},
	}); err != nil {
		return err
	}
	return st.emitTextDelta(st.cur, s)
}

func (st *streamState) emitTextDelta(idx int, s string) error {
	return st.emit("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": idx,
		"delta": map[string]any{"type": "text_delta", "text": s},
	})
}

// onTool opens a tool_use block for a new tool_call index or appends an
// arguments fragment to the open one.
func (st *streamState) onTool(idx int, id, name, args string) error {
	if st.finished {
		return nil
	}
	if err := st.ensureStart(); err != nil {
		return err
	}
	if st.cur >= 0 {
		if b := st.blocks[st.cur]; b.kind == "tool" && b.toolOrd == idx {
			b.args += args
			if id != "" {
				b.toolID = id
			}
			if name != "" {
				b.toolName = name
			}
			if args == "" {
				return nil
			}
			return st.emit("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": st.cur,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
			})
		}
	}
	if err := st.closeCur(); err != nil {
		return err
	}
	blk := &streamBlock{kind: "tool", toolOrd: idx, toolID: id, toolName: name, args: args}
	if blk.toolID == "" {
		blk.toolID = fmt.Sprintf("toolu_%s_%d", st.id, idx)
	}
	st.blocks = append(st.blocks, blk)
	st.cur = len(st.blocks) - 1
	if err := st.emit("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": st.cur,
		"content_block": map[string]any{
			"type":  "tool_use",
			"id":    blk.toolID,
			"name":  blk.toolName,
			"input": map[string]any{},
		},
	}); err != nil {
		return err
	}
	if args == "" {
		return nil
	}
	return st.emit("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": st.cur,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
	})
}

func (st *streamState) setFinish(reason string) {
	st.finished = true
	st.stopReason = mapStopReason(reason)
}

// consumeChunk folds one OpenAI streaming chunk into the state, emitting
// translated frames as it goes.
func (st *streamState) consumeChunk(c *oaiChunk) error {
	if st.id == "" && c.ID != "" {
		st.id = msgID(c.ID)
	}
	if st.model == "" && c.Model != "" {
		st.model = c.Model
	}
	if c.Usage != nil {
		st.usage = c.Usage.anthropic()
	}
	if len(c.Choices) == 0 {
		return nil
	}
	ch := c.Choices[0]
	if ch.Delta.Content != nil {
		if err := st.onText(*ch.Delta.Content); err != nil {
			return err
		}
	}
	for _, tc := range ch.Delta.ToolCalls {
		idx := st.lastToolIdx
		if tc.Index != nil {
			idx = *tc.Index
		}
		st.lastToolIdx = idx
		if err := st.onTool(idx, tc.ID, tc.Function.Name, tc.Function.Arguments); err != nil {
			return err
		}
	}
	if ch.FinishReason != nil && *ch.FinishReason != "" {
		st.setFinish(*ch.FinishReason)
	}
	return nil
}

// consumeMessage folds a non-streaming response through the same handlers,
// which lets one state machine serve JSON and SSE in any combination.
func (st *streamState) consumeMessage(r *oaiResponse) error {
	if r == nil {
		return nil
	}
	if st.id == "" && r.ID != "" {
		st.id = msgID(r.ID)
	}
	if st.model == "" && r.Model != "" {
		st.model = r.Model
	}
	if r.Usage != nil {
		st.usage = r.Usage.anthropic()
	}
	if len(r.Choices) == 0 {
		return nil
	}
	ch := r.Choices[0]
	if err := st.onText(extractText(ch.Message.Content)); err != nil {
		return err
	}
	for i, tc := range ch.Message.ToolCalls {
		if err := st.onTool(i, tc.ID, tc.Function.Name, tc.Function.Arguments); err != nil {
			return err
		}
	}
	if ch.FinishReason != nil && *ch.FinishReason != "" {
		st.setFinish(*ch.FinishReason)
	} else {
		st.setFinish("")
	}
	return nil
}

// extractText pulls assistant text out of a response message content that
// may be a string, a part array, or null.
func extractText(c any) string {
	switch v := c.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, el := range v {
			m, ok := el.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := m["type"].(string); t != "text" {
				continue
			}
			if s, _ := m["text"].(string); s != "" {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(s)
			}
		}
		return b.String()
	default:
		return ""
	}
}

// finish closes the stream: empty responses get one empty text block
// (empty content arrays confuse downstream history assembly), open blocks
// stop, then message_delta carries the final stop_reason and the full
// usage, followed by message_stop. Idempotent-guarded by caller usage
// (each transport path calls it exactly once).
func (st *streamState) finish() error {
	if err := st.ensureStart(); err != nil {
		return err
	}
	if err := st.closeCur(); err != nil {
		return err
	}
	if len(st.blocks) == 0 {
		st.blocks = append(st.blocks, &streamBlock{kind: "text"})
		st.cur = 0
		if err := st.emit("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": 0,
			"content_block": map[string]any{
				"type": "text",
				"text": "",
			},
		}); err != nil {
			return err
		}
		if err := st.closeCur(); err != nil {
			return err
		}
	}
	if st.stopReason == "" {
		st.stopReason = "end_turn"
	}
	if err := st.emit("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   st.stopReason,
			"stop_sequence": nil,
		},
		"usage": st.usage,
	}); err != nil {
		return err
	}
	return st.emit("message_stop", map[string]any{"type": "message_stop"})
}

// finalMessage renders the accumulated state as the non-streaming
// Anthropic message document. finish() has already run in every path that
// reaches here.
func (st *streamState) finalMessage() anthropicResponse {
	if st.model == "" {
		st.model = st.reqModel
	}
	if st.id == "" {
		st.id = msgID("")
	}
	stop := st.stopReason
	if stop == "" {
		stop = "end_turn"
	}
	content := make([]any, 0, len(st.blocks))
	if len(st.blocks) == 0 {
		content = append(content, anthropicTextBlock{Type: "text", Text: ""})
	}
	for _, b := range st.blocks {
		if b.kind == "text" {
			content = append(content, anthropicTextBlock{Type: "text", Text: b.text})
			continue
		}
		content = append(content, anthropicToolBlock{
			Type:  "tool_use",
			ID:    b.toolID,
			Name:  b.toolName,
			Input: json.RawMessage(argsString(json.RawMessage(b.args))),
		})
	}
	return anthropicResponse{
		ID:           st.id,
		Type:         "message",
		Role:         "assistant",
		Model:        st.model,
		Content:      content,
		StopReason:   stop,
		StopSequence: nil,
		Usage:        st.usage,
	}
}

// sseWrite returns an emit func that serializes one Anthropic SSE frame
// (event: line + single-line data JSON + blank line) per call.
func sseWrite(w io.Writer) func(event string, data any) error {
	return func(event string, data any) error {
		raw, err := json.Marshal(data)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw)
		return err
	}
}

// translateSSE consumes an OpenAI SSE stream to completion, feeding the
// state machine and emitting translated frames. The upstream body is
// always closed. A clean end — [DONE] or plain EOF after finish_reason —
// runs a graceful finish; a read error or an in-band error frame surfaces
// to the caller instead.
func translateSSE(upstream io.ReadCloser, st *streamState) error {
	defer upstream.Close()
	sc := bufio.NewScanner(upstream)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var data []string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if len(data) == 0 {
				continue
			}
			payload := strings.Join(data, "\n")
			data = data[:0]
			if payload == "[DONE]" {
				return st.finish()
			}
			var c oaiChunk
			if json.Unmarshal([]byte(payload), &c) != nil {
				continue // gateway keep-alive junk: skip, never crash the stream
			}
			if c.Error != nil {
				msg := "upstream stream error"
				if c.Error.Error != nil && c.Error.Error.Message != "" {
					msg = c.Error.Error.Message
				}
				return &streamAPIError{payload: map[string]any{
					"type": "error",
					"error": map[string]string{
						"type":    "api_error",
						"message": msg,
					},
				}}
			}
			if err := st.consumeChunk(&c); err != nil {
				return err
			}
		case strings.HasPrefix(line, ":"):
			// comment / keep-alive line
		default:
			name, val, _ := strings.Cut(line, ":")
			if strings.HasPrefix(val, " ") {
				val = val[1:]
			}
			if name == "data" {
				data = append(data, val)
			}
			// "event:", "id:", "retry:" — the data JSON carries the type.
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	// EOF without [DONE]: gateways that just close after finish_reason
	// still get a graceful close; without finish_reason the response
	// degrades to end_turn rather than failing the whole prompt.
	return st.finish()
}
