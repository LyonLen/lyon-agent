package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"lyon-agent/pkg/agent"
)

// writeSSEHeaders commits the streaming response headers. X-Accel-Buffering
// disables proxy buffering (nginx and friends).
func writeSSEHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
}

// streamPrompt pumps session events to the client as SSE until the prompt
// ends, the client disconnects, or the write deadline kicks a stalled
// consumer. Returns when the stream is finished.
//
// Backpressure model: writes to a slow-but-alive client block (TCP window),
// which blocks reading from the session's bounded event buffer, which blocks
// the driver — natural, lossless backpressure. A client that stops reading
// entirely trips the per-event write deadline; we then abandon the stream
// (the session itself stays alive and can be resumed or deleted).
func streamPrompt(w http.ResponseWriter, r *http.Request, sess *agent.Session) error {
	writeSSEHeaders(w)
	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("response writer does not support flushing")
	}
	rc := http.NewResponseController(w)

	const writeTimeout = 15 * time.Second

	emit := func(ev agent.Event) error {
		data, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		// Reset the write deadline for this event so a stalled client fails
		// the write instead of hanging the handler forever.
		_ = rc.SetWriteDeadline(time.Now().Add(writeTimeout))
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	for {
		select {
		case <-r.Context().Done():
			return r.Context().Err()
		case ev, ok := <-sess.Events():
			if !ok {
				return nil // session closed
			}
			if err := emit(ev); err != nil {
				return err
			}
			if ev.Type == agent.EventPromptEnd {
				return nil
			}
		}
	}
}
