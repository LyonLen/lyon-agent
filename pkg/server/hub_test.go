package server

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"lyon-agent/pkg/agent"
)

// drainOne reads one hub item with a timeout guard so a broken hub fails
// the test instead of hanging it.
func drainOne(t *testing.T, sub *hubSub) (hubItem, bool) {
	t.Helper()
	select {
	case it, ok := <-sub.ch:
		return it, ok
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a hub event")
		return hubItem{}, false
	}
}

func TestHubFanoutSeqAndReplay(t *testing.T) {
	events := make(chan agent.Event)
	h := newHub(events)
	go h.run()

	// Live subscriber attached before the first event: sees seq 1, 2.
	live := h.subscribeLive()
	defer h.unsubscribe(live)

	events <- agent.Event{Type: agent.EventMessageStart}
	it, ok := drainOne(t, live)
	if !ok {
		t.Fatal("live sub closed early")
	}
	if it.seq != 1 || it.ev.Type != agent.EventMessageStart {
		t.Fatalf("first event = seq %d %s, want seq 1 message_start", it.seq, it.ev.Type)
	}
	events <- agent.Event{Type: agent.EventTextDelta, Text: "hi"}
	it, ok = drainOne(t, live)
	if !ok || it.seq != 2 || it.ev.Text != "hi" {
		t.Fatalf("second event = seq %d %+v, want seq 2 text_delta %q", it.seq, it.ev.Type, it.ev.Text)
	}

	// Replay semantics on fresh subscribers (web plan §WatchEvents).
	cases := []struct {
		fromSeq int64
		wantSeq []int64
	}{
		{0, nil},            // live only
		{-1, []int64{1, 2}}, // whole ring
		{1, []int64{2}},     // strictly after anchor
		{2, nil},            // exactly current: nothing to replay
	}
	for _, tc := range cases {
		sub, replay, err := h.subscribe(tc.fromSeq)
		if err != nil {
			t.Fatalf("subscribe(%d): %v", tc.fromSeq, err)
		}
		if len(replay) != len(tc.wantSeq) {
			t.Fatalf("subscribe(%d) replay = %d items, want %d", tc.fromSeq, len(replay), len(tc.wantSeq))
		}
		for i, want := range tc.wantSeq {
			if replay[i].seq != want {
				t.Fatalf("subscribe(%d) replay[%d] = seq %d, want %d", tc.fromSeq, i, replay[i].seq, want)
			}
		}
		h.unsubscribe(sub)
	}

	// Anchor from beyond the ring: ErrRingGap, nothing subscribed.
	if _, _, err := h.subscribe(7); !errors.Is(err, ErrRingGap) {
		t.Fatalf("subscribe(7) = %v, want ErrRingGap", err)
	}

	// Session close: the event chan closes, run drains it and closes every
	// subscriber — a watcher sees EOF, not a hang.
	close(events)
	if _, ok := drainOne(t, live); ok {
		t.Fatal("live sub still open after session close")
	}
}

func TestHubKicksSlowSubscriber(t *testing.T) {
	events := make(chan agent.Event)
	h := newHub(events)
	go h.run()

	sub := h.subscribeLive()
	// Never read from sub: flood past its bounded channel. The hub must
	// kick, not block the driver.
	for i := 0; i < hubSubBuffer+2; i++ {
		select {
		case events <- agent.Event{Type: agent.EventTextDelta}:
		case <-time.After(2 * time.Second):
			t.Fatal("hub blocked on a slow subscriber (driver stall)")
		}
	}

	deadline := time.After(2 * time.Second)
	drained := 0
	for {
		select {
		case _, ok := <-sub.ch:
			if !ok {
				if !sub.kicked {
					t.Fatal("slow sub closed without kick")
				}
				goto kicked
			}
			drained++
			if drained > hubSubBuffer+2 {
				t.Fatal("slow sub channel never closed")
			}
		case <-deadline:
			t.Fatal("timed out draining the kicked subscriber")
		}
	}

kicked:
	// The hub is unharmed: a fresh fast subscriber still gets events with
	// the stamp continuing after the flooded ones.
	fast := h.subscribeLive()
	events <- agent.Event{Type: agent.EventPromptEnd}
	it, ok := drainOne(t, fast)
	if !ok || it.seq != hubSubBuffer+3 || it.ev.Type != agent.EventPromptEnd {
		t.Fatalf("post-kick event = seq %d ok=%v, want seq %d prompt_end", it.seq, ok, hubSubBuffer+3)
	}
	close(events)
}

func TestSanitizeEventUTF8(t *testing.T) {
	bad := "ok\xffbad"
	ev := sanitizeEvent(agent.Event{
		Type:  agent.EventTextDelta,
		Text:  bad,
		Input: json.RawMessage(`{"k":"` + bad + `"}`),
	})
	if !utf8.ValidString(ev.Text) || ev.Text != "ok�bad" {
		t.Fatalf("Text = %q, want valid UTF-8 ok<U+FFFD>bad", ev.Text)
	}
	if !utf8.ValidString(string(ev.Input)) {
		t.Fatalf("Input = %q, not valid UTF-8", string(ev.Input))
	}
	if !strings.ContainsRune(string(ev.Input), '�') {
		t.Fatalf("Input replacement missing: %q", string(ev.Input))
	}

	// Valid input passes through untouched (SSE frame bytes stay stable).
	good := "héllo"
	out := sanitizeEvent(agent.Event{Type: agent.EventTextDelta, Text: good})
	if out.Text != good {
		t.Fatalf("valid Text rewritten: %q", out.Text)
	}
}
