package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultExamPass is the pass ratio when an exam file does not set one.
const DefaultExamPass = 0.75

// ClosedBookSystem is the exam prompt: no tools, no files — the point of
// an exam is what the agent carries in weights, not what it can look up
// (design §1.4 anti-cheat: exam questions stay out of retrieval).
const ClosedBookSystem = `Answer the question from your own knowledge only. Do not use tools, do not ask for files, do not hedge. One short paragraph of concrete facts.`

// Question is one held-out exam item. Expected is graded by normalized
// substring; Keywords grade by majority hit when Expected is absent (or
// as a bonus — a hit on either counts).
type Question struct {
	Q        string   `json:"q"`
	Expected string   `json:"expected,omitempty"`
	Keywords []string `json:"keywords,omitempty"`
}

// Exam is a task class's test set at $LYON_HOME/exams/<class>.json —
// written by hand (or seeded), never generated from the production tasks
// it is used to grade.
type Exam struct {
	Class     string     `json:"class"`
	Pass      float64    `json:"pass,omitempty"` // pass ratio; ≤0 → DefaultExamPass
	Questions []Question `json:"questions"`
}

// ExamPath is where a class's exam set lives.
func ExamPath(class string) string {
	safe := strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' {
			return '_'
		}
		return r
	}, class)
	return filepath.Join(homeDir(), "exams", safe+".json")
}

// LoadExam reads a class's exam set (os.ErrNotExist when the class has none).
func LoadExam(class string) (Exam, error) {
	data, err := os.ReadFile(ExamPath(class))
	if err != nil {
		return Exam{}, err
	}
	var ex Exam
	if err := json.Unmarshal(data, &ex); err != nil {
		return Exam{}, fmt.Errorf("parse exam %s: %w", class, err)
	}
	ex.Class = class
	return ex, nil
}

// SaveExam writes an exam set (used by seeding and tests).
func SaveExam(ex Exam) error {
	if ex.Class == "" {
		return fmt.Errorf("exam has no class")
	}
	path := ExamPath(ex.Class)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(ex, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	_ = tmp.Chmod(0o644) // CreateTemp makes 0600; keep the file's original mode
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// Answerer answers one question closed-book (wired to a model call with
// ClosedBookSystem and no tools).
type Answerer func(ctx context.Context, question string) (string, error)

// ExamAnswer is one graded item.
type ExamAnswer struct {
	Q        string `json:"q"`
	Expected string `json:"expected,omitempty"`
	Got      string `json:"got"`
	OK       bool   `json:"ok"`
}

// ExamResult is the exam outcome: Pct is the pass ratio actually scored,
// Passed compares it to the exam's line.
type ExamResult struct {
	Class   string       `json:"class"`
	Pct     float64      `json:"pct"`
	Passed  bool         `json:"passed"`
	Answers []ExamAnswer `json:"answers"`
}

// Summary renders a one-line human result for cmdOut / stdout.
func (r ExamResult) Summary() string {
	verdict := "FAIL"
	if r.Passed {
		verdict = "PASS"
	}
	return fmt.Sprintf("exam %s — %s %d/%d (%.0f%%)", r.Class, verdict,
		countOK(r.Answers), len(r.Answers), r.Pct*100)
}

func countOK(as []ExamAnswer) int {
	n := 0
	for _, a := range as {
		if a.OK {
			n++
		}
	}
	return n
}

// RunExam walks the held-out questions through the closed-book answerer
// and grades deterministically. A failed answer call grades as a miss —
// an agent that cannot answer has not demonstrated the knowledge.
func RunExam(ctx context.Context, ex Exam, ans Answerer) ExamResult {
	res := ExamResult{Class: ex.Class}
	if len(ex.Questions) == 0 {
		return res
	}
	pass := ex.Pass
	if pass <= 0 {
		pass = DefaultExamPass
	}
	for _, q := range ex.Questions {
		got := ""
		if a, err := ans(ctx, q.Q); err == nil {
			got = a
		}
		res.Answers = append(res.Answers, ExamAnswer{
			Q: q.Q, Expected: q.Expected, Got: got, OK: checkAnswer(q, got),
		})
	}
	res.Pct = float64(countOK(res.Answers)) / float64(len(res.Answers))
	res.Passed = res.Pct >= pass
	return res
}

// checkAnswer grades one closed-book answer: normalized Expected
// contained in the reply passes; otherwise a majority of Keywords must
// appear. Empty replies always fail.
func checkAnswer(q Question, got string) bool {
	g := normalize(got)
	if g == "" {
		return false
	}
	if q.Expected != "" && strings.Contains(g, normalize(q.Expected)) {
		return true
	}
	if len(q.Keywords) > 0 {
		hits := 0
		for _, k := range q.Keywords {
			if k != "" && strings.Contains(g, normalize(k)) {
				hits++
			}
		}
		return hits*2 >= len(q.Keywords)
	}
	return false
}

// normalize lowercases and collapses whitespace so grading ignores
// formatting noise (case, wrapping, double spaces) but not wording.
func normalize(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// clipRunes bounds a payload passed into a model call.
func clipRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
