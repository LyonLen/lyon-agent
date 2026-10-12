package eval

import (
	"context"
	"fmt"
	"os"
	"testing"
)

func TestCheckAnswerRules(t *testing.T) {
	cases := []struct {
		name string
		q    Question
		got  string
		want bool
	}{
		{"expected substring, case-insensitive", Question{Expected: "go test ./..."}, "First run go TEST ./... then report.", true},
		{"expected missing", Question{Expected: "port 8080"}, "port 80", false},
		{"keywords majority", Question{Keywords: []string{"cache", "prefix", "bytes"}}, "the prefix cache misses when bytes differ", true},
		{"keywords minority", Question{Keywords: []string{"cache", "prefix", "bytes", "order", "hash"}}, "it is about caching", false},
		{"empty reply", Question{Expected: "x"}, "   ", false},
		{"no expectations at all", Question{}, "anything", false},
	}
	for _, c := range cases {
		if got := checkAnswer(c.q, c.got); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestRunExamGradesAndPasses(t *testing.T) {
	scratchHome(t)
	ex := Exam{Class: "coding", Questions: []Question{
		{Q: "how to run tests?", Expected: "go test ./..."},
		{Q: "port?", Expected: "8080"},
		{Q: "trigger word?", Keywords: []string{"buffet"}},
	}}
	answers := map[string]string{
		"how to run tests?": "You run GO TEST ./...  ", // normalized hit
		"port?":             "somewhere on 9090",       // miss
		"trigger word?":     "memory buffet",           // keyword hit
	}
	res := RunExam(context.Background(), ex, func(_ context.Context, q string) (string, error) {
		return answers[q], nil
	})
	if len(res.Answers) != 3 || res.Answers[0].OK != true || res.Answers[1].OK != false || res.Answers[2].OK != true {
		t.Fatalf("answers = %+v", res.Answers)
	}
	if res.Pct != 2.0/3.0 || res.Passed { // default line is 0.75
		t.Fatalf("pct=%v passed=%v", res.Pct, res.Passed)
	}
	// Lowering the line flips it.
	ex.Pass = 0.6
	res = RunExam(context.Background(), ex, func(_ context.Context, q string) (string, error) {
		return answers[q], nil
	})
	if !res.Passed {
		t.Fatal("0.6 line must pass a 2/3 exam")
	}
}

func TestRunExamAnswererErrorGradesAsMiss(t *testing.T) {
	scratchHome(t)
	ex := Exam{Class: "c", Questions: []Question{{Q: "q", Expected: "a"}}}
	res := RunExam(context.Background(), ex, func(context.Context, string) (string, error) {
		return "", fmt.Errorf("gateway down")
	})
	if res.Answers[0].OK || res.Passed {
		t.Fatalf("call failure must grade as a miss: %+v", res)
	}
}

func TestExamEmptyQuestionListNeverPasses(t *testing.T) {
	scratchHome(t)
	res := RunExam(context.Background(), Exam{Class: "c"},
		func(context.Context, string) (string, error) { return "x", nil })
	if res.Passed || res.Pct != 0 {
		t.Fatalf("empty exam: %+v", res)
	}
}

func TestExamSaveLoadRoundtripAndPathSafety(t *testing.T) {
	scratchHome(t)
	ex := Exam{Class: "coding", Pass: 0.9, Questions: []Question{{Q: "q1", Expected: "e1"}}}
	if err := SaveExam(ex); err != nil {
		t.Fatal(err)
	}
	got, err := LoadExam("coding")
	if err != nil {
		t.Fatal(err)
	}
	if got.Pass != 0.9 || len(got.Questions) != 1 || got.Questions[0].Expected != "e1" {
		t.Fatalf("roundtrip = %+v", got)
	}
	// Hostile class names cannot escape the exams directory.
	p := ExamPath("../../evil")
	if !fileWithin(t, p) {
		t.Fatalf("path escaped: %s", p)
	}
	// Missing class → os.ErrNotExist for the caller to branch on.
	if _, err := LoadExam("nope"); !os.IsNotExist(err) {
		t.Fatalf("err = %v, want not-exist", err)
	}
}

func fileWithin(t *testing.T, path string) bool {
	t.Helper()
	return !containsDotDot(path)
}

func containsDotDot(s string) bool {
	for i := 0; i+1 < len(s); i++ {
		if s[i] == '.' && s[i+1] == '.' {
			// only count ".." as a path element
			before := i == 0 || s[i-1] == '/' || s[i-1] == '\\'
			after := i+2 == len(s) || s[i+2] == '/' || s[i+2] == '\\'
			if before && after {
				return true
			}
		}
	}
	return false
}
