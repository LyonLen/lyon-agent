package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"lyon-agent/pkg/tools"
)

// TestOpenAIGatewayToolLoop drives the full engine tool loop across the
// Anthropic→OpenAI translation layer against a scripted gateway: turn 1
// comes back as an OpenAI tool_call, the runner executes the local tool,
// turn 2 carries the result as a role:"tool" message, and the loop ends on
// text. This is the production path (NewToolRunnerStreaming) end to end.
func TestOpenAIGatewayToolLoop(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	var lastBody []byte
	var sawAnthropicHeader bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("anthropic-version") != "" || r.Header.Get("x-api-key") != "" {
			mu.Lock()
			sawAnthropicHeader = true
			mu.Unlock()
		}
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls++
		call := calls
		lastBody = body
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch call {
		case 1:
			_, _ = io.WriteString(w, `{
				"id":"chatcmpl-1","model":"gw-model",
				"choices":[{"index":0,"message":{"role":"assistant","content":null,
					"tool_calls":[{"id":"call_1","type":"function",
						"function":{"name":"get_quote","arguments":"{}"}}]},
					"finish_reason":"tool_calls"}],
				"usage":{"prompt_tokens":21,"completion_tokens":9}}`)
		default:
			_, _ = io.WriteString(w, `{
				"id":"chatcmpl-2","model":"gw-model",
				"choices":[{"index":0,"message":{"role":"assistant","content":"quote: 42"},
					"finish_reason":"stop"}],
				"usage":{"prompt_tokens":31,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":20}}}`)
		}
	}))
	defer srv.Close()

	reg := tools.NewRegistry()
	var toolRan bool
	if err := tools.RegisterFunc(reg, "get_quote", "returns the quote", true,
		func(ctx context.Context, _ struct{}) (string, error) {
			toolRan = true
			return "42", nil
		}); err != nil {
		t.Fatal(err)
	}

	eng := NewEngine(Config{
		OpenAIBase:  srv.URL,
		Model:       "gw-model",
		WebToolMode: "off",
		Workspace:   tempDir(t),
	}, reg)
	sess := eng.NewSession(nil)
	defer sess.Close()

	if err := sess.Submit("what is the quote?"); err != nil {
		t.Fatal(err)
	}
	pe := waitEvent(t, sess, EventPromptEnd)
	if pe.Err != "" {
		t.Fatalf("prompt failed over the gateway: %s", pe.Err)
	}

	if !toolRan {
		t.Fatal("tool_call never executed the local tool")
	}
	mu.Lock()
	defer mu.Unlock()
	if sawAnthropicHeader {
		t.Fatal("anthropic-version/x-api-key headers leaked to the OpenAI gateway")
	}
	if calls != 2 {
		t.Fatalf("gateway calls = %d, want 2", calls)
	}

	// Turn 2 must carry the tool output as a role:"tool" message and the
	// OpenAI-shaped tool definition for the first turn's request.
	var req struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
		StreamOptions *struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if err := json.Unmarshal(lastBody, &req); err != nil {
		t.Fatalf("second request: %v", err)
	}
	if req.Model != "gw-model" || !req.Stream || req.StreamOptions == nil || !req.StreamOptions.IncludeUsage {
		t.Fatalf("request header: %+v", req)
	}
	var sawToolResult bool
	for _, m := range req.Messages {
		if m.Role == "tool" && strings.Contains(string(m.Content), "42") {
			sawToolResult = true
		}
	}
	if !sawToolResult {
		t.Fatalf("no role:tool message with the result:\n%s", lastBody)
	}

	// The runner keeps the tools array attached on every turn; entries must
	// be OpenAI function-tool shaped (registry order is alphabetical).
	var haveQuote bool
	for _, tl := range req.Tools {
		if tl.Type == "function" && tl.Function.Name == "get_quote" {
			haveQuote = true
		}
	}
	if !haveQuote {
		t.Fatalf("tools array not translated: %+v", req.Tools)
	}
}
