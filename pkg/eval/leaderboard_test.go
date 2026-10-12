package eval

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// scratchHome points LYON_HOME at a fresh temp dir and schedules a
// retrying cleanup: Windows file scanners can hold a just-written file
// just long enough that the framework's immediate RemoveAll hits a
// delete-pending entry ("directory not empty"). No parallel tests in
// this package — env is process-wide.
func scratchHome(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() {
		var err error
		for i := 0; i < 40; i++ {
			if err = os.RemoveAll(dir); err == nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Logf("settle cleanup failed: %v", err)
	})
	t.Setenv("LYON_HOME", dir)
}

// lbFor points the ledger at a scratch LYON_HOME.
func lbFor(t *testing.T) *Leaderboard {
	t.Helper()
	scratchHome(t)
	return OpenLeaderboard()
}

func prodCard(agent string, total float64, passed bool) Scorecard {
	return Scorecard{Agent: agent, Total: total, Passed: passed, Source: "production"}
}

func TestElectionHighestEligibleWins(t *testing.T) {
	lb := lbFor(t)
	lb.Record("coding", prodCard("weak", 0.99, false)) // best score, failed conditions
	lb.Record("coding", prodCard("ok", 0.70, true))
	lb.Record("coding", prodCard("strong", 0.90, true))
	if got := lb.OnDuty("coding"); got != "strong" {
		t.Fatalf("on duty = %q, want strong (highest among condition-passers)", got)
	}
	// weak improves on the next attempt → re-election flips.
	lb.Record("coding", prodCard("weak", 0.95, true))
	if got := lb.OnDuty("coding"); got != "weak" {
		t.Fatalf("on duty = %q, want weak after it qualified", got)
	}
}

func TestElectionMeanOverRecentWindowAndTieBreak(t *testing.T) {
	lb := lbFor(t)
	// Same latest pass, different means: b's average is higher.
	lb.Record("c", prodCard("a", 0.60, true))
	lb.Record("c", prodCard("b", 0.90, true))
	if got := lb.OnDuty("c"); got != "b" {
		t.Fatalf("on duty = %q, want b", got)
	}
	// Exact tie across candidates → lexicographic, deterministic.
	lb.Record("tie", prodCard("a", 0.90, true))
	lb.Record("tie", prodCard("b", 0.90, true))
	if got := lb.OnDuty("tie"); got != "a" {
		t.Fatalf("tied on-duty = %q, want a (stable tie-break)", got)
	}
}

func TestRoundClockAndExamDue(t *testing.T) {
	lb := lbFor(t)
	lb.Record("c", prodCard("a", 0.9, true))
	for i := 1; i < ExamInterval; i++ {
		_, due := lb.CountRound("c")
		if due {
			t.Fatalf("exam due at round %d", i)
		}
	}
	rounds, due := lb.CountRound("c")
	if rounds != ExamInterval || !due {
		t.Fatalf("rounds=%d due=%v at the tenth round", rounds, due)
	}
	// A vacant seat never triggers exams.
	lb2 := lbFor(t)
	if _, due := lb2.CountRound("vacant"); due {
		t.Fatal("vacant seat must not schedule exams")
	}
}

func TestSubmitExamClockResetAndDemotion(t *testing.T) {
	lb := lbFor(t)
	lb.Record("c", prodCard("a", 0.9, true))
	lb.CountRound("c")

	// Passing exam: clock resets, seat kept, streak cleared.
	if demoted := lb.SubmitExam("c", Scorecard{Agent: "a", Passed: true}); demoted {
		t.Fatal("passing exam must not demote")
	}
	if on, rounds, fails := lb.Status("c"); on != "a" || rounds != 0 || fails != 0 {
		t.Fatalf("after pass: on=%q rounds=%d fails=%d", on, rounds, fails)
	}

	// First failure: seat kept, streak = 1.
	if demoted := lb.SubmitExam("c", Scorecard{Agent: "a", Passed: false}); demoted {
		t.Fatal("single failure must not demote")
	}
	if on, _, fails := lb.Status("c"); on != "a" || fails != 1 {
		t.Fatalf("after 1st fail: on=%q fails=%d", on, fails)
	}
	// Second failure: seat opens (design §1.4).
	if demoted := lb.SubmitExam("c", Scorecard{Agent: "a", Passed: false}); !demoted {
		t.Fatal("second consecutive failure must demote")
	}
	if on, _, fails := lb.Status("c"); on != "" || fails != 0 {
		t.Fatalf("after demotion: on=%q fails=%d", on, fails)
	}
}

func TestExamCardsAloneNeverHandOutTheSeat(t *testing.T) {
	lb := lbFor(t)
	// Exam-only candidate passes → gets the seat (nothing better exists).
	lb.SubmitExam("c", Scorecard{Agent: "exam-only", Passed: true})
	if got := lb.OnDuty("c"); got != "exam-only" {
		t.Fatalf("on duty = %q", got)
	}
	// Production candidate outranks exam-only on election day.
	lb.Record("c", prodCard("prod", 0.9, true))
	if got := lb.OnDuty("c"); got != "prod" {
		t.Fatalf("on duty = %q, want prod", got)
	}
}

func TestLeaderboardPersistsAcrossReloads(t *testing.T) {
	scratchHome(t)
	dir := os.Getenv("LYON_HOME")
	lb := OpenLeaderboard()
	lb.Record("c", prodCard("a", 0.9, true))
	lb.CountRound("c")

	lb2 := OpenLeaderboard()
	if got := lb2.OnDuty("c"); got != "a" {
		t.Fatalf("reloaded on duty = %q", got)
	}
	if _, rounds, _ := lb2.Status("c"); rounds != 1 {
		t.Fatalf("reloaded rounds = %d, want 1", rounds)
	}
	// Corrupt file → empty board, never fatal.
	if err := os.WriteFile(filepath.Join(dir, "leaderboard.json"), []byte("{nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := OpenLeaderboard().OnDuty("c"); got != "" {
		t.Fatalf("corrupt board on duty = %q", got)
	}
}

// TestViewsSnapshot: the render snapshot follows the requested class
// order, keeps empty classes for stable panel rows, sorts ledger extras
// after the requested set, and computes the on-duty mean over the recent
// window — all under one lock, so the TUI never races Record.
func TestViewsSnapshot(t *testing.T) {
	lb := lbFor(t)
	lb.Record("fix", Scorecard{Agent: "b-model", Total: 0.8, Passed: true})
	lb.Record("fix", Scorecard{Agent: "b-model", Total: 0.6, Passed: true})
	lb.Record("zzz-extra", Scorecard{Agent: "c-model", Total: 1.0, Passed: true})

	views := lb.Views([]string{"coding", "fix", "general"})
	want := []string{"coding", "fix", "general", "zzz-extra"}
	if len(views) != len(want) {
		t.Fatalf("views: %d rows, want %d", len(views), len(want))
	}
	for i, w := range want {
		if views[i].Class != w {
			t.Fatalf("views[%d] = %q, want %q", i, views[i].Class, w)
		}
	}
	if empty := views[0]; empty.OnDuty != "" || empty.Cards != 0 {
		t.Fatalf("empty class snapshot: %+v", empty)
	}
	fix := views[1]
	if fix.OnDuty != "b-model" || fix.Cards != 2 {
		t.Fatalf("fix snapshot: %+v", fix)
	}
	if math.Abs(fix.Mean-0.7) > 1e-9 {
		t.Fatalf("fix mean: %v, want 0.7", fix.Mean)
	}
	if fix.LastAgent != "b-model" || fix.LastAt.IsZero() || !fix.LastPassed {
		t.Fatalf("fix last card: %+v", fix)
	}
	if extra := views[3]; extra.Cards != 1 || extra.OnDuty != "c-model" {
		t.Fatalf("extra snapshot: %+v", extra)
	}
}
