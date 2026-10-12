package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// Client-side compaction. The SDK's CompactBeforeNextTurn only works when
// the endpoint returns a native compaction block — gateways that don't
// silently keep the full history while paying for the request — so lyon
// summarizes the history itself and replaces it with one user message. The
// path is endpoint-agnostic, testable against mockapi, and never wastes a
// turn.

const (
	// compactBlockCap bounds one history block inside the summarizer
	// request so a single giant tool result cannot blow the compaction
	// prompt (chars/runes, not bytes).
	compactBlockCap = 4000
	// compactMaxTokens bounds the summary itself: dense, not exhaustive.
	compactMaxTokens = 4096
)

// compactSystemPrompt instructs the summarizer. Key requirement: the
// ORIGINAL user request survives verbatim — continuation depends on it.
const compactSystemPrompt = `You are given a conversation between a user and an AI agent, rendered as role-tagged text. Produce a dense continuation summary that lets the agent resume the original task without the full history.

Preserve verbatim: the user's original request(s); decisions already made and their rationale; file paths, commands and their outcomes; the current task state and exact next steps; constraints, security rules and user preferences.

Omit pleasantries, reasoning traces and redundant detail. Reply with the summary only — no preamble, no surrounding quotes.`

// clipRunes truncates to n runes at a rune boundary.
func clipRunes(s string, n int) string {
	if len(s) <= n { // fast path: ASCII-heavy content under the cap
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "... [truncated]"
}

// renderHistory flattens a conversation into the text handed to the
// summarizer: role-tagged entries, thinking dropped (never useful for
// continuation), every block clipped at compactBlockCap.
func renderHistory(msgs []anthropic.BetaMessageParam) string {
	var b strings.Builder
	emit := func(role, entry string) {
		fmt.Fprintf(&b, "[%s] %s\n\n", role, clipRunes(entry, compactBlockCap))
	}
	for _, m := range msgs {
		role := string(m.Role)
		for _, blk := range m.Content {
			switch {
			case blk.OfText != nil:
				if t := strings.TrimSpace(blk.OfText.Text); t != "" {
					emit(role, t)
				}
			case blk.OfToolUse != nil:
				emit(role, fmt.Sprintf("→ tool %s %s", blk.OfToolUse.Name, blk.OfToolUse.Input))
			case blk.OfToolResult != nil:
				var parts []string
				for _, c := range blk.OfToolResult.Content {
					if c.OfText != nil && strings.TrimSpace(c.OfText.Text) != "" {
						parts = append(parts, c.OfText.Text)
					}
				}
				if len(parts) > 0 {
					emit(role, "← tool result: "+strings.Join(parts, "\n"))
				}
			case blk.OfImage != nil:
				emit(role, "(image omitted)")
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// historyEndsWithToolUse reports whether the last message is an assistant
// tool_use awaiting results — the signal that the runner still owes tool
// executions, so compaction must recreate the runner instead of just
// swapping Params.Messages (the SDK would dispatch those calls against the
// summary).
func historyEndsWithToolUse(msgs []anthropic.BetaMessageParam) bool {
	if len(msgs) == 0 {
		return false
	}
	last := msgs[len(msgs)-1]
	if last.Role != anthropic.BetaMessageParamRoleAssistant {
		return false
	}
	for _, b := range last.Content {
		if b.OfToolUse != nil {
			return true
		}
	}
	return false
}

// compactNow summarizes the current history and replaces it with a single
// summary user message. Called from the drive goroutine (histMu held) when
// maybeCompact triggered. Two cases:
//
//   - history ends without a dangling tool call: swap Params.Messages; the
//     runner completes (or the next prompt's buildHistory picks it up).
//   - history ends on an assistant tool_use: recreate the runner seeded
//     with the summary, because the SDK would otherwise dispatch the
//     abandoned calls against the replacement message.
//
// trigger is the usage that crossed the threshold (records the progress
// guard); turnIdx continues this prompt's turn numbering.
func (s *Session) compactNow(pctx context.Context, prompt string, trigger Usage, turnIdx *int) {
	// Progress guard BEFORE any early return: one attempt per growth step,
	// even when summarizing fails — a failing endpoint cannot loop us.
	s.lastCompactInput = float64(trigger.Input + trigger.CacheRead + trigger.CacheCreation)
	if pctx.Err() != nil {
		return
	}
	msgs := s.runner.Messages()
	pendingTools := historyEndsWithToolUse(msgs)
	// remaining is the recreated runner's budget: what the prompt has left
	// of a set cap, or 0 (unlimited) when no cap is configured — passing
	// "cfg − spent" through unchanged would read as a negative, i.e. cut,
	// budget where none exists.
	remaining := 0
	if s.eng.cfg.MaxIterations > 0 {
		remaining = s.eng.cfg.MaxIterations - *turnIdx
		if pendingTools && remaining <= 0 {
			return // at the iteration cap: repairInterruptedHistory closes the call
		}
	}
	rendered := renderHistory(msgs)
	if rendered == "" {
		return
	}

	if !s.eng.acquireTurn(pctx) {
		return
	}
	defer s.eng.releaseTurn()
	turnStart := time.Now()

	params := anthropic.BetaMessageNewParams{
		Model:     s.eng.cfg.Model,
		MaxTokens: compactMaxTokens,
		System:    []anthropic.BetaTextBlockParam{{Text: compactSystemPrompt}},
		Messages: []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(
			anthropic.NewBetaTextBlock(
				"Current user prompt (verbatim):\n" + prompt +
					"\n\nRendered conversation history (role-tagged):\n" + rendered))},
	}
	stream := s.eng.client.Beta.Messages.NewStreaming(pctx, params)
	var resp anthropic.BetaMessage
	for stream.Next() {
		if err := resp.Accumulate(stream.Current()); err != nil {
			break // stream.Err() below reports the real failure
		}
	}
	if err := stream.Err(); err != nil {
		if pctx.Err() == nil {
			s.emit(Event{Type: EventCompaction, Detail: "compaction failed: " + err.Error(), TS: time.Now()})
		}
		return
	}

	var parts []string
	for _, b := range resp.Content {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, b.Text)
		}
	}
	summary := strings.TrimSpace(strings.Join(parts, "\n"))
	if summary == "" {
		s.emit(Event{Type: EventCompaction, Detail: "compaction failed: summarizer returned no text", TS: time.Now()})
		return
	}

	text := "[conversation compacted — this summary replaces the full history]\n\n" + summary
	if pendingTools {
		text += "\n\nContinue the original task from where it left off."
	}
	summaryMsg := anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(text))

	// Undo point: the full history right before the summary replaces it,
	// so a compaction is always reversible (/rollback). Captured only now
	// — a failed summarizer above leaves history untouched and must not
	// litter the undo stack.
	s.captureCheckpoint(CheckpointCompact,
		fmt.Sprintf("before compact (%d msgs)", len(msgs)), msgs)

	if !pendingTools {
		// Final case: the runner is done (or about to be). Replacing
		// Messages lets its completion proceed; the next prompt's
		// buildHistory reads the summary as its history.
		s.runner.Params.Messages = []anthropic.BetaMessageParam{summaryMsg}
	} else {
		// Recreate case: fresh runner seeded with the summary only.
		// MaxIterations counts down what this prompt already spent so the
		// global iteration budget is unchanged; iterOffset keeps prompt_end's
		// iteration total honest across the swap. Client tools come from
		// betaTools (adapted once); server web tools re-append after.
		s.iterOffset += s.runner.IterationCount()
		s.runner = s.eng.client.Beta.Messages.NewToolRunnerStreaming(s.betaTools, anthropic.BetaToolRunnerParams{
			BetaMessageNewParams: s.baseParams([]anthropic.BetaMessageParam{summaryMsg}),
			MaxIterations:        remaining,
		})
		if s.eng.cfg.WebToolMode == "server" {
			s.runner.Params.Tools = append(s.runner.Params.Tools, serverToolDefs()...)
		}
	}

	u := Usage{
		Input:         resp.Usage.InputTokens,
		Output:        resp.Usage.OutputTokens,
		CacheRead:     resp.Usage.CacheReadInputTokens,
		CacheCreation: resp.Usage.CacheCreationInputTokens,
	}
	*turnIdx++
	stat := TurnStat{
		Turn:       *turnIdx,
		StopReason: "compaction",
		Usage:      u,
		Duration:   time.Since(turnStart),
		At:         time.Now(),
	}
	s.record(stat) // renumbers to len(turns)+1 — identical: turnIdx tracks it
	s.emit(Event{
		Type:  EventCompaction,
		Usage: &u,
		Detail: fmt.Sprintf("summary replaced %d messages (%.1fKB → %.1fKB)",
			len(msgs), float64(len(rendered))/1024, float64(len(text))/1024),
		TS: stat.At,
	})
	s.emit(Event{
		Type:       EventTurnEnd,
		Turn:       stat.Turn,
		StopReason: "compaction",
		Usage:      &u,
		DurationMS: stat.Duration.Milliseconds(),
		TS:         stat.At,
	})
}
