package openaix

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// rtFunc adapts a function to http.RoundTripper for passthrough tests.
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// frame is one parsed Anthropic SSE frame.
type frame struct {
	Event string
	Data  string
}

func parseFrames(t *testing.T, body []byte) []frame {
	t.Helper()
	var out []frame
	blocks := strings.Split(string(body), "\n\n")
	for _, b := range blocks {
		if strings.TrimSpace(b) == "" {
			continue
		}
		f := frame{}
		for _, line := range strings.Split(b, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				f.Event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				f.Data = strings.TrimPrefix(line, "data: ")
			}
		}
		if f.Event == "" && f.Data == "" {
			continue
		}
		out = append(out, f)
	}
	return out
}

func frameData(t *testing.T, f frame) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(f.Data), &m); err != nil {
		t.Fatalf("frame %q data: %v", f.Event, err)
	}
	return m
}

// TestTranslateRequest pins the request-side mapping: system extraction,
// image data URLs, thinking dropped, tool results promoted to role:"tool"
// (errors prefixed), consecutive user messages merged, server tools skipped,
// tool_choice and stream_options translated.
func TestTranslateRequest(t *testing.T) {
	req := &anthropicRequest{
		Model:     "m1",
		MaxTokens: 1234,
		Stream:    true,
		System: json.RawMessage(
			`[{"type":"text","text":"SYS","cache_control":{"type":"ephemeral"}}]`),
		Messages: []anthropicMessage{
			{Role: "user", Content: json.RawMessage(
				`[{"type":"text","text":"hi"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAA"}}]`)},
			{Role: "assistant", Content: json.RawMessage(
				`[{"type":"text","text":"ok"},{"type":"thinking","thinking":"secret"},{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"a.txt"}}]`)},
			{Role: "user", Content: json.RawMessage(
				`[{"type":"tool_result","tool_use_id":"toolu_1","content":"DATA","is_error":true},{"type":"text","text":"next"}]`)},
			{Role: "user", Content: json.RawMessage(`"second prompt"`)},
			{Role: "system", Content: json.RawMessage(
				`[{"type":"text","text":"a note at the boundary"}]`)},
		},
		Tools: []anthropicTool{
			{Name: "read_file", Description: "d",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)},
			{Name: "", InputSchema: nil}, // server-side tool variant: skipped
		},
		ToolChoice:    json.RawMessage(`{"type":"auto","disable_parallel_tool_use":false}`),
		StopSequences: []string{"STOP"},
	}

	o := translateRequest(req)
	if o.Model != "m1" || o.MaxTokens != 1234 || !o.Stream {
		t.Fatalf("header: %+v", o)
	}
	if o.StreamOptions == nil || !o.StreamOptions.IncludeUsage {
		t.Fatal("stream_options.include_usage must be set when streaming")
	}
	if len(o.Stop) != 1 || o.Stop[0] != "STOP" {
		t.Fatalf("stop: %v", o.Stop)
	}
	if len(o.Tools) != 1 || o.Tools[0].Function.Name != "read_file" {
		t.Fatalf("tools: %+v", o.Tools)
	}
	if string(o.Tools[0].Function.Parameters) != `{"type":"object","properties":{"path":{"type":"string"}}}` {
		t.Fatalf("parameters not verbatim: %s", o.Tools[0].Function.Parameters)
	}
	if o.ToolChoice != "auto" {
		t.Fatalf("tool_choice: %v", o.ToolChoice)
	}

	if len(o.Messages) != 6 {
		t.Fatalf("messages = %d, want 6 (system, user, assistant, tool, merged-user, system-note): %+v",
			len(o.Messages), o.Messages)
	}
	if o.Messages[0].Role != "system" || o.Messages[0].Content != "SYS" {
		t.Fatalf("first message: %+v", o.Messages[0])
	}

	parts, ok := o.Messages[1].Content.([]oaiPart)
	if !ok || len(parts) != 2 || parts[0].Text != "hi" ||
		parts[1].Type != "image_url" ||
		parts[1].ImageURL == nil || parts[1].ImageURL.URL != "data:image/png;base64,AAA" {
		t.Fatalf("user parts: %#v", o.Messages[1].Content)
	}

	asst := o.Messages[2]
	if asst.Role != "assistant" || asst.Content != "ok" || len(asst.ToolCalls) != 1 {
		t.Fatalf("assistant: %+v", asst)
	}
	if asst.ToolCalls[0].ID != "toolu_1" || asst.ToolCalls[0].Function.Name != "read_file" ||
		asst.ToolCalls[0].Function.Arguments != `{"path":"a.txt"}` {
		t.Fatalf("tool call: %+v", asst.ToolCalls[0])
	}

	tool := o.Messages[3]
	if tool.Role != "tool" || tool.ToolCallID != "toolu_1" || tool.Content != "[tool error] DATA" {
		t.Fatalf("tool message: %+v", tool)
	}

	// "next" and "second prompt" were adjacent user messages: merged.
	merged := o.Messages[4]
	if merged.Role != "user" || merged.Content != "next\n\nsecond prompt" {
		t.Fatalf("merged user: %+v", merged)
	}
	// The trailing system note survives at its position (not folded into
	// the top-level system prompt).
	if o.Messages[5].Role != "system" || o.Messages[5].Content != "a note at the boundary" {
		t.Fatalf("boundary system note: %+v", o.Messages[5])
	}
}

// TestRoundTripNonStream: a raw Anthropic request comes back as a valid
// Anthropic message, with the wire flipped to OpenAI (path, Bearer auth,
// message shapes) on the way out.
func TestRoundTripNonStream(t *testing.T) {
	var gotPath, gotMethod, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod, gotAuth = r.URL.Path, r.Method, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req-42")
		w.Header().Set("Retry-After", "7")
		_, _ = io.WriteString(w, `{
			"id":"chatcmpl-abc","model":"gpt-x",
			"choices":[{"index":0,"message":{"role":"assistant","content":null,
				"tool_calls":[{"id":"call_1","type":"function",
					"function":{"name":"read_file","arguments":"{\"path\":\"a.txt\"}"}}]},
				"finish_reason":"tool_calls"}],
			"usage":{"prompt_tokens":11,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":5}}}`)
	}))
	defer srv.Close()

	client := &http.Client{Transport: &Transport{Base: srv.URL}}
	body := `{"model":"claude-x","max_tokens":1000,
		"system":[{"type":"text","text":"SYS"}],
		"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],
		"tools":[{"name":"read_file","description":"d","input_schema":{"type":"object"}}],
		"stream":false}`
	req, _ := http.NewRequest(http.MethodPost, "http://anthropic.invalid/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-abc")
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if gotPath != "/chat/completions" || gotMethod != http.MethodPost {
		t.Fatalf("upstream saw %s %s", gotMethod, gotPath)
	}
	if gotAuth != "Bearer sk-abc" {
		t.Fatalf("auth: %q", gotAuth)
	}
	if resp.StatusCode != 200 || resp.Header.Get("X-Request-Id") != "req-42" {
		t.Fatalf("status=%d request-id=%q", resp.StatusCode, resp.Header.Get("X-Request-Id"))
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("content-type: %q", resp.Header.Get("Content-Type"))
	}

	// Outgoing request shape.
	if gotBody["model"] != "claude-x" || gotBody["max_tokens"].(float64) != 1000 {
		t.Fatalf("header fields: %v", gotBody)
	}
	msgs := gotBody["messages"].([]any)
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "SYS" {
		t.Fatalf("first message: %v", first)
	}
	if gotBody["tool_choice"] != "auto" {
		t.Fatalf("tool_choice: %v", gotBody["tool_choice"])
	}
	if _, leaked := gotBody["cache_control"]; leaked {
		t.Fatal("anthropic-only cache_control leaked upstream")
	}
	tool0 := gotBody["tools"].([]any)[0].(map[string]any)
	fn := tool0["function"].(map[string]any)
	if tool0["type"] != "function" || fn["name"] != "read_file" {
		t.Fatalf("tool: %v", tool0)
	}

	// Back-translated Anthropic message.
	var msg anthropicResponse
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("response: %v\n%s", err, raw)
	}
	if msg.ID != "msg_abc" || msg.Type != "message" || msg.Role != "assistant" ||
		msg.Model != "gpt-x" || msg.StopReason != "tool_use" {
		t.Fatalf("envelope: %+v", msg)
	}
	if len(msg.Content) != 1 {
		t.Fatalf("content: %+v", msg.Content)
	}
	blk, ok := msg.Content[0].(map[string]any)
	if !ok || blk["type"] != "tool_use" || blk["id"] != "call_1" || blk["name"] != "read_file" {
		t.Fatalf("block: %v", msg.Content[0])
	}
	if input, _ := blk["input"].(map[string]any); input["path"] != "a.txt" {
		t.Fatalf("input: %v", blk["input"])
	}
	if msg.Usage.InputTokens != 11 || msg.Usage.OutputTokens != 7 ||
		msg.Usage.CacheReadInputTokens != 5 {
		t.Fatalf("usage: %+v", msg.Usage)
	}
}

// TestRoundTripStream: OpenAI chunks become the exact Anthropic SSE frame
// sequence the SDK decodes and accumulates (event: lines included — the
// SDK dispatches on them).
func TestRoundTripStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		chunks := []string{
			`{"id":"chatcmpl-s1","model":"gpt-x","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
			`{"id":"chatcmpl-s1","choices":[{"index":0,"delta":{"content":"Hello "}}]}`,
			`{"id":"chatcmpl-s1","choices":[{"index":0,"delta":{"content":"world"}}]}`,
			`{"id":"chatcmpl-s1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":4}}}`,
			`[DONE]`,
		}
		for _, c := range chunks {
			_, _ = io.WriteString(w, "data: "+c+"\n\n")
		}
	}))
	defer srv.Close()

	client := &http.Client{Transport: &Transport{Base: srv.URL}}
	body := `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":true}`
	resp, err := client.Post("http://anthropic.invalid/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	frames := parseFrames(t, raw)
	wantEvents := []string{
		"message_start", "content_block_start", "content_block_delta",
		"content_block_delta", "content_block_stop", "message_delta", "message_stop",
	}
	if len(frames) != len(wantEvents) {
		t.Fatalf("frames = %d, want %d:\n%s", len(frames), len(wantEvents), raw)
	}
	for i, want := range wantEvents {
		if frames[i].Event != want {
			t.Fatalf("frame[%d] = %q, want %q\n%s", i, frames[i].Event, want, raw)
		}
	}
	start := frameData(t, frames[0])
	msg := start["message"].(map[string]any)
	if msg["id"] != "msg_s1" || msg["model"] != "gpt-x" || msg["role"] != "assistant" {
		t.Fatalf("message_start.message: %v", msg)
	}
	// usage at start is zeros — message_delta overwrites it.
	startUsage := msg["usage"].(map[string]any)
	if startUsage["input_tokens"].(float64) != 0 {
		t.Fatalf("start usage: %v", startUsage)
	}
	d1 := frameData(t, frames[2])
	if d1["delta"].(map[string]any)["text"] != "Hello " || d1["index"].(float64) != 0 {
		t.Fatalf("text delta: %v", d1)
	}
	delta := frameData(t, frames[5])
	if delta["delta"].(map[string]any)["stop_reason"] != "end_turn" {
		t.Fatalf("message_delta: %v", delta)
	}
	u := delta["usage"].(map[string]any)
	if u["input_tokens"].(float64) != 9 || u["output_tokens"].(float64) != 3 ||
		u["cache_read_input_tokens"].(float64) != 4 {
		t.Fatalf("final usage: %v", u)
	}
}

// TestRoundTripStreamTool: fragmented arguments and parallel tool_calls
// reconstruct into sequential content blocks with the SDK's
// input_json_delta accumulation rules (start input {}, append fragments).
func TestRoundTripStreamTool(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		chunks := []string{
			`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"ls","arguments":"{\"p\":"}}]}}]}`,
			`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"/\"}"}}]}}]}`,
			`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","function":{"name":"pwd","arguments":"{}"}}]}}]}`,
			`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":6}}`,
			`[DONE]`,
		}
		for _, c := range chunks {
			_, _ = io.WriteString(w, "data: "+c+"\n\n")
		}
	}))
	defer srv.Close()

	client := &http.Client{Transport: &Transport{Base: srv.URL}}
	body := `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"list"}],"stream":true}`
	resp, err := client.Post("http://anthropic.invalid/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	frames := parseFrames(t, raw)

	var events []string
	for _, f := range frames {
		events = append(events, f.Event)
	}
	want := []string{
		"message_start",
		"content_block_start", "content_block_delta", "content_block_delta",
		"content_block_stop",
		"content_block_start", "content_block_delta",
		"content_block_stop",
		"message_delta", "message_stop",
	}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("events:\n got %v\nwant %v\nraw:\n%s", events, want, raw)
	}

	// Block 0: tool_use call_a, fragments concatenated to {"p":"/"}.
	b0 := frameData(t, frames[1])["content_block"].(map[string]any)
	if b0["type"] != "tool_use" || b0["id"] != "call_a" || b0["name"] != "ls" {
		t.Fatalf("block0 start: %v", b0)
	}
	frag1 := frameData(t, frames[2])["delta"].(map[string]any)["partial_json"]
	frag2 := frameData(t, frames[3])["delta"].(map[string]any)["partial_json"]
	if frag1.(string)+frag2.(string) != `{"p":"/"}` {
		t.Fatalf("fragments: %q + %q", frag1, frag2)
	}
	// Block 1: tool_use call_b at index 1.
	b1 := frameData(t, frames[5])["content_block"].(map[string]any)
	if b1["id"] != "call_b" || b1["name"] != "pwd" {
		t.Fatalf("block1 start: %v", b1)
	}
	if idx := frameData(t, frames[5])["index"].(float64); idx != 1 {
		t.Fatalf("block1 index: %v", idx)
	}
	// finish_reason tool_calls → stop_reason tool_use.
	if sr := frameData(t, frames[8])["delta"].(map[string]any)["stop_reason"]; sr != "tool_use" {
		t.Fatalf("stop_reason: %v", sr)
	}
}

// TestRoundTripError: HTTP status and retry hints survive; the body comes
// back in Anthropic error shape so SDK error typing works.
func TestRoundTripError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"slow down","type":"rate_limit_error"}}`)
	}))
	defer srv.Close()

	client := &http.Client{Transport: &Transport{Base: srv.URL}}
	body := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := client.Post("http://anthropic.invalid/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") != "7" {
		t.Fatalf("retry-after: %q", resp.Header.Get("Retry-After"))
	}
	var e struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatalf("body: %v\n%s", err, raw)
	}
	if e.Type != "error" || e.Error.Type != "rate_limit_error" || e.Error.Message != "slow down" {
		t.Fatalf("error doc: %s", raw)
	}
}

// TestJSONWhenStreamingRequested: a gateway that ignores stream:true still
// works — the JSON answer is re-rendered as a complete Anthropic SSE
// sequence.
func TestJSONWhenStreamingRequested(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"chatcmpl-j","model":"gpt-x",
			"choices":[{"index":0,"message":{"role":"assistant","content":"plain text"},
				"finish_reason":"stop"}],
			"usage":{"prompt_tokens":2,"completion_tokens":2}}`)
	}))
	defer srv.Close()

	client := &http.Client{Transport: &Transport{Base: srv.URL}}
	body := `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":true}`
	resp, err := client.Post("http://anthropic.invalid/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("content-type: %q", resp.Header.Get("Content-Type"))
	}
	raw, _ := io.ReadAll(resp.Body)
	frames := parseFrames(t, raw)
	if len(frames) < 6 || frames[0].Event != "message_start" ||
		frames[len(frames)-1].Event != "message_stop" {
		t.Fatalf("frames: %s", raw)
	}
	found := false
	for _, f := range frames {
		if f.Event == "content_block_delta" &&
			strings.Contains(f.Data, `"text":"plain text"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("text never delivered:\n%s", raw)
	}
}

// TestPassthrough: anything that is not POST /messages goes to the inner
// transport untouched (client web tools share this pool).
func TestPassthrough(t *testing.T) {
	var sawPath, sawMethod string
	tr := &Transport{
		Base: "http://never-reached.invalid/v1",
		Inner: rtFunc(func(r *http.Request) (*http.Response, error) {
			sawPath, sawMethod = r.URL.Path, r.Method
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/plain"}},
				Body:       io.NopCloser(strings.NewReader("ok")),
				Request:    r,
			}, nil
		}),
	}
	client := &http.Client{Transport: tr}
	resp, err := client.Get("http://example.com/fetch?q=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if sawMethod != http.MethodGet || sawPath != "/fetch" {
		t.Fatalf("inner saw %s %s", sawMethod, sawPath)
	}

	// POST to a non-messages path also passes through.
	req, _ := http.NewRequest(http.MethodPost, "http://example.com/webhook", strings.NewReader("{}"))
	r2, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if sawPath != "/webhook" {
		t.Fatalf("inner saw %s", sawPath)
	}
}

// TestAuthForwarding: an already-Bearer Authorization header (the
// ANTHROPIC_AUTH_TOKEN style) is forwarded verbatim, not double-wrapped.
func TestAuthForwarding(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"y"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	client := &http.Client{Transport: &Transport{Base: srv.URL}}
	req, _ := http.NewRequest(http.MethodPost, "http://anthropic.invalid/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":1,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer tok-123")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotAuth != "Bearer tok-123" {
		t.Fatalf("auth: %q", gotAuth)
	}
}

// TestSDKRoundTrip drives the real anthropic-sdk-go client through the
// translator — both the non-streaming New and the streaming accumulator —
// proving the translated wire is exactly what the SDK decodes.
func TestSDKRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		streaming := strings.Contains(string(body), `"stream":true`)
		if r.Header.Get("Authorization") != "Bearer sk-sdk" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"bad key","type":"invalid_request_error"}}`)
			return
		}
		if !streaming {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{
				"id":"chatcmpl-sdk","model":"gpt-x",
				"choices":[{"index":0,"message":{"role":"assistant","content":"Hello back"},
					"finish_reason":"stop"}],
				"usage":{"prompt_tokens":11,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":5}}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunks := []string{
			`{"id":"chatcmpl-sdk","model":"gpt-x","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello "}}]}`,
			`{"id":"chatcmpl-sdk","choices":[{"index":0,"delta":{"content":"back"}}]}`,
			`{"id":"chatcmpl-sdk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":5}}}`,
			`[DONE]`,
		}
		for _, c := range chunks {
			_, _ = io.WriteString(w, "data: "+c+"\n\n")
		}
	}))
	defer srv.Close()

	client := anthropic.NewClient(
		option.WithBaseURL("http://anthropic.invalid/v1/"),
		option.WithAPIKey("sk-sdk"),
		option.WithMaxRetries(0),
		option.WithHTTPClient(&http.Client{Transport: &Transport{Base: srv.URL}}),
	)
	ctx := context.Background()
	params := anthropic.BetaMessageNewParams{
		Model:     "claude-x",
		MaxTokens: 100,
		System:    []anthropic.BetaTextBlockParam{{Text: "SYS"}},
		Messages: []anthropic.BetaMessageParam{
			anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("hi")),
		},
	}

	// Non-streaming path (title/distill/compact one-shots use this).
	msg, err := client.Beta.Messages.New(ctx, params)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(msg.Content) != 1 || msg.Content[0].Type != "text" ||
		msg.Content[0].Text != "Hello back" {
		t.Fatalf("content: %+v", msg.Content)
	}
	if string(msg.StopReason) != "end_turn" {
		t.Fatalf("stop_reason: %v", msg.StopReason)
	}
	if msg.Usage.InputTokens != 11 || msg.Usage.CacheReadInputTokens != 5 {
		t.Fatalf("usage: %+v", msg.Usage)
	}

	// Streaming path (tool runner / compact use this).
	stream := client.Beta.Messages.NewStreaming(ctx, params)
	var acc anthropic.BetaMessage
	for stream.Next() {
		if err := acc.Accumulate(stream.Current()); err != nil {
			t.Fatalf("accumulate: %v", err)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(acc.Content) != 1 || acc.Content[0].Type != "text" ||
		acc.Content[0].Text != "Hello back" {
		t.Fatalf("accumulated content: %+v", acc.Content)
	}
	if string(acc.StopReason) != "end_turn" {
		t.Fatalf("accumulated stop: %v", acc.StopReason)
	}
	if acc.Usage.InputTokens != 11 || acc.Usage.OutputTokens != 4 ||
		acc.Usage.CacheReadInputTokens != 5 {
		t.Fatalf("accumulated usage: %+v", acc.Usage)
	}
}
