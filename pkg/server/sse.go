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

// streamPrompt pumps a pre-attached hub subscription to the client as SSE
// until the prompt ends, the client disconnects, or the write deadline
// kicks a stalled consumer. Returns when the stream is finished.
//
// The subscription must be taken (hub.subscribeLive) BEFORE Session.Submit:
// the hub stamps events from the moment the prompt runs, and an anchor
// taken later would miss the opening frames — nothing else re-delivers
// them on this path (the legacy SSE contract is one stream per prompt,
// from message_start to prompt_end).
//
// Backpressure model: writes to a slow-but-alive client block (TCP
// window), which backs up this one subscriber's bounded channel. A client
// that stops reading entirely trips the per-event write deadline; we then
// abandon the stream and the caller tears the session down (the legacy
// broken-stream contract). A subscriber that outruns hubSubBuffer is
// kicked by the hub — that surfaces here as a hard error too.
func streamPrompt(w http.ResponseWriter, r *http.Request, sub *hubSub) error {
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
		case it, ok := <-sub.ch:
			if !ok {
				if sub.kicked {
					return fmt.Errorf("event subscriber fell behind (kicked by hub)")
				}
				return nil // session closed
			}
			if err := emit(it.ev); err != nil {
				return err
			}
			if it.ev.Type == agent.EventPromptEnd {
				return nil
			}
		}
	}
}
