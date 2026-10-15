package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"lyon-agent/pkg/eval"
	"lyon-agent/pkg/routing"
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

	// Fresh ledger: header first, hint block in the body. The footer is
	// composed by composeSplit at the column bottom — it spans the whole
	// right column now that the resident plan block sits above it (#1).
	rows := m.renderEvolve(40, 12)
	joined := strings.Join(rows, "\n")
	if !strings.Contains(joined, "evolution") {
		t.Fatalf("header missing:\n%s", joined)
	}
	if !strings.Contains(joined, "no scores yet") {
		t.Fatalf("empty-state hints missing:\n%s", joined)
	}
	if !strings.Contains(evoFooter(40), "/exam") {
		t.Fatalf("footer hint missing: %q", evoFooter(40))
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

// TestRenderEvolveInfoBlocks: the two #14 blocks render from real data —
// the summary from a populated ledger (totals, strongest/weakest latest
// scores, newest result, health verdict, routing evidence) and the
// exam-set block from a seeded bank plus the candidate field — with the
// idle forms on a fresh home, the feed still coexisting, and the exact-h
// / exact-w contract holding at every panel size.
func TestRenderEvolveInfoBlocks(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))
	m := newTestModel(t)

	// Fresh home: both blocks show their idle/seed forms.
	rows := m.renderEvolve(72, 44)
	joined := strings.Join(rows, "\n")
	for _, want := range []string{
		"overall evolution",
		"health: idle — no scorecards yet",
		"exam set · adjust it",
		"banks seed to $LYON_HOME/exams on first /exam",
		"LYON_EXAM=off",
		"/agents add <id> <model>",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("fresh panel missing %q:\n%s", want, joined)
		}
	}

	// Seed one bank on disk so the on-disk composition branch renders.
	if err := eval.SaveExam(eval.Exam{Class: "coding", Questions: []eval.Question{
		{Q: "q1", Expected: "a1"}, {Q: "q2", Expected: "a2"},
		{Q: "q3", Expected: "a3"}, {Q: "q4", Expected: "a4"},
	}}); err != nil {
		t.Fatalf("seed exam: %v", err)
	}

	// Populate the ledger: two seated classes, one demoted-style open
	// seat, a due clock on general and a failure streak on coding.
	lb := eval.OpenLeaderboard()
	lb.Record("coding", eval.Scorecard{Agent: "claude-opus-5-5", Total: 0.9, Passed: true})
	lb.Record("general", eval.Scorecard{Agent: "claude-sonnet-5", Total: 0.7, Passed: true})
	lb.Record("review", eval.Scorecard{Agent: "claude-haiku-5", Total: 0.2, Passed: false})
	for i := 0; i < eval.ExamInterval; i++ {
		lb.CountRound("general")
	}
	lb.SubmitExam("coding", eval.Scorecard{
		Agent: "claude-opus-5-5", Total: 0.5, Passed: false, Source: "exam",
	})
	// Routing evidence: 3 samples above the steering bar → 1 routed.
	rt := routing.Open()
	for i := 0; i < 3; i++ {
		rt.Record("coding", "claude-opus-5-5", "claude-opus-5-5", 0.9)
	}

	rows = m.renderEvolve(72, 48)
	joined = strings.Join(rows, "\n")
	for _, want := range []string{
		// Summary: totals, range with models, newest result, health.
		"8 classes · 3 tested · 4 cards",
		"seats 2 held · 1 open · 1 routed",
		"strongest general 0.70 [sonnet-5]",
		"weakest review 0.20 [haiku-5]",
		"newest: coding exam 0.50 fail",
		"failure streaks 1 · exams due 1 · open seats 1",
		// Exam set: composition, files, field, commands, toggles.
		"1/8 banks on disk · 4 held-out questions",
		"coding 4",
		"hand-written facts",
		"add/remove/edit: $LYON_HOME/exams/<class>.json",
		"candidates: 1 in $LYON_HOME",
		"/agents add <id> <model>",
		"/exam [class] now",
		"/contest [class]",
		"LYON_SCORE=off",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("populated panel missing %q:\n%s", want, joined)
		}
	}

	// The feed still shares the panel: a card row and both block titles
	// coexist — the blocks' reserve keeps them from being crowded out.
	m.evoFeed = append(m.evoFeed, "kickScore: coding 0.90 filed")
	joined = strings.Join(m.renderEvolve(72, 48), "\n")
	for _, want := range []string{"kickScore: coding 0.90 filed", "overall evolution", "exam set"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("feed+blocks render missing %q:\n%s", want, joined)
		}
	}

	// Contract at awkward sizes: exactly h rows, every non-filler row
	// exactly w cells, never a panic while the blocks truncate.
	for _, tc := range []struct{ w, h int }{
		{28, 9}, {28, 52}, {34, 22}, {40, 3}, {52, 15}, {72, 60},
	} {
		got := m.renderEvolve(tc.w, tc.h)
		if len(got) != tc.h {
			t.Fatalf("renderEvolve(%d,%d): %d rows, want %d", tc.w, tc.h, len(got), tc.h)
		}
		for i, r := range got {
			if r == "" {
				continue // filler tail, per the renderSteps contract
			}
			if n := ansi.StringWidth(r); n != tc.w {
				t.Fatalf("renderEvolve(%d,%d): row %d is %d cells, want %d: %q",
					tc.w, tc.h, i, n, tc.w, r)
			}
		}
	}
}

// TestRenderEvolveInfoBlocksZH: the same two blocks carry their Chinese
// variants — tr() re-evaluates per render, so flipping uiLang flips the
// panel — and the width contract holds for the wider CJK cells too.
func TestRenderEvolveInfoBlocksZH(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))
	orig := uiLang
	t.Cleanup(func() { uiLang = orig })
	uiLang = "zh"
	m := newTestModel(t)

	lb := eval.OpenLeaderboard()
	lb.Record("coding", eval.Scorecard{Agent: "claude-opus-5-5", Total: 0.9, Passed: true})
	lb.Record("general", eval.Scorecard{Agent: "claude-sonnet-5", Total: 0.7, Passed: true})

	rows := m.renderEvolve(72, 44)
	joined := strings.Join(rows, "\n")
	for _, want := range []string{
		"整体进化情况",
		"8 类 · 已测 2 · 卡 2",
		"最强 coding 0.90 [opus-5-5]",
		"最弱 general 0.70 [sonnet-5]",
		"健康度：健康",
		"测评集 · 如何调整",
		"增删改题：$LYON_HOME/exams/<class>.json",
		"候选：1 名在 $LYON_HOME/agents/",
		"演练：/exam",
		"开关：LYON_HOME",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("zh panel missing %q:\n%s", want, joined)
		}
	}
	for i, r := range rows {
		if r == "" {
			continue
		}
		if n := ansi.StringWidth(r); n != 72 {
			t.Fatalf("zh row %d is %d cells, want 72: %q", i, n, r)
		}
	}
}
