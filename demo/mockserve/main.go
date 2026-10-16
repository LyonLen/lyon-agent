// Command mockserve is the demo-60s mock gateway (ROADMAP 0.3): a thin
// supervising front for two internal/mockapi servers — one serving the
// scripted task turns, one serving the diagnosis reply — plus the two
// capabilities mockapi itself deliberately has no knobs for:
//
//   - content-based routing: a request whose system prompt is the
//     failure-post-mortem (or the title check) goes to its own mock, so
//     side-channel Ask calls can never shift the task script's index;
//   - sticky failure injection: once a phase's fail_at call index is
//     reached, every task request answers 500 until the next reset —
//     SDK retries see the same failure, so the prompt reliably ends in
//     an error (the event that kicks 错因归纳) without the script
//     desyncing. The diagnosis route bypasses the failure by content,
//     which is how the post-mortem still gets its one model call.
//
// Admin API on the same port:
//
//	POST /__demo/reset  {"task":[turn...],"fail_at":N,"diag":[turn...]}
//	GET  /__demo/status {"total":N,"task":N,"diag":N,"fail":bool,"last_body_len":N}
//	GET  /__demo/last_body   raw body of the most recent request
//
// On start it prints one line "MOCKSERVE <url>" for the driver to parse.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"lyon-agent/internal/mockapi"
)

// Content markers that route a request away from the task script. Both
// are distinctive prefixes of the prompts they identify (eval.DiagnoseSystem,
// agent.titleSystemPrompt) — substring matches on the raw body, which
// carries the system prompt JSON-escaped but otherwise verbatim.
const (
	diagMarker  = "post-mortem a failed agent attempt"
	titleMarker = "You maintain a terse session title"
)

// turnSpec is the JSON shape of one scripted turn:
//
//	{"text":["chunk1","chunk2"]}          a streamed text reply
//	{"tool":{"name":"read_file","input":"{\"path\":\"x\"}"}}
type turnSpec struct {
	Text []string `json:"text,omitempty"`
	Tool *struct {
		Name  string `json:"name"`
		Input string `json:"input"`
	} `json:"tool,omitempty"`
}

func (t turnSpec) turn() (mockapi.Turn, error) {
	switch {
	case t.Tool != nil:
		return mockapi.Turn{Tool: &mockapi.ToolCall{Name: t.Tool.Name, Input: t.Tool.Input}}, nil
	case len(t.Text) > 0:
		return mockapi.Turn{Text: t.Text}, nil
	default:
		return mockapi.Turn{}, fmt.Errorf("turn has neither text nor tool")
	}
}

type resetReq struct {
	Task   []turnSpec `json:"task"`
	Diag   []turnSpec `json:"diag"`
	FailAt int        `json:"fail_at"`
}

// state is the mutable routing/failure world. Every admin mutation and
// every proxied request serializes through mu — the demo is strictly
// sequential, contention is not a design concern.
type state struct {
	mu        sync.Mutex
	taskSrv   *mockapi.Server
	diagSrv   *mockapi.Server
	failAt    int
	taskCalls int
	diagCalls int
	failMode  bool // sticky: armed at failAt, cleared only by reset
	lastBody  []byte
	latency   time.Duration
}

func turnsOf(specs []turnSpec) ([]mockapi.Turn, error) {
	out := make([]mockapi.Turn, 0, len(specs))
	for _, ts := range specs {
		t, err := ts.turn()
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func (s *state) reset(req resetReq) error {
	taskTurns, err := turnsOf(req.Task)
	if err != nil {
		return fmt.Errorf("task: %w", err)
	}
	diagTurns, err := turnsOf(req.Diag)
	if err != nil {
		return fmt.Errorf("diag: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.taskSrv != nil {
		s.taskSrv.Close()
	}
	if s.diagSrv != nil {
		s.diagSrv.Close()
	}
	s.taskSrv = mockapi.New(mockapi.Config{Script: taskTurns, Latency: s.latency})
	s.diagSrv = mockapi.New(mockapi.Config{Script: diagTurns, Latency: s.latency})
	s.failAt = req.FailAt
	s.taskCalls = 0
	s.diagCalls = 0
	s.failMode = false
	s.lastBody = nil
	return nil
}

func main() {
	addr := flag.String("addr", "127.0.0.1:0", "listen address")
	latency := flag.Duration("latency", 15*time.Millisecond, "per-response latency injected into forwarded calls")
	flag.Parse()

	st := &state{latency: *latency}
	// Fresh empty mocks so /v1/messages answers (with "ok") even before
	// the driver's first reset — a race in the driver must degrade to a
	// visible wrong answer, not a connection error.
	if err := st.reset(resetReq{}); err != nil {
		fmt.Fprintln(os.Stderr, "mockserve: startup reset:", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", st.handleMessages)
	mux.HandleFunc("POST /__demo/reset", st.handleReset)
	mux.HandleFunc("GET /__demo/status", st.handleStatus)
	mux.HandleFunc("GET /__demo/last_body", st.handleLastBody)

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mockserve: listen:", err)
		os.Exit(1)
	}
	// One machine-parseable line first (url + pid, so the demo driver can
	// reap exactly the instance it started); anything after is diagnostics.
	fmt.Printf("MOCKSERVE http://%s pid=%d\n", ln.Addr().String(), os.Getpid())
	if err := (&http.Server{Handler: mux}).Serve(ln); err != nil {
		fmt.Fprintln(os.Stderr, "mockserve: serve:", err)
		os.Exit(1)
	}
}

// handleMessages is the demo-facing /v1/messages: record, classify,
// then either inject the sticky failure or relay to the right mock.
func (s *state) handleMessages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	s.lastBody = body
	switch {
	case contains(body, diagMarker):
		s.diagCalls++
		srv := s.diagSrv
		s.mu.Unlock()
		relay(w, r, body, srv)
	case contains(body, titleMarker):
		s.mu.Unlock()
		// Insurance, not a feature: LYON_TITLE=off gates this call before
		// it is ever made — but if an env ever gets dropped, answering it
		// here keeps the task script's call indices from silently shifting.
		writeTitleReply(w, body)
	default:
		s.taskCalls++
		fail := s.failAt > 0 && s.taskCalls >= s.failAt
		if fail {
			s.failMode = true // sticky: retries fail the same way
		}
		fail = fail || (s.failMode && s.failAt > 0)
		srv := s.taskSrv
		s.mu.Unlock()
		if fail {
			writeInjectedFailure(w)
			return
		}
		relay(w, r, body, srv)
	}
}

func contains(body []byte, marker string) bool {
	return bytes.Contains(body, []byte(marker))
}

// relay copies one mockapi response — status, headers, and a flushed
// body stream so SSE frames reach the SDK as they are produced. The
// caller hands back the body bytes it already read for classification.
func relay(w http.ResponseWriter, r *http.Request, body []byte, srv *mockapi.Server) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, srv.URL()+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header = r.Header.Clone()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			_, _ = w.Write(buf[:n])
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// writeInjectedFailure answers the 500 that ends a prompt. Shape matches
// the API's error envelope so the SDK surfaces a readable message in the
// transcript and the diagnosis trajectory.
func writeInjectedFailure(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = io.WriteString(w, `{"type":"error","error":{"type":"api_error","message":"demo-60s: fake model repeated its mistake — injected failure"}}`)
}

// writeTitleReply is the cached-shape answer to the title check: the
// non-streaming envelope (standalone Ask calls are non-streaming) with
// changed:false, i.e. "keep the current title".
func writeTitleReply(w http.ResponseWriter, body []byte) {
	model := "mock-model"
	streaming := false
	var head map[string]json.RawMessage
	if json.Unmarshal(body, &head) == nil {
		if raw, ok := head["model"]; ok {
			_ = json.Unmarshal(raw, &model)
		}
		if raw, ok := head["stream"]; ok {
			_ = json.Unmarshal(raw, &streaming)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if !streaming {
		_, _ = io.WriteString(w, fmt.Sprintf(
			`{"id":"msg_mocktitle","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":"{\"changed\":false}"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
			model))
		return
	}
	// Streaming shape (kept for completeness — Ask is non-streaming):
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	_, _ = io.WriteString(w,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_mocktitle\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\""+model+"\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"+
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"+
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"{\\\"changed\\\":false}\"}}\n\n"+
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"+
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":1}}\n\n"+
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
}

func (s *state) handleReset(w http.ResponseWriter, r *http.Request) {
	var req resetReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad reset body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.reset(req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "fail_at": req.FailAt})
}

func (s *state) handleStatus(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, map[string]any{
		"total":         s.taskCalls + s.diagCalls,
		"task":          s.taskCalls,
		"diag":          s.diagCalls,
		"fail":          s.failMode,
		"last_body_len": len(s.lastBody),
	})
}

func (s *state) handleLastBody(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	body := append([]byte(nil), s.lastBody...)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(body)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
