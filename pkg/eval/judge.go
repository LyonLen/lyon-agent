package eval

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// JudgeCap bounds what the judge sees of the task and of the answer —
// a wall of output must not push the question out of the window.
const JudgeCap = 4000

// JudgeSystem is the fixed blind-grading prompt (design §1.2: 有标准
// 答案时对着答案判，无标准答案时裁判模型盲评). Fixed wording keeps
// grades comparable across runs; the reply is a bare number so
// ParseGrade can extract it without guessing at prose.
const JudgeSystem = `You grade how well an agent's final answer fulfils the user's task. Weigh correctness above everything: a confident but wrong or off-task answer scores near 0, a partially correct answer middling, a complete, correct, on-task answer near 1. Style, length and politeness do not matter. Reply with a single number between 0.00 and 1.00 and nothing else.`

// JudgeRun blind-grades a free-form production run: the caller passes
// the task and the run's final answer (empty = the run produced no
// prose, only tool activity — the judge is told so). A judge or parse
// failure is an error so the caller files no card instead of grading
// by guesswork.
func JudgeRun(ctx context.Context, ask Asker, task, answer string) (float64, error) {
	if strings.TrimSpace(task) == "" {
		return 0, errors.New("judge: empty task")
	}
	var b strings.Builder
	b.WriteString("Task:\n")
	b.WriteString(clipRunes(task, JudgeCap))
	b.WriteString("\n\nFinal answer:\n")
	if strings.TrimSpace(answer) == "" {
		b.WriteString("(no textual answer — the run finished as tool activity only)")
	} else {
		b.WriteString(clipRunes(answer, JudgeCap))
	}
	reply, err := ask(ctx, JudgeSystem, b.String())
	if err != nil {
		return 0, err
	}
	return ParseGrade(reply)
}

var (
	gradeFull = regexp.MustCompile(`^-?\d+(?:\.\d+)?$`) // a line that is just the number
	gradeDec  = regexp.MustCompile(`-?\d+\.\d+`)        // first decimal number
	gradeAny  = regexp.MustCompile(`-?\d+(?:\.\d+)?`)   // first number at all
)

// ParseGrade extracts the judge's 0..1 number from its reply. A line
// that is exactly a number wins (the instructed reply shape, and it
// can't be a numbered bullet); otherwise the first decimal number,
// then any number — "0.85", "The answer scores 0.85.", "8.5" and "85"
// all grade 0.85. A reply with no number, or a number above 100 (not
// a grade on any sane scale), is an error.
func ParseGrade(s string) (float64, error) {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if gradeFull.MatchString(line) {
			return normGrade(gradeAny.FindString(line))
		}
	}
	if m := gradeDec.FindString(s); m != "" {
		return normGrade(m)
	}
	if m := gradeAny.FindString(s); m != "" {
		return normGrade(m)
	}
	return 0, errors.New("grade: no number in judge reply")
}

// normGrade maps a numeric token onto 0..1: bare 0..1 passes through,
// 8.5 reads as 8.5/10, 85 as 85/100 (percent-style replies), negatives
// clamp to 0, and anything above 100 is refused as non-grade noise.
func normGrade(tok string) (float64, error) {
	v, err := strconv.ParseFloat(tok, 64)
	if err != nil {
		return 0, err
	}
	switch {
	case v < 0:
		return 0, nil
	case v <= 1:
		return v, nil
	case v <= 10:
		return v / 10, nil
	case v <= 100:
		return v / 100, nil
	}
	return 0, errors.New("grade: value out of range")
}
