package agent

import (
	"strings"
	"unicode"
)

// narrationHygiene fixes the two narration defects models produce before
// tool_use blocks, at the source so web and TUI both render clean lines
// without touching either renderer:
//
//  1. a line that opens with punctuation — the observed defect was
//     「，这是莎姐对 P4…」, the text block literally starting on a comma;
//  2. a line that trails off mid-phrase — 「读取最近 5 」 — instead of
//     ending with a full stop before the next block.
//
// The system prompt asks for both (DefaultSystemPrompt, the Narrate
// bullet); the model obeys roughly half the time, so this state machine
// guarantees the floor. It holds at most one text delta: the last delta
// of a run is the only place a trailing space can be trimmed before a
// synthetic full stop is appended, and nothing is sent until we know
// whether more text follows. One delta of latency on live text is
// imperceptible (deltas arrive in bursts).
//
// The run loop owns the value (single goroutine); zero value is ready.
type narrationHygiene struct {
	held     *Event // last text delta, not yet emitted
	midBlock bool   // the next text delta continues the current text block
	msgText  bool   // the current message has visible text
	lastR    rune   // last non-space rune of that text
	hasCJK   bool   // any CJK rune seen — picks 。 over .
}

// narrationLeadPunct: characters a text block must never open with.
// Comma is the observed defect; the close cousins ride along. Quotes and
// brackets are deliberately absent — an answer may legitimately open
// with one.
const narrationLeadPunct = "，,。、；;"

// narrationTerminates reports whether the line already closes itself.
// The colon pair is included because appending 。 after a colon lead-in
// (「先做两件事：」) reads worse than the tail-off it prevents.
func narrationTerminates(r rune) bool {
	switch r {
	case '。', '.', '！', '？', '?', '!', '：', ':':
		return true
	}
	return false
}

// filter turns one normalized stream event into the events to emit, in
// order: possibly a previously held text delta, possibly a synthetic
// full stop closing the narration, then the event itself. Text deltas
// are held back (returned on the NEXT call or flush); every other event
// flushes the hold first and closes an open narration line.
func (h *narrationHygiene) filter(e Event) []Event {
	switch e.Type {
	case EventTextDelta:
		if !h.midBlock {
			e.Text = strings.TrimLeft(e.Text, narrationLeadPunct)
			if e.Text == "" {
				return nil // punctuation-only opener: swallow it whole
			}
		}
		visible := false
		for _, r := range e.Text {
			if r >= 0x2E80 {
				h.hasCJK = true
			}
			if !unicode.IsSpace(r) {
				h.lastR, visible = r, true
			}
		}
		if visible {
			h.msgText = true
		}
		h.midBlock = true
		out := h.detach()
		h.held = &e
		return out

	default:
		// message_start, thinking, tool cards, server results — anything
		// that splits text blocks downstream. The hold lands first (a
		// tool gets the trailing-space trim so 「5 」 + 。 never renders
		// as 「5 。」), an unclosed narration gets its full stop, then
		// the event.
		var out []Event
		if e.Type == EventToolUseStart {
			out = h.detachTrim()
		} else {
			out = h.detach()
		}
		out = h.closeNarration(out)
		if e.Type == EventMessageStart {
			h.midBlock, h.lastR = false, 0
		} else {
			h.midBlock = false
		}
		return append(out, e)
	}
}

// flush returns the held delta at stream end — final answers close
// themselves, so the text goes out untouched.
func (h *narrationHygiene) flush() *Event {
	return h.detachOne()
}

// closeNarration appends the synthetic full stop when the visible text
// run is still open, and marks it closed. Any non-text event means the
// run ended as far as the renderer's block layout is concerned.
func (h *narrationHygiene) closeNarration(out []Event) []Event {
	if h.msgText && !narrationTerminates(h.lastR) {
		stop := "."
		if h.hasCJK {
			stop = "。"
		}
		out = append(out, Event{Type: EventTextDelta, Text: stop})
		h.lastR = []rune(stop)[0]
	}
	h.msgText, h.hasCJK = false, false
	return out
}

// detach emits the held delta as-is (mid-run flush).
func (h *narrationHygiene) detach() []Event {
	if e := h.detachOne(); e != nil {
		return []Event{*e}
	}
	return nil
}

// detachTrim emits the held delta with trailing whitespace removed —
// only used when a tool_use follows, so 「读取最近 5 」 + 。 lands as
// 「读取最近 5。」, never 「5 。」. A whitespace-only delta is dropped.
func (h *narrationHygiene) detachTrim() []Event {
	e := h.detachOne()
	if e == nil {
		return nil
	}
	e.Text = strings.TrimRight(e.Text, " \t\r\n")
	if e.Text == "" {
		return nil
	}
	return []Event{*e}
}

func (h *narrationHygiene) detachOne() *Event {
	e := h.held
	h.held = nil
	return e
}
