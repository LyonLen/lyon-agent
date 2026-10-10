package agent

import (
	"os"
	"testing"
	"time"
)

// waitPromptEnd drains events until prompt_end, failing the test on errors.
func waitPromptEnd(t *testing.T, sess *Session) {
	t.Helper()
	deadline := time.After(3 * time.Minute)
	for {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				t.Fatal("event stream closed before prompt_end")
			}
			if ev.Type == EventPromptEnd {
				if ev.Err != "" {
					t.Fatalf("prompt failed: %s", ev.Err)
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for prompt_end")
		}
	}
}

// TestCacheHitsLive verifies the end-to-end prompt-cache signature against
// the real endpoint: the second prompt of one session must read cache.
// Gated by LYON_LIVE_TEST=1 because it spends real tokens.
func TestCacheHitsLive(t *testing.T) {
	if os.Getenv("LYON_LIVE_TEST") == "" {
		t.Skip("set LYON_LIVE_TEST=1 to run live API tests")
	}

	eng := NewEngine(Config{Effort: "low", MaxIterations: 2}, nil)
	sess := eng.NewSession(nil)
	defer sess.Close()

	if err := sess.Submit("Reply with exactly the word OK."); err != nil {
		t.Fatalf("submit 1: %v", err)
	}
	waitPromptEnd(t, sess)

	if err := sess.Submit("Reply with exactly the word DONE."); err != nil {
		t.Fatalf("submit 2: %v", err)
	}
	waitPromptEnd(t, sess)

	stats := sess.Stats()
	if len(stats) < 2 {
		t.Fatalf("want at least 2 turns, got %d", len(stats))
	}
	last := stats[len(stats)-1]
	t.Logf("second prompt turn: in=%d out=%d cache_read=%d cache_creation=%d",
		last.Usage.Input, last.Usage.Output, last.Usage.CacheRead, last.Usage.CacheCreation)
	if last.Usage.CacheRead <= 0 {
		t.Fatalf("expected CacheReadInputTokens > 0 on the second prompt, got %+v (all turns: %+v)",
			last.Usage, stats)
	}
}
