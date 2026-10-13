package eval

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCandidateFieldLifecycle: profiles round-trip through
// $LYON_HOME/agents, the live "self" incumbent is always in the field
// (a file claiming id "self" cannot shadow it), and the built-in cannot
// be overwritten or deleted.
func TestCandidateFieldLifecycle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("LYON_HOME", home)

	// Empty field: Contestants is just the incumbent.
	cands := Contestants("main-model")
	if len(cands) != 1 || cands[0].ID != "self" || cands[0].Model != "main-model" {
		t.Fatalf("bare field = %+v, want only self/main-model", cands)
	}

	// Save two entrants; field grows, sorted by id.
	if err := SaveCandidate(Candidate{ID: "zeta", Model: "model-z"}); err != nil {
		t.Fatal(err)
	}
	if err := SaveCandidate(Candidate{ID: "alpha", Model: "model-a", Effort: "high"}); err != nil {
		t.Fatal(err)
	}
	cands = Contestants("main-model")
	if len(cands) != 3 {
		t.Fatalf("field size = %d, want 3", len(cands))
	}
	if cands[0].ID != "alpha" || cands[1].ID != "self" || cands[2].ID != "zeta" {
		var ids []string
		for _, c := range cands {
			ids = append(ids, c.ID)
		}
		t.Fatalf("field order = %v, want alpha < self < zeta", ids)
	}
	if cands[0].Effort != "high" {
		t.Fatalf("effort lost in round-trip: %+v", cands[0])
	}

	// The on-disk file actually landed where the ledger says.
	if _, err := os.Stat(filepath.Join(home, "agents", "alpha.json")); err != nil {
		t.Fatalf("profile file missing: %v", err)
	}

	// self is untouchable both ways.
	if err := SaveCandidate(Candidate{ID: "self", Model: "impostor"}); err != ErrSelfCandidate {
		t.Fatalf("SaveCandidate(self) = %v, want ErrSelfCandidate", err)
	}
	if err := DeleteCandidate("self"); err != ErrSelfCandidate {
		t.Fatalf("DeleteCandidate(self) = %v, want ErrSelfCandidate", err)
	}

	// rm removes one entrant; unknown ids are reported, not ignored.
	if err := DeleteCandidate("zeta"); err != nil {
		t.Fatal(err)
	}
	if n := len(Contestants("main-model")); n != 2 {
		t.Fatalf("field after rm = %d, want 2", n)
	}
	if err := DeleteCandidate("zeta"); err == nil {
		t.Fatal("deleting a missing candidate must report an error")
	}

	// Hostile ids never reach the filesystem.
	if err := SaveCandidate(Candidate{ID: "../evil", Model: "x"}); err == nil {
		t.Fatal("path-traversal id must be rejected")
	}
	if err := SaveCandidate(Candidate{ID: "ok", Model: ""}); err == nil {
		t.Fatal("model-less candidate must be rejected")
	}
}

// TestExamStreakBelongsToSittingAgent: with a full field submitting exam
// cards, only the ON-DUTY agent's own failures advance the demotion
// clock — a challenger's flunk must never shorten the sitting agent's
// leash, and a vacant seat has no clock at all.
func TestExamStreakBelongsToSittingAgent(t *testing.T) {
	t.Setenv("LYON_HOME", t.TempDir())
	lb := OpenLeaderboard()

	// Seat the incumbent on a passing production card.
	lb.Record("coding", Scorecard{Agent: "sitter", Total: 0.8, Passed: true})
	if got := lb.OnDuty("coding"); got != "sitter" {
		t.Fatalf("on-duty = %q, want sitter", got)
	}

	// Challenger fails twice — the streak belongs to nobody on duty
	// touching their own record: sitter's clock must not move.
	for i := 0; i < 2; i++ {
		lb.SubmitExam("coding", Scorecard{Agent: "challenger", Total: 0.2, Passed: false})
	}
	if _, _, fails := lb.Status("coding"); fails != 0 {
		t.Fatalf("challenger failures moved the streak: fails=%d", fails)
	}
	if got := lb.OnDuty("coding"); got != "sitter" {
		t.Fatalf("seat lost to a challenger's flunk: %q", got)
	}

	// Sitter's first failure: streak 1, still seated.
	lb.SubmitExam("coding", Scorecard{Agent: "sitter", Total: 0.5, Passed: false})
	if _, _, fails := lb.Status("coding"); fails != 1 {
		t.Fatalf("sitter fails = %d, want 1", fails)
	}
	// Second consecutive failure: demoted, seat open, clock cleared.
	if demoted := lb.SubmitExam("coding", Scorecard{Agent: "sitter", Total: 0.4, Passed: false}); !demoted {
		t.Fatal("second consecutive sitting failure must demote")
	}
	onDuty, _, fails := lb.Status("coding")
	if onDuty != "" || fails != 0 {
		t.Fatalf("after demotion: seat=%q fails=%d, want open/0", onDuty, fails)
	}
}

// TestElectAfterContest: the contest's closing step — every card filed,
// one election on the combined recent-window mean. The higher-evidence
// entrant takes (or retakes) the seat regardless of who filed last.
func TestElectAfterContest(t *testing.T) {
	t.Setenv("LYON_HOME", t.TempDir())
	lb := OpenLeaderboard()

	// Two exam-only entrants: hot passed at 0.9, cold at 0.6.
	lb.SubmitExam("research", Scorecard{Agent: "hot", Total: 0.9, Passed: true})
	lb.SubmitExam("research", Scorecard{Agent: "cold", Total: 0.6, Passed: true})

	if got := lb.Elect("research"); got != "hot" {
		t.Fatalf("Elect = %q, want hot (higher mean)", got)
	}
	// Replaying the election is deterministic — same evidence, same winner.
	if got := lb.Elect("research"); got != "hot" {
		t.Fatalf("re-Elect = %q, want stable hot", got)
	}

	// A stronger challenger's exam evidence outbids the incumbent's
	// stale production record at the next contest.
	lb.Record("research", Scorecard{Agent: "incumbent", Total: 0.7, Passed: true})
	if got := lb.OnDuty("research"); got == "" {
		t.Fatal("production card should seat the incumbent")
	}
	lb.SubmitExam("research", Scorecard{Agent: "challenger", Total: 0.95, Passed: true})
	if got := lb.Elect("research"); got != "challenger" {
		t.Fatalf("Elect after challenger 0.95 = %q, want challenger", got)
	}
}
