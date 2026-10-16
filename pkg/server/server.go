package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"time"

	"lyon-agent/gen/go/lyon/v1/lyonv1connect"
	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/tools"
)

// Server routes HTTP: JSON session lifecycle endpoints plus SSE prompt
// streams. It holds one Engine (shared Anthropic client + transport) and a
// session Store.
type Server struct {
	eng   *agent.Engine
	store *Store
	mux   *http.ServeMux
	obs   tools.Observer // shared across sessions; feeds /metrics
	start time.Time
	token string // bearer token; empty = no auth (loopback-only deployments)
}

// New builds the server and its routes.
func New(eng *agent.Engine, store *Store) *Server {
	s := &Server{eng: eng, store: store, mux: http.NewServeMux(), start: time.Now()}
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("GET /metrics", s.handleMetrics)
	s.mux.HandleFunc("POST /v1/sessions", s.handleCreate)
	s.mux.HandleFunc("POST /v1/sessions/{id}/messages", s.handleMessage)
	s.mux.HandleFunc("DELETE /v1/sessions/{id}", s.handleDelete)
	s.mux.HandleFunc("GET /v1/sessions/{id}/stats", s.handleStats)
	// Connect RPC (web v1): mounts under /lyon.v1.*Service/* on the same
	// mux, so Handler()'s bearer-token wrapper covers them wholesale.
	sessPath, sessHandler := lyonv1connect.NewSessionServiceHandler(&sessionService{s: s})
	s.mux.Handle(sessPath, sessHandler)
	filePath, fileHandler := lyonv1connect.NewFileServiceHandler(&fileService{s: s})
	s.mux.Handle(filePath, fileHandler)
	dashPath, dashHandler := lyonv1connect.NewDashboardServiceHandler(&dashboardService{s: s})
	s.mux.Handle(dashPath, dashHandler)
	return s
}

// SetObserver installs an observer (typically tools.Metrics) that receives
// every session's tool executions. Call before serving.
func (s *Server) SetObserver(obs tools.Observer) { s.obs = obs }

// SetToken installs the bearer token that every route except GET /healthz
// will require. Empty disables auth (safe only behind loopback or a proxy).
// Call before Handler.
func (s *Server) SetToken(tok string) { s.token = tok }

// Handler returns the root HTTP handler, wrapped in token auth when a
// token is set. lyond hands out run_shell and file writes through these
// routes, so an unauthenticated listener is anonymous remote code execution.
func (s *Server) Handler() http.Handler {
	if s.token == "" {
		return s.mux
	}
	tok := s.token
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Liveness must stay cheap for probes; it exposes no user data.
		if r.URL.Path == "/healthz" {
			s.mux.ServeHTTP(w, r)
			return
		}
		if !authorized(r, tok) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="lyond"`)
			writeErr(w, http.StatusUnauthorized, errors.New("missing or invalid bearer token"))
			return
		}
		s.mux.ServeHTTP(w, r)
	})
}

// authorized reports whether r carries the expected token, comparing in
// constant time so the header cannot be probed byte by byte.
func authorized(r *http.Request, want string) bool {
	got := ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		got = strings.TrimPrefix(h, "Bearer ")
	} else if h := r.Header.Get("X-Lyon-Token"); h != "" {
		got = h
	}
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

type errorBody struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, errorBody{Error: err.Error()})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"sessions": s.store.Len(),
		"model":    s.eng.Config().Model,
	})
}

// usageRollup is the process-wide sessions→usage aggregation shared by
// /metrics and DashboardService.GetOverview — one rollup, two transports
// (web plan Phase 4). Sessions and CacheShare ride along for the RPC
// caller but are tagged out of the embedded "turns" object whose key set
// /metrics consumers see.
type usageRollup struct {
	Sessions      int     `json:"-"`
	Turns         int     `json:"turns"`
	Input         int64   `json:"input"`
	Output        int64   `json:"output"`
	CacheRead     int64   `json:"cache_read"`
	CacheCreation int64   `json:"cache_creation"`
	CacheShare    float64 `json:"-"`
}

// usageRollup walks every live session's per-turn stats — the same
// accounting /v1/sessions/{id}/stats reports per session.
func (s *Server) usageRollup() usageRollup {
	var r usageRollup
	sessions := s.store.Sessions()
	r.Sessions = len(sessions)
	for _, sess := range sessions {
		for _, t := range sess.Stats() {
			r.Turns++
			r.Input += t.Usage.Input
			r.Output += t.Usage.Output
			r.CacheRead += t.Usage.CacheRead
			r.CacheCreation += t.Usage.CacheCreation
		}
	}
	totalIn := float64(r.Input + r.CacheRead + r.CacheCreation)
	if totalIn > 0 {
		r.CacheShare = float64(r.CacheRead+r.CacheCreation) / totalIn
	}
	return r
}

// handleMetrics aggregates process, session, and tool metrics in one JSON
// document: turn usage totals (with cache share), per-tool latency
// percentiles, and live session/turn counts.
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	agg := s.usageRollup()

	body := map[string]any{
		"uptime_seconds": int(time.Since(s.start).Seconds()),
		"sessions":       agg.Sessions,
		"model":          s.eng.Config().Model,
		"turns":          agg,
		"cache_share":    agg.CacheShare,
		"goroutines":     runtime.NumGoroutine(),
	}
	if m, ok := s.obs.(*tools.Metrics); ok && m != nil {
		// DTO: time.Duration marshals as raw nanoseconds, useless over JSON.
		type toolDTO struct {
			Name    string  `json:"name"`
			Calls   int     `json:"calls"`
			Errors  int     `json:"errors"`
			AvgMS   float64 `json:"avg_ms"`
			P50MS   float64 `json:"p50_ms"`
			P95MS   float64 `json:"p95_ms"`
			MaxMS   float64 `json:"max_ms"`
			LastErr string  `json:"last_error,omitempty"`
		}
		snap := m.Snapshot()
		toolsOut := make([]toolDTO, 0, len(snap))
		for _, tm := range snap {
			toolsOut = append(toolsOut, toolDTO{
				Name: tm.Name, Calls: tm.Calls, Errors: tm.Errors,
				AvgMS:   float64(tm.Avg().Microseconds()) / 1000,
				P50MS:   float64(tm.P50.Microseconds()) / 1000,
				P95MS:   float64(tm.P95.Microseconds()) / 1000,
				MaxMS:   float64(tm.Max.Microseconds()) / 1000,
				LastErr: tm.LastErr,
			})
		}
		body["tools"] = toolsOut
	}
	writeJSON(w, http.StatusOK, body)
}

type createRequest struct {
	Message string `json:"message"`
}

// handleCreate registers a session. With {"message": "..."} it also starts
// the first prompt and streams the response as SSE; without a message it
// returns JSON with the session id for later /messages calls.
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, errors.New("invalid JSON body: "+err.Error()))
			return
		}
	}

	sess := s.eng.NewSession(s.obs)
	id, err := s.store.Create(sess)
	if err != nil {
		sess.Close()
		if errors.Is(err, ErrFull) {
			writeErr(w, http.StatusTooManyRequests, err)
		} else {
			writeErr(w, http.StatusInternalServerError, err)
		}
		return
	}

	if strings.TrimSpace(req.Message) == "" {
		writeJSON(w, http.StatusCreated, map[string]string{"session_id": id})
		return
	}

	// One-shot: submit then stream. The hub is the session's only Events()
	// consumer now (store.Create started it), so attach the SSE pump to the
	// hub BEFORE Submit — an anchor taken later would miss the opening
	// frames (they only ever pass through once).
	h, err := s.store.Hub(id)
	if err != nil {
		s.store.Delete(id)
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	sub := h.subscribeLive()
	defer h.unsubscribe(sub)

	if err := sess.Submit(req.Message); err != nil {
		s.store.Delete(id)
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeSSEHeaders(w)
	w.WriteHeader(http.StatusOK)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	// Announce the id as the first frame so the client can follow up.
	sseFrame(w, "session", map[string]string{"session_id": id})
	if err := streamPrompt(w, r, sub); err != nil {
		// Client gone or write deadline tripped. The driver may be wedged
		// mid-prompt with nobody reading its bounded buffer — close the
		// session so it stops. Headers are already out, so the client just
		// sees the stream end without prompt_end.
		s.store.Delete(id)
		return
	}
}

// handleMessage runs one prompt against an existing session as SSE.
func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := s.store.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}

	var req struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("invalid JSON body: "+err.Error()))
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		writeErr(w, http.StatusBadRequest, errors.New("message is required"))
		return
	}

	// Attach to the hub before Submit (see handleCreate): the pump must be
	// anchored before the first event is stamped.
	h, err := s.store.Hub(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	sub := h.subscribeLive()
	defer h.unsubscribe(sub)

	if err := sess.Submit(req.Message); err != nil {
		if errors.Is(err, agent.ErrPromptInProgress) {
			writeErr(w, http.StatusConflict, err)
		} else {
			writeErr(w, http.StatusInternalServerError, err)
		}
		return
	}
	// From here the response is an SSE stream. On a broken stream (disconnect
	// or slow-client kick) the session would wedge mid-prompt with nobody
	// watching, so a failure tears the session down.
	if err := streamPrompt(w, r, sub); err != nil {
		s.store.Delete(id)
		return
	}
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if !s.store.Delete(r.PathValue("id")) {
		writeErr(w, http.StatusNotFound, ErrNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	sess, err := s.store.Get(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	// DTO: time.Duration marshals as raw nanoseconds, useless over JSON.
	type turnStat struct {
		Turn       int         `json:"turn"`
		StopReason string      `json:"stop_reason"`
		Usage      agent.Usage `json:"usage"`
		DurationMS int64       `json:"duration_ms"`
		At         string      `json:"at"`
	}
	stats := sess.Stats()
	out := make([]turnStat, len(stats))
	for i, t := range stats {
		out[i] = turnStat{
			Turn:       t.Turn,
			StopReason: t.StopReason,
			Usage:      t.Usage,
			DurationMS: t.Duration.Milliseconds(),
			At:         t.At.Format(time.RFC3339Nano),
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// sseFrame writes one already-headed SSE frame.
func sseFrame(w http.ResponseWriter, event string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
