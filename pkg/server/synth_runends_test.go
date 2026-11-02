package server

import (
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"lyon-agent/pkg/agent"
)

// synthPair is one user prompt and its assistant answer — the two-message
// unit a run's end anchors onto.
func synthPair(q, a string) []anthropic.BetaMessageParam {
	return []anthropic.BetaMessageParam{
		anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(q)),
		{
			Role: anthropic.BetaMessageParamRoleAssistant,
			Content: []anthropic.BetaContentBlockParamUnion{
				{OfText: &anthropic.BetaTextBlockParam{Text: a}},
			},
		},
	}
}

// promptEnds filters the synthesized stream down to its prompt_end events,
// in order — what the web reducer pins duration badges from.
func promptEnds(evs []agent.Event) []agent.Event {
	var out []agent.Event
	for _, ev := range evs {
		if ev.Type == agent.EventPromptEnd {
			out = append(out, ev)
		}
	}
	return out
}

// TestSynthEmitsPromptEndAtRunCounts: aligned anchors re-emit prompt_end
// after exactly the message count each run ended at — badge on reply one
// before prompt two starts, badge on reply two at the end. This is the
// whole point: a restarted seat's transcript must show how long each reply
// took, not a blank where the badge was.
func TestSynthEmitsPromptEndAtRunCounts(t *testing.T) {
	msgs := synthPair("q1", "a1")
	msgs = append(msgs, synthPair("q2", "a2")...)
	runs := []agent.RunEnd{
		{DurationMS: 111, Messages: 2, StopReason: "end_turn"},
		{DurationMS: 222, Messages: 4, StopReason: "end_turn"},
	}

	evs := synthSnapshotEvents(msgs, runs)
	ends := promptEnds(evs)
	if len(ends) != 2 {
		t.Fatalf("prompt_end count = %d, want 2", len(ends))
	}
	if ends[0].DurationMS != 111 || ends[1].DurationMS != 222 {
		t.Fatalf("durations = %d,%d want 111,222", ends[0].DurationMS, ends[1].DurationMS)
	}

	// Position: the first prompt_end lands between a1's text delta and
	// q2's user_message — the reply it times is the one before it.
	var sawFirstEnd, sawQ2 bool
	for _, ev := range evs {
		switch {
		case ev.Type == agent.EventPromptEnd && ev.DurationMS == 111:
			if sawQ2 {
				t.Fatal("first prompt_end after the second prompt — it would time the wrong reply")
			}
			sawFirstEnd = true
		case ev.Type == evUserMessage && ev.Text == "q2":
			if !sawFirstEnd {
				t.Fatal("second prompt before the first run's prompt_end")
			}
			sawQ2 = true
		}
	}
	if !sawFirstEnd || !sawQ2 {
		t.Fatalf("stream shape wrong: firstEnd=%v sawQ2=%v", sawFirstEnd, sawQ2)
	}
}

// TestSynthPromptEndCarriesStopAndErr: the anchor keeps why the run ended,
// so a replayed max_tokens notice or error line matches what live showed.
func TestSynthPromptEndCarriesStopAndErr(t *testing.T) {
	msgs := synthPair("q", "a")
	evs := synthSnapshotEvents(msgs, []agent.RunEnd{
		{DurationMS: 42, Messages: 2, StopReason: "max_tokens", Err: "interrupted"},
	})
	ends := promptEnds(evs)
	if len(ends) != 1 {
		t.Fatalf("prompt_end count = %d, want 1", len(ends))
	}
	if ends[0].StopReason != "max_tokens" || ends[0].Err != "interrupted" || ends[0].DurationMS != 42 {
		t.Fatalf("prompt_end = %+v, want the anchor's stop/err/duration", ends[0])
	}
}

// TestSynthRunEndsGuard: anchors that do not walk forward inside this
// message space are dropped from the first violation on — the intact
// prefix keeps its badges, everything past a bad anchor replays without
// them. Wrong badges are worse than missing ones.
func TestSynthRunEndsGuard(t *testing.T) {
	msgs := synthPair("q1", "a1")
	msgs = append(msgs, synthPair("q2", "a2")...)
	cases := []struct {
		name string
		runs []agent.RunEnd
		want int // expected prompt_end count
	}{
		{"no anchors (old snapshot)", nil, 0},
		{"count past history", []agent.RunEnd{{DurationMS: 1, Messages: 9}}, 0},
		{"not increasing", []agent.RunEnd{
			{DurationMS: 1, Messages: 2},
			{DurationMS: 2, Messages: 2},
		}, 1}, // first anchor valid, second dropped
		{"crash tail: last run short of history", []agent.RunEnd{
			{DurationMS: 1, Messages: 2},
		}, 1}, // badge on reply one; the dangling q2 gets none
		{"zero count", []agent.RunEnd{{DurationMS: 1, Messages: 0}}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := len(promptEnds(synthSnapshotEvents(msgs, tc.runs))); got != tc.want {
				t.Fatalf("prompt_end count = %d, want %d", got, tc.want)
			}
		})
	}
}
