// Interval session titles: lyond regenerates a session's title on the
// user-set beat — after prompt counts 1, 6, 26, 46 … (gaps 1, 5, 20,
// 20 …) — summarizing the last ≤5 prompts plus the current title. The
// TUI's per-prompt intent check (tui.go kickTitleCheck → RefreshTitle)
// is untouched; this is the server-side equivalent for web sessions,
// which had no title path at all (every row stayed "(untitled)").

package server

import (
	"context"
	"sync"
	"time"

	"lyon-agent/pkg/agent"
)

// titleBeats are the prompt counts a summary fires on: the first prompt
// names the session immediately, then gaps of 5, 20, 20, 20 …
func titleBeat(prompts int64) bool {
	return prompts == 1 || (prompts >= 6 && (prompts-6)%20 == 0)
}

// titleInFlight tracks sessions with a summary call still out, so a beat
// can't stampede the same session — beats are ≥5 prompts apart, but a
// paused model answer could still straddle the next one. Sessions are
// independent: one slow session never blocks another's beat.
var titleInFlight sync.Map // *agent.Session → struct{}

// maybeIntervalTitle checks the beat and, on a hit, runs the summary on
// its own goroutine — the hub's consumer goroutine calls this and must
// never wait on an LLM round trip. The cadence input is the SESSION's
// prompt count (SubmitOn plus injected gap lines). It used to read the
// engine hooks' Stats().Prompts — that counter is process-wide, so on a
// multi-session lyond it drifts and almost no session ever lands on
// beats 1/6/26 (every row stayed "(untitled)"). Failures keep the
// previous title (IntervalTitle's contract); LYON_TITLE=off is honored
// inside IntervalTitle itself.
func maybeIntervalTitle(sess *agent.Session) {
	if !titleBeat(sess.PromptCount()) {
		return
	}
	if _, loaded := titleInFlight.LoadOrStore(sess, struct{}{}); loaded {
		return // a summary is still out for this session; the next beat catches up
	}
	go func() {
		defer titleInFlight.Delete(sess)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _, _ = sess.IntervalTitle(ctx) // error ⇒ previous title stays
	}()
}
