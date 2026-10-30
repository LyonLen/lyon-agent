package agent

import "testing"

// drive runs a sequence through one hygiene state (flushing at the end,
// as the run loop does when the stream closes) and returns everything
// that would be emitted, in order.
func drive(evs ...Event) []Event {
	var h narrationHygiene
	var out []Event
	for _, e := range evs {
		out = append(out, h.filter(e)...)
	}
	if p := h.flush(); p != nil {
		out = append(out, *p)
	}
	return out
}

// texts collects the text_delta payloads in emission order.
func texts(evs []Event) []string {
	var out []string
	for _, e := range evs {
		if e.Type == EventTextDelta {
			out = append(out, e.Text)
		}
	}
	return out
}

func seqTypes(evs []Event) []EventType {
	out := make([]EventType, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Type)
	}
	return out
}

// The observed live defect: the narration block opens on a comma.
func TestNarrationStripsLeadingComma(t *testing.T) {
	got := drive(
		Event{Type: EventMessageStart},
		Event{Type: EventTextDelta, Text: "，这是莎姐对 P4 的更新。"},
		Event{Type: EventToolUseStart, ToolName: "run_shell"},
	)
	ts := texts(got)
	if len(ts) != 1 || ts[0] != "这是莎姐对 P4 的更新。" {
		t.Fatalf("texts = %q", ts)
	}
	if seqTypes(got)[len(got)-1] != EventToolUseStart {
		t.Fatalf("tool_use must stay last: %v", seqTypes(got))
	}
}

// A delta that is nothing but punctuation is swallowed whole — the next
// delta becomes the block opener.
func TestNarrationSwallowsPunctOnlyDelta(t *testing.T) {
	got := drive(
		Event{Type: EventMessageStart},
		Event{Type: EventTextDelta, Text: "，"},
		Event{Type: EventTextDelta, Text: "这是新的。"},
		Event{Type: EventToolUseStart, ToolName: "run_shell"},
	)
	ts := texts(got)
	if len(ts) != 1 || ts[0] != "这是新的。" {
		t.Fatalf("texts = %q", ts)
	}
}

// The other observed defect: trailing off mid-phrase (with a trailing
// space) right before the tool card — close it with 。, without a gap.
func TestNarrationClosesCJKBeforeTool(t *testing.T) {
	got := drive(
		Event{Type: EventMessageStart},
		Event{Type: EventTextDelta, Text: "确认 git 仓库，读取最近 5 "},
		Event{Type: EventToolUseStart, ToolName: "run_shell"},
	)
	ts := texts(got)
	want := []string{"确认 git 仓库，读取最近 5", "。"}
	if len(ts) != 2 || ts[0] != want[0] || ts[1] != want[1] {
		t.Fatalf("texts = %q, want %q", ts, want)
	}
}

func TestNarrationEnglishPeriod(t *testing.T) {
	got := drive(
		Event{Type: EventMessageStart},
		Event{Type: EventTextDelta, Text: "Let me check the files"},
		Event{Type: EventToolUseStart, ToolName: "run_shell"},
	)
	if ts := texts(got); len(ts) != 2 || ts[1] != "." {
		t.Fatalf("texts = %q, want a trailing \".\"", ts)
	}
}

// Lines that already close themselves are left alone.
func TestNarrationAlreadyTerminated(t *testing.T) {
	for _, line := range []string{"先列出目录。", "Check the log.", "真麻烦！", "先做两件事："} {
		got := drive(
			Event{Type: EventMessageStart},
			Event{Type: EventTextDelta, Text: line},
			Event{Type: EventToolUseStart, ToolName: "run_shell"},
		)
		if ts := texts(got); len(ts) != 1 || ts[0] != line {
			t.Fatalf("%q: texts = %q, want unchanged", line, ts)
		}
	}
}

// One narration, two tool calls in the same message: one full stop.
func TestNarrationOneStopPerRun(t *testing.T) {
	got := drive(
		Event{Type: EventMessageStart},
		Event{Type: EventTextDelta, Text: "并行读两个文件"},
		Event{Type: EventToolUseStart, ToolName: "read_file"},
		Event{Type: EventToolUseStart, ToolName: "read_file"},
	)
	if ts := texts(got); len(ts) != 2 || ts[1] != "。" {
		t.Fatalf("texts = %q, want exactly one 。", ts)
	}
}

// thinking between narration and tool: the stop lands BEFORE the
// thinking card so the narration block closes in place.
func TestNarrationClosesBeforeThinking(t *testing.T) {
	got := drive(
		Event{Type: EventMessageStart},
		Event{Type: EventTextDelta, Text: "先看看配置"},
		Event{Type: EventThinkingDelta, Thinking: "hmm"},
		Event{Type: EventToolUseStart, ToolName: "run_shell"},
	)
	ts := texts(got)
	want := []string{"先看看配置", "。"}
	if len(ts) != 2 || ts[0] != want[0] || ts[1] != want[1] {
		t.Fatalf("texts = %q, want %q", ts, want)
	}
	// …and the later tool must not add a second one.
	if n := len(ts); n != 2 {
		t.Fatalf("second stop injected: %q", ts)
	}
}

// message_start closes an open line, flushes it, and resets the state —
// the next message narrates (and closes) independently.
func TestNarrationResetOnMessageStart(t *testing.T) {
	got := drive(
		Event{Type: EventMessageStart},
		Event{Type: EventTextDelta, Text: "第一段"},
		Event{Type: EventMessageStart},
		Event{Type: EventTextDelta, Text: "第二段"},
		Event{Type: EventToolUseStart, ToolName: "run_shell"},
	)
	ts := texts(got)
	want := []string{"第一段", "。", "第二段", "。"}
	if len(ts) != len(want) {
		t.Fatalf("texts = %q, want %q", ts, want)
	}
	for i := range want {
		if ts[i] != want[i] {
			t.Fatalf("texts = %q, want %q", ts, want)
		}
	}
}

// Final answers: deltas stream out (one behind) and the tail goes
// through flush() untouched — no synthetic stop on an answer that
// already ends with one, and no mangling of an open-ended answer.
func TestNarrationAnswerStreamsUntouched(t *testing.T) {
	got := drive(
		Event{Type: EventMessageStart},
		Event{Type: EventTextDelta, Text: "答案是 28"},
		Event{Type: EventTextDelta, Text: "，条目。"},
	)
	ts := texts(got)
	want := []string{"答案是 28", "，条目。"}
	if len(ts) != 2 || ts[0] != want[0] || ts[1] != want[1] {
		t.Fatalf("texts = %q, want %q", ts, want)
	}
}
