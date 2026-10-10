// Package mockapi is a scripted, in-process Anthropic-compatible API used
// by the bench suite. It streams real SSE event sequences, applies
// configurable latency, and runs a prefix-hash prompt-cache simulator over
// raw request bodies — so tests can assert cache behavior without an API
// key and can prove that byte drift in requests breaks cache hits.
package mockapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"
)

// ToolCall is one scripted tool invocation: the model emits a tool_use
// block with this name and raw-JSON input.
type ToolCall struct {
	Name  string
	Input string
}

// Turn is one scripted assistant response. Exactly one of Text (streamed as
// a text block) or Tool (streamed as a tool_use block) is used.
type Turn struct {
	Text []string
	Tool *ToolCall
}

// Config configures a mock server.
type Config struct {
	// Script is consumed one entry per API call; the last entry repeats
	// once the script is exhausted. Empty means a single "ok" text turn.
	Script []Turn
	// Latency is slept before each response's first byte — the noise floor
	// bench tests subtract from end-to-end turn latency.
	Latency time.Duration
	// MinCacheBytes drops cache breakpoint entries shorter than this,
	// approximating the API's minimum cacheable prefix. 0 disables.
	MinCacheBytes int64
}

// Server is a running mock API. Point ANTHROPIC_BASE_URL at URL() and the
// Anthropic SDK talks to it unchanged.
type Server struct {
	cfg Config
	ts  *httptest.Server

	mu       sync.Mutex
	calls    int
	lastBody []byte
	cache    cacheStore
}

// New starts a mock server; the caller must Close it.
func New(cfg Config) *Server {
	s := &Server{cfg: cfg}
	s.cache.entries = make(map[[32]byte]int64)
	s.cache.min = cfg.MinCacheBytes
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", s.handle)
	s.ts = httptest.NewServer(mux)
	return s
}

// URL returns the base URL for ANTHROPIC_BASE_URL.
func (s *Server) URL() string { return s.ts.URL }

// Close shuts the server down.
func (s *Server) Close() { s.ts.Close() }

// Calls reports how many API requests the mock has served.
func (s *Server) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// LastBody returns a copy of the most recent request body — lets tests
// assert exactly what history the next prompt replayed.
func (s *Server) LastBody() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.lastBody...)
}

// CacheStats returns cumulative simulator accounting: read / creation /
// fresh-input tokens and request count.
func (s *Server) CacheStats() (read, create, input, requests int64) {
	return s.cache.Stats()
}

func (s *Server) pick(call int) Turn {
	if len(s.cfg.Script) == 0 {
		return Turn{Text: []string{"ok"}}
	}
	i := call - 1
	if i >= len(s.cfg.Script) {
		i = len(s.cfg.Script) - 1
	}
	return s.cfg.Script[i]
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var head map[string]json.RawMessage
	_ = json.Unmarshal(body, &head)

	streaming := true
	if raw, ok := head["stream"]; ok {
		_ = json.Unmarshal(raw, &streaming)
	}
	model := "mock-model"
	if raw, ok := head["model"]; ok {
		_ = json.Unmarshal(raw, &model)
	}

	read, create, input := s.cache.observe(body)

	s.mu.Lock()
	s.calls++
	call := s.calls
	s.lastBody = body
	s.mu.Unlock()

	turn := s.pick(call)
	if s.cfg.Latency > 0 {
		time.Sleep(s.cfg.Latency)
	}

	id := fmt.Sprintf("msg_mock%06d", call)
	if turn.Tool != nil {
		writeToolResponse(w, streaming, model, id, call, turn.Tool, read, create, input)
		return
	}
	writeTextResponse(w, streaming, model, id, turn.Text, read, create, input)
}

type usage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

type msgEnvelope struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	Role         string          `json:"role"`
	Model        string          `json:"model"`
	Content      json.RawMessage `json:"content"`
	StopReason   *string         `json:"stop_reason"`
	StopSequence *string         `json:"stop_sequence"`
	Usage        usage           `json:"usage"`
}

func writeTextResponse(w http.ResponseWriter, streaming bool, model, id string, chunks []string, read, create, input int64) {
	if len(chunks) == 0 {
		chunks = []string{"ok"}
	}
	var chars int
	for _, c := range chunks {
		chars += len(c)
	}
	outTokens := toks(int64(chars))
	if outTokens < 1 {
		outTokens = 1
	}
	u := usage{InputTokens: input, OutputTokens: outTokens, CacheReadInputTokens: read, CacheCreationInputTokens: create}

	if !streaming {
		content, _ := json.Marshal([]map[string]string{{"type": "text", "text": joinChunks(chunks)}})
		reason := "end_turn"
		resp := msgEnvelope{ID: id, Type: "message", Role: "assistant", Model: model,
			Content: content, StopReason: &reason, Usage: u}
		writeJSON(w, resp)
		return
	}

	f := startStream(w)
	defer f.Close()
	f.event("message_start", map[string]any{
		"type": "message_start",
		"message": msgEnvelope{ID: id, Type: "message", Role: "assistant", Model: model,
			Content: json.RawMessage("[]"), Usage: usage{InputTokens: input, CacheReadInputTokens: read, CacheCreationInputTokens: create}},
	})
	f.event("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": 0, "content_block": map[string]any{"type": "text", "text": ""},
	})
	for _, c := range chunks {
		f.event("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": 0, "delta": map[string]any{"type": "text_delta", "text": c},
		})
	}
	f.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	reason := "end_turn"
	f.event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": reason, "stop_sequence": nil},
		"usage": u,
	})
	f.event("message_stop", map[string]any{"type": "message_stop"})
}

func writeToolResponse(w http.ResponseWriter, streaming bool, model, id string, call int, tc *ToolCall, read, create, input int64) {
	const outTokens = 16
	u := usage{InputTokens: input, OutputTokens: outTokens, CacheReadInputTokens: read, CacheCreationInputTokens: create}
	toolID := fmt.Sprintf("toolu_mock%06d", call)

	if !streaming {
		content, _ := json.Marshal([]map[string]any{{
			"type": "tool_use", "id": toolID, "name": tc.Name, "input": json.RawMessage(tc.Input),
		}})
		reason := "tool_use"
		resp := msgEnvelope{ID: id, Type: "message", Role: "assistant", Model: model,
			Content: content, StopReason: &reason, Usage: u}
		writeJSON(w, resp)
		return
	}

	f := startStream(w)
	defer f.Close()
	f.event("message_start", map[string]any{
		"type": "message_start",
		"message": msgEnvelope{ID: id, Type: "message", Role: "assistant", Model: model,
			Content: json.RawMessage("[]"), Usage: usage{InputTokens: input, CacheReadInputTokens: read, CacheCreationInputTokens: create}},
	})
	f.event("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": 0, "content_block": map[string]any{"type": "tool_use", "id": toolID, "name": tc.Name, "input": map[string]any{}},
	})
	f.event("content_block_delta", map[string]any{
		"index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": tc.Input},
	})
	f.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	reason := "tool_use"
	f.event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": reason, "stop_sequence": nil},
		"usage": u,
	})
	f.event("message_stop", map[string]any{"type": "message_stop"})
}

// sseWriter emits `event:`/`data:` frames in the exact shape the Anthropic
// SDK's stream decoder expects.
type sseWriter struct {
	w   http.ResponseWriter
	fl  http.Flusher
	err error
}

func startStream(w http.ResponseWriter) *sseWriter {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	fl, _ := w.(http.Flusher)
	return &sseWriter{w: w, fl: fl}
}

func (s *sseWriter) event(name string, data any) {
	if s.err != nil {
		return
	}
	raw, err := json.Marshal(data)
	if err != nil {
		s.err = err
		return
	}
	if _, err = fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, raw); err != nil {
		s.err = err
		return
	}
	if s.fl != nil {
		s.fl.Flush()
	}
}

func (s *sseWriter) Close() error { return s.err }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func joinChunks(chunks []string) string {
	out := ""
	for _, c := range chunks {
		out += c
	}
	return out
}
