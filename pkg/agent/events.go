package agent

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// EventType identifies an Event. The set is the stable contract between the
// engine and its renderers (CLI terminal, SSE server).
type EventType string

const (
	EventMessageStart     EventType = "message_start"
	EventTextDelta        EventType = "text_delta"
	EventThinkingDelta    EventType = "thinking_delta"
	EventToolUseStart     EventType = "tool_use_start"     // model emitted a tool call
	EventToolExecStart    EventType = "tool_exec_start"    // harness began executing it
	EventToolExecEnd      EventType = "tool_exec_end"      // execution finished
	EventServerToolResult EventType = "server_tool_result" // server-side tool (web_search/fetch) returned results
	EventCompaction       EventType = "compaction"         // context compaction triggered / completed
	EventSubagentStart    EventType = "subagent_start"     // one dispatched subtask begins (#53 fan-out; carries SubID)
	EventSubagentEnd      EventType = "subagent_end"       // run_subagent / dispatched subtask finished (independent usage line)
	EventMemoryInjected   EventType = "memory_injected"    // hot memory block rode into the first message
	EventTurnEnd          EventType = "turn_end"           // one API turn finished (usage here)
	EventPromptEnd        EventType = "prompt_end"         // whole tool loop for one prompt finished
	EventError            EventType = "error"
)

// Usage is the per-turn token accounting, including cache activity.
type Usage struct {
	Input         int64 `json:"input"`
	Output        int64 `json:"output"`
	CacheRead     int64 `json:"cache_read"`
	CacheCreation int64 `json:"cache_creation"`
}

// Event is one item on a session's event stream. Fields are omitempty so the
// same struct serializes cleanly as SSE data later.
type Event struct {
	Type EventType `json:"type"`

	// Text / Thinking carry stream deltas.
	Text     string `json:"text,omitempty"`
	Thinking string `json:"thinking,omitempty"`

	// Tool identification: ToolID is the API tool_use id (tool_use_start),
	// ExecID is the harness-local execution sequence (tool_exec_*).
	ToolID   string          `json:"tool_id,omitempty"`
	ExecID   uint64          `json:"exec_id,omitempty"`
	ToolName string          `json:"tool_name,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
	// Server marks a tool_use the Anthropic API executes server-side
	// (web_search, web_fetch): no harness execution events follow it.
	Server bool `json:"server,omitempty"`
	// Sub marks a harness execution that happened inside a run_subagent
	// call — renderers nest these under the subagent's task instead of
	// mixing them into the main flow (exec events only; the subagent's
	// own loop has no tool_use_start on this stream).
	Sub bool `json:"sub,omitempty"`
	// SubID disambiguates concurrent subtask runs of run_subagents (#53):
	// events carrying the same token belong to the same task node, so a
	// fan-out never conflates attribution ("" = the legacy single call,
	// attributed to the one active task).
	SubID string `json:"sub_id,omitempty"`

	// DurationMS / IsError / Err describe tool executions and failures.
	DurationMS int64  `json:"duration_ms,omitempty"`
	IsError    bool   `json:"is_error,omitempty"`
	Err        string `json:"error,omitempty"`

	// Turn / StopReason / Usage describe turn boundaries.
	Turn       int    `json:"turn,omitempty"`
	StopReason string `json:"stop_reason,omitempty"`
	Usage      *Usage `json:"usage,omitempty"`

	// Prompt metadata on prompt_end.
	Iterations int       `json:"iterations,omitempty"`
	TS         time.Time `json:"ts,omitempty"`

	// Detail carries a human-readable note on compaction/subagent events.
	Detail string `json:"detail,omitempty"`
}

// normalize converts one raw SDK stream event into an engine Event, or nil
// when the raw event carries nothing user-facing (block stops, signatures,
// partial JSON, message_delta plumbing).
func normalize(ev anthropic.BetaRawMessageStreamEventUnion) *Event {
	switch ev.Type {
	case "message_start":
		return &Event{Type: EventMessageStart}

	case "content_block_start":
		cb := ev.AsContentBlockStart().ContentBlock
		switch cb.Type {
		case "tool_use":
			return &Event{Type: EventToolUseStart, ToolID: cb.ID, ToolName: cb.Name}
		case "server_tool_use":
			// Anthropic executes these; the query is visible, results
			// arrive as a follow-up content block. Input is typed `any` in
			// the stream union, so round-trip it through Marshal.
			input, err := json.Marshal(cb.Input)
			if err != nil {
				input = nil
			}
			return &Event{
				Type:     EventToolUseStart,
				ToolID:   cb.ID,
				ToolName: cb.Name,
				Input:    input,
				Server:   true,
			}
		case "web_search_tool_result", "web_fetch_tool_result":
			payload, err := json.Marshal(cb.Content)
			if err != nil {
				payload = nil
			}
			return &Event{
				Type:     EventServerToolResult,
				ToolID:   cb.ToolUseID,
				ToolName: strings.TrimSuffix(cb.Type, "_tool_result"),
				Input:    payload,
				IsError:  cb.IsError,
			}
		}
		return nil

	case "content_block_delta":
		d := ev.AsContentBlockDelta().Delta
		switch d.Type {
		case "text_delta":
			if d.Text == "" {
				return nil
			}
			return &Event{Type: EventTextDelta, Text: d.Text}
		case "thinking_delta":
			if d.Thinking == "" {
				return nil
			}
			return &Event{Type: EventThinkingDelta, Thinking: d.Thinking}
		}
		return nil

	default:
		return nil
	}
}
