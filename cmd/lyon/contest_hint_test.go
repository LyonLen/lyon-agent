package main

// Task C of docs/planning/archive/EVOLUTION-GAPS.md: /contest with an empty candidate field
// (the single-model default — only the built-in self entrant) must say
// so with an actionable create-a-candidate hint instead of silently
// running a one-horse race. The field itself is file-backed
// ($LYON_HOME/agents/*.json via eval.Contestants), never hardcoded.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lyon-agent/pkg/eval"
)

// TestContestHintWhenFieldIsOnlySelf: /contest against an empty agents
// dir answers with the hint in the ACTIVE language, names the real
// /agents add syntax (positional <id> <model> [effort] — no model=), and
// opens no contest step.
func TestContestHintWhenFieldIsOnlySelf(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t)) // no agents/*.json → field is self alone
	m := newTestModel(t)

	old := uiLang
	t.Cleanup(func() { uiLang = old })

	uiLang = "en"
	m.cmdOut = nil
	m.contestCommand("fix")
	got := stripANSI(m.cmdOutText())
	if !strings.Contains(got, "no candidates beyond self") ||
		!strings.Contains(got, "/agents add <id> <model> [effort]") {
		t.Fatalf("English hint missing or off-syntax:\n%s", got)
	}

	uiLang = "zh"
	m.cmdOut = nil
	m.contestCommand("fix")
	got = stripANSI(m.cmdOutText())
	if !strings.Contains(got, "先创建一个") ||
		!strings.Contains(got, "/agents add <id> <model> [effort]") {
		t.Fatalf("Chinese hint missing or off-syntax:\n%s", got)
	}

	// The refusal happens before any contest machinery spins up — no
	// stepEval row, no goroutine, no gateway exam.
	for _, s := range m.steps {
		if strings.HasPrefix(s.name, "contest.") {
			t.Fatalf("hint must not open step %q", s.name)
		}
	}
}

// TestRunContestRefusesFieldOfOne: runContest itself refuses a
// self-only field on the error channel (the contestDone error line
// renders it) and returns before the held-out bank is seeded — no disk
// write for a contest that never runs.
func TestRunContestRefusesFieldOfOne(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))
	m := newTestModel(t)

	old := uiLang
	t.Cleanup(func() { uiLang = old })
	uiLang = "en"

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rows, winner, err := runContest(ctx, m.sess, "main-x", "research")
	if err == nil {
		t.Fatal("runContest with only self in the field must refuse")
	}
	if !strings.Contains(err.Error(), "no candidates beyond self") ||
		!strings.Contains(err.Error(), "/agents add <id> <model> [effort]") {
		t.Fatalf("refusal must carry the actionable hint: %q", err.Error())
	}
	if rows != nil || winner != "" {
		t.Fatalf("refusal must return nothing: rows=%v winner=%q", rows, winner)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("LYON_HOME"), "exams")); !os.IsNotExist(err) {
		t.Fatalf("refused contest must not seed the exam bank, stat err=%v", err)
	}
}

// TestContestFieldReadsAgentsFiles: the entrant list is file-backed, not
// hardcoded — a profile saved under $LYON_HOME/agents joins Contestants
// beside self, which is what lets /contest become a real competition.
func TestContestFieldReadsAgentsFiles(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))
	if err := eval.SaveCandidate(eval.Candidate{ID: "chally", Model: "model-c"}); err != nil {
		t.Fatal(err)
	}
	cands := eval.Contestants("main-x")
	if len(cands) != 2 {
		t.Fatalf("field = %d entrants, want chally + self", len(cands))
	}
	if cands[0].ID != "chally" || cands[0].Model != "model-c" {
		t.Fatalf("filed candidate not loaded: %+v", cands[0])
	}
	if cands[1].ID != "self" || cands[1].Model != "main-x" {
		t.Fatalf("self incumbent missing/wrong: %+v", cands[1])
	}
}
