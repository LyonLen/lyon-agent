package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"lyon-agent/pkg/eval"
)

// TestRenderEvolve: the evolution panel honors the renderSteps contract —
// exactly h rows, content rows padded to w cells ("" filler rows allowed),
// shown at every size — with the empty-state hints on a fresh ledger, and
// class rows with seat, exam clock and mean once scorecards exist; ten
// booked rounds light the EXAM flag.
func TestRenderEvolve(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))
	m := newTestModel(t)

	for _, tc := range []struct{ w, h int }{{40, 20}, {28, 12}, {72, 40}, {40, 1}, {40, 2}} {
		rows := m.renderEvolve(tc.w, tc.h)
		if len(rows) != tc.h {
			t.Fatalf("renderEvolve(%d,%d): %d rows, want %d", tc.w, tc.h, len(rows), tc.h)
		}
		for i, r := range rows {
			if r == "" {
				continue // filler rows below the body, per renderSteps
			}
			if n := ansi.StringWidth(r); n != tc.w {
				t.Fatalf("renderEvolve(%d,%d): row %d is %d cells, want %d: %q",
					tc.w, tc.h, i, n, tc.w, r)
			}
		}
	}

	// Fresh ledger: header first, hint block in the body, footer last.
	rows := m.renderEvolve(40, 12)
	joined := strings.Join(rows, "\n")
	if !strings.Contains(joined, "evolution") {
		t.Fatalf("header missing:\n%s", joined)
	}
	if !strings.Contains(joined, "no scores yet") {
		t.Fatalf("empty-state hints missing:\n%s", joined)
	}
	if !strings.Contains(rows[len(rows)-1], "/exam") {
		t.Fatalf("footer hint missing: %q", rows[len(rows)-1])
	}

	// File a scorecard: the class row appears with seat, clock and mean.
	eval.OpenLeaderboard().Record("coding", eval.Scorecard{
		Agent: "claude-opus-5-5", Total: 0.9, Passed: true,
	})
	rows = m.renderEvolve(40, 20)
	joined = strings.Join(rows, "\n")
	for _, want := range []string{"coding", "opus-5-5", "0/10", "0.90"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("row missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "no scores yet") {
		t.Fatalf("fresh hints outstayed their welcome:\n%s", joined)
	}

	// Ten booked rounds → the exam clock strikes: yellow EXAM flag.
	for i := 0; i < eval.ExamInterval; i++ {
		eval.OpenLeaderboard().CountRound("coding")
	}
	rows = m.renderEvolve(40, 20)
	if joined = strings.Join(rows, "\n"); !strings.Contains(joined, "EXAM") {
		t.Fatalf("exam-due flag missing:\n%s", joined)
	}
}
