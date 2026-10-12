package eval

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
)

// TestParseGrade pins the extraction rules: an exact-number line wins,
// then the first decimal, then any number; scale tokens (8.5, 85) fold
// into 0..1; junk and out-of-range values are errors, never guesses.
func TestParseGrade(t *testing.T) {
	good := []struct{ in, want string }{
		{"0.85", "0.85"},                          // instructed shape
		{"  0.42  \n", "0.42"},                    // padded line
		{"0.9\nBecause it fully answers.", "0.9"}, // number line before prose
		{"The answer scores 0.75 overall.", "0.75"},
		{"8.5", "0.85"}, // /10 style
		{"85", "0.85"},  // percent style
		{"0", "0"},      // clamps are exact
		{"1", "1"},
		{"-0.3", "0"}, // negatives floor at 0
		{"100", "1"},
	}
	for _, tc := range good {
		got, err := ParseGrade(tc.in)
		if err != nil {
			t.Errorf("ParseGrade(%q) error: %v", tc.in, err)
			continue
		}
		if got != mustFloat(t, tc.want) {
			t.Errorf("ParseGrade(%q) = %v, want %s", tc.in, got, tc.want)
		}
	}

	bad := []string{
		"no numbers in this reply",
		"", "   \n  ",
		"250", // not a grade on any scale
	}
	for _, in := range bad {
		if v, err := ParseGrade(in); err == nil {
			t.Errorf("ParseGrade(%q) = %v, want error", in, v)
		}
	}
}

func mustFloat(t *testing.T, s string) float64 {
	t.Helper()
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatalf("bad test literal %q: %v", s, err)
	}
	return f
}

// TestJudgeRun: the asker receives system + task + answer, the parsed
// number comes back, and the empty-task / empty-answer / asker-error
// legs behave as documented.
func TestJudgeRun(t *testing.T) {
	var gotUser string
	ask := func(ctx context.Context, system, user string) (string, error) {
		if !strings.Contains(system, "single number") {
			t.Errorf("judge system prompt missing reply contract:\n%s", system)
		}
		gotUser = user
		return "0.75", nil
	}

	v, err := JudgeRun(context.Background(), ask, "add a -max-iter flag", "done: flag added")
	if err != nil {
		t.Fatal(err)
	}
	if v != 0.75 {
		t.Fatalf("judge = %v, want 0.75", v)
	}
	if !strings.Contains(gotUser, "Task:\nadd a -max-iter flag") ||
		!strings.Contains(gotUser, "Final answer:\ndone: flag added") {
		t.Fatalf("judge payload malformed:\n%s", gotUser)
	}

	// Tool-only run: the judge is told there is no prose.
	if _, err := JudgeRun(context.Background(), ask, "run the tests", ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotUser, "no textual answer") {
		t.Fatalf("empty answer not flagged for the judge:\n%s", gotUser)
	}

	// Empty task refuses before any call.
	called := false
	noAsk := func(ctx context.Context, system, user string) (string, error) {
		called = true
		return "1", nil
	}
	if _, err := JudgeRun(context.Background(), noAsk, "  ", "x"); err == nil || called {
		t.Fatalf("empty task must fail without calling the asker (err=%v called=%v)", err, called)
	}

	// Asker failure propagates untouched.
	boom := errors.New("boom")
	_, err = JudgeRun(context.Background(), func(ctx context.Context, system, user string) (string, error) {
		return "", boom
	}, "task", "answer")
	if !errors.Is(err, boom) {
		t.Fatalf("asker error = %v, want boom", err)
	}
}
