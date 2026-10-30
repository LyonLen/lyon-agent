// Background shell completion, session side: the tools registry calls
// onBgEnd (bound via tools.WithBackgroundOwner) when a detached run_shell
// exits. One rule — the chat always hears about it, and the model hears
// about it through whichever channel is live: gap injection while a run
// is in flight (Enqueue), a system note when idle (the next prompt opens
// with it). The event and the model-facing line both carry the tail so a
// reader never has to open the log to judge the outcome.
package agent

import (
	"fmt"
	"sync/atomic"
	"time"

	"lyon-agent/pkg/tools"
)

// bgKeySeq stamps sessions with unique bgKeys — never recycled, so a
// stale registry entry can never belong to a later session's key.
var bgKeySeq atomic.Uint64

// onBgEnd is the completion callback for this session's background
// shells. Called from the registry's waiter goroutine: emit is bounded,
// the delivery below never blocks.
func (s *Session) onBgEnd(b tools.BgInfo) {
	if s.ctx.Err() != nil {
		return // session is gone: no stream, no next prompt — drop quietly
	}

	status := fmt.Sprintf("exit %d", b.Code)
	if b.Err != "" {
		status = "failed: " + b.Err
	}
	detail := fmt.Sprintf("%s after %s", status, bgDur(b.ElapsedMS))
	s.emit(Event{
		Type:    EventBackgroundEnd,
		Detail:  detail,
		IsError: b.Code != 0 || b.Err != "",
		Err:     b.Err,
		Attrs: map[string]string{
			"task": fmt.Sprintf("%d", b.ID),
			"code": fmt.Sprintf("%d", b.Code),
			"log":  b.Log,
			"tail": clipTail(b.Tail, 600),
		},
		TS: time.Now(),
	})

	line := fmt.Sprintf("[background task #%d] %s\n%s\nfull log: %s",
		b.ID, detail, b.Tail, b.Log)
	if s.Busy() && s.Enqueue(line) == nil {
		return // a run is live: it picks the result up at the next turn gap
	}
	s.PushSystemNote(line) // idle (or the run just ended): next prompt opens with it
}

// bgDur renders a completion's wall time in the ledger's register:
// 8.6s / 4m12s.
func bgDur(ms int64) string {
	if ms < 60000 {
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	}
	m := ms / 60000
	s := (ms % 60000) / 1000
	return fmt.Sprintf("%dm%02ds", m, s)
}

// clipTail bounds a log tail for the event attrs — enough to judge the
// outcome, small enough for the 4096-deep replay ring.
func clipTail(t string, n int) string {
	if len(t) <= n {
		return t
	}
	return t[:n] + "…"
}
