package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

func parseEvent(t *testing.T, raw string) anthropic.BetaRawMessageStreamEventUnion {
	t.Helper()
	var ev anthropic.BetaRawMessageStreamEventUnion
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("unmarshal stream event: %v", err)
	}
	return ev
}

func TestNormalizeClientToolUse(t *testing.T) {
	ev := parseEvent(t, `{
		"type":"content_block_start","index":1,
		"content_block":{"type":"tool_use","id":"toolu_1","name":"read_file","input":{}}
	}`)
	got := normalize(ev)
	if got == nil || got.Type != EventToolUseStart || got.Server || got.ToolName != "read_file" {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestNormalizeServerToolUse(t *testing.T) {
	ev := parseEvent(t, `{
		"type":"content_block_start","index":2,
		"content_block":{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search",
			"input":{"query":"golang 1.25 release notes"}}
	}`)
	got := normalize(ev)
	if got == nil {
		t.Fatal("server_tool_use not normalized")
	}
	if got.Type != EventToolUseStart || !got.Server || got.ToolName != "web_search" {
		t.Fatalf("unexpected: %+v", got)
	}
	if !strings.Contains(string(got.Input), "golang 1.25") {
		t.Fatalf("input not carried: %s", got.Input)
	}
}

func TestNormalizeWebSearchToolResult(t *testing.T) {
	ev := parseEvent(t, `{
		"type":"content_block_start","index":3,
		"content_block":{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1",
			"content":[{"type":"web_search_result","url":"https://go.dev/blog/go1.25",
				"title":"Go 1.25 Release Notes","page_age":"2025-08-01"}]}
	}`)
	got := normalize(ev)
	if got == nil {
		t.Fatal("web_search_tool_result not normalized")
	}
	if got.Type != EventServerToolResult || got.IsError {
		t.Fatalf("unexpected: %+v", got)
	}
	if got.ToolName != "web_search" {
		t.Fatalf("tool name = %q", got.ToolName)
	}
	if !strings.Contains(string(got.Input), "go.dev/blog/go1.25") {
		t.Fatalf("result content not carried: %s", got.Input)
	}
	// SSE round-trip: the event must serialize with its payload.
	data, err := json.Marshal(got)
	if err != nil || !strings.Contains(string(data), "go.dev/blog") {
		t.Fatalf("marshal round-trip failed: %v %s", err, data)
	}
}

func TestNormalizeIgnoresPlumbingEvents(t *testing.T) {
	for _, raw := range []string{
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
	} {
		if got := normalize(parseEvent(t, raw)); got != nil {
			t.Fatalf("expected nil for %s, got %+v", raw, got)
		}
	}
}
