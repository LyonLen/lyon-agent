package eval

import (
	"os"
	"strings"
	"testing"
)

// TestLoadOrSeedExam: every routing class ships a bank, first call seeds
// the file, the second call reads it back, and unknown classes error
// instead of grading an empty exam 100%.
func TestLoadOrSeedExam(t *testing.T) {
	scratchHome(t)

	classes := []string{
		"coding", "fix", "review", "research", "writing", "design", "data", "general",
	}
	for _, class := range classes {
		ex, err := LoadOrSeedExam(class)
		if err != nil {
			t.Fatalf("%s: seed: %v", class, err)
		}
		if len(ex.Questions) < 4 {
			t.Errorf("%s: bank has %d questions, want ≥4", class, len(ex.Questions))
		}
		if _, err := os.Stat(ExamPath(class)); err != nil {
			t.Errorf("%s: seed did not write %s: %v", class, ExamPath(class), err)
		}
		// Second call comes from disk — the editable file wins.
		ex2, err := LoadOrSeedExam(class)
		if err != nil {
			t.Fatalf("%s: reload: %v", class, err)
		}
		if len(ex2.Questions) != len(ex.Questions) {
			t.Errorf("%s: reload mismatch %d vs %d", class, len(ex2.Questions), len(ex.Questions))
		}
	}

	if _, err := LoadOrSeedExam("no-such-class"); err == nil {
		t.Fatal("unknown class must error, not return an empty exam")
	}
}

// TestSeedQuestionsGradeable: each shipped question has a gradable
// shape — a non-empty expected substring or keywords — so a correct
// closed-book answer cannot fail on an empty rubric.
func TestSeedQuestionsGradeable(t *testing.T) {
	for class, ex := range seedExams {
		for i, q := range ex.Questions {
			if q.Q == "" {
				t.Errorf("%s q%d: empty question", class, i)
			}
			if q.Expected == "" && len(q.Keywords) == 0 {
				t.Errorf("%s q%d %q: no expected, no keywords — ungradable",
					class, i, q.Q)
			}
			if strings.TrimSpace(q.Q) != q.Q {
				t.Errorf("%s q%d: question not trimmed", class, i)
			}
		}
	}
}
