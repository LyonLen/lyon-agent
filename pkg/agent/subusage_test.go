package agent

import "testing"

// TestSubUsageAccounting: per-model accumulation, copy isolation, and
// the clear-on-Restore contract (a new session never inherits the old
// session's subagent bill).
func TestSubUsageAccounting(t *testing.T) {
	s := &Session{}
	if got := s.SubUsage(); len(got) != 0 {
		t.Fatalf("fresh SubUsage = %v, want empty", got)
	}

	s.recordSub("cheap", Usage{Input: 10, Output: 5, CacheRead: 3})
	s.recordSub("cheap", Usage{Input: 1, Output: 2, CacheCreation: 4})
	s.recordSub("main", Usage{Input: 100})

	got := s.SubUsage()
	if len(got) != 2 {
		t.Fatalf("models = %d, want 2", len(got))
	}
	if c := got["cheap"]; c.Input != 11 || c.Output != 7 ||
		c.CacheRead != 3 || c.CacheCreation != 4 {
		t.Fatalf("cheap totals = %+v", c)
	}
	if m := got["main"]; m.Input != 100 {
		t.Fatalf("main totals = %+v", m)
	}

	// Returned map is a copy: mutating it must not touch the session.
	got["cheap"] = Usage{Input: 9999}
	if again := s.SubUsage(); again["cheap"].Input != 11 {
		t.Fatalf("SubUsage leaked internal map: %+v", again["cheap"])
	}

	// Restore clears usage together with turn stats. Restore reads the
	// workspace through the engine — give the bare Session one.
	s.eng = &Engine{cfg: Config{Workspace: t.TempDir()}}
	s.Restore(Snapshot{Workspace: t.TempDir()})
	if after := s.SubUsage(); len(after) != 0 {
		t.Fatalf("Restore kept subagent usage: %v", after)
	}
}
