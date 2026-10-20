package server

import (
	"sync/atomic"
	"testing"
	"time"

	"lyon-agent/pkg/agent"
)

// TestTitleBeat pins the user-set cadence: gaps of 1, 5, 20, 20 …
func TestTitleBeat(t *testing.T) {
	yes := []int64{1, 6, 26, 46, 66, 86, 106}
	no := []int64{0, 2, 3, 4, 5, 7, 25, 27, 45, 47, 65, 67, 85, 87}
	for _, p := range yes {
		if !titleBeat(p) {
			t.Errorf("titleBeat(%d) = false, want true", p)
		}
	}
	for _, p := range no {
		if titleBeat(p) {
			t.Errorf("titleBeat(%d) = true, want false", p)
		}
	}
}

// TestHubOnPromptEndHook: the tap fires only for prompt_end, after the
// event was stamped — never for other event types.
func TestHubOnPromptEndHook(t *testing.T) {
	events := make(chan agent.Event)
	h := newHub(events)
	var fired atomic.Int64
	h.onPromptEnd = func() { fired.Add(1) }
	done := make(chan struct{})
	go func() {
		h.run()
		close(done)
	}()

	events <- agent.Event{Type: agent.EventMessageStart}
	events <- agent.Event{Type: agent.EventPromptEnd}
	events <- agent.Event{Type: agent.EventTurnEnd}
	events <- agent.Event{Type: agent.EventPromptEnd}
	close(events)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("hub did not drain")
	}
	if got := fired.Load(); got != 2 {
		t.Fatalf("hook fired %d times, want 2 (one per prompt_end)", got)
	}
}
