package eval

import (
	"fmt"
	"os"
)

// Seed exam banks — hand-written held-out questions per task class
// (§1.4). The rule stands: an exam is never generated from the
// production tasks it grades, so these ship frozen in the binary and
// land on disk at $LYON_HOME/exams/<class>.json on first use, where
// they can be inspected and edited. Four questions each; the pass line
// (DefaultExamPass = 0.75) needs three of four.
var seedExams = map[string]Exam{
	"coding": {Questions: []Question{
		{Q: "Which command runs every Go package's tests in a repository?",
			Expected: "go test ./...", Keywords: []string{"go test"}},
		{Q: "What does the Go keyword defer schedule a call to run?",
			Expected: "just before the surrounding function returns",
			Keywords: []string{"before", "return", "last"}},
		{Q: "What is the atomic unit of change Git records?",
			Expected: "commit", Keywords: []string{"commit"}},
		{Q: "Name the command that formats Go source files in place.",
			Expected: "gofmt", Keywords: []string{"gofmt", "go fmt", "goimports"}},
	}},
	"fix": {Questions: []Question{
		{Q: "What technique halves the offending commit range until the culprit is found?",
			Expected: "git bisect", Keywords: []string{"bisect"}},
		{Q: "Before fixing a regression, what do you write to prove the bug exists?",
			Expected: "a failing test", Keywords: []string{"test", "reproduce"}},
		{Q: "In Go, what does recovering from a panic prevent?",
			Expected: "the program from crashing", Keywords: []string{"crash", "terminate", "exit"}},
		{Q: "What is the smallest acceptable change when fixing a root cause?",
			Expected: "the minimal fix", Keywords: []string{"minimal", "smallest", "root cause"}},
	}},
	"review": {Questions: []Question{
		{Q: "A change adds new branches but no tests for them. What is the usual review verdict?",
			Expected: "request changes", Keywords: []string{"test", "request", "changes"}},
		{Q: "Name the review question that asks whether error paths are handled.",
			Expected: "error handling", Keywords: []string{"error"}},
		{Q: "What should a reviewer do with a nitpick (style-only remark)?",
			Expected: "mark it non-blocking", Keywords: []string{"non-blocking", "nit", "optional", "minor"}},
		{Q: "Which is checked first in a review: design or naming?",
			Expected: "design", Keywords: []string{"design"}},
	}},
	"research": {Questions: []Question{
		{Q: "What document starts with a one-line research question?",
			Expected: "a research brief", Keywords: []string{"brief", "question", "objective"}},
		{Q: "Name a method for comparing two options by weighted criteria.",
			Expected: "trade-off matrix", Keywords: []string{"matrix", "weighted", "criteria", "pros and cons"}},
		{Q: "In a comparison, what makes a source credible?",
			Expected: "primary evidence", Keywords: []string{"primary", "official", "vendor", "source"}},
		{Q: "What must a research conclusion state about its evidence?",
			Expected: "confidence and gaps", Keywords: []string{"confidence", "uncertainty", "gap", "caveat"}},
	}},
	"writing": {Questions: []Question{
		{Q: "What does a changelog entry group its items under?",
			Expected: "Added, Changed and Fixed", Keywords: []string{"added", "changed", "fixed"}},
		{Q: "What is the recommended sentence voice for technical docs?",
			Expected: "active voice", Keywords: []string{"active"}},
		{Q: "What must a heading hierarchy do?",
			Expected: "nest in order without skipping levels", Keywords: []string{"level", "nest", "order", "skip"}},
		{Q: "In a summary, what comes first?",
			Expected: "the conclusion", Keywords: []string{"conclusion", "result", "answer", "first"}},
	}},
	"design": {Questions: []Question{
		{Q: "Name the technique of making a component's contract explicit before its implementation.",
			Expected: "interface design", Keywords: []string{"interface", "contract", "api"}},
		{Q: "What are the three legs of the CAP theorem?",
			Expected: "consistency, availability, partition tolerance",
			Keywords: []string{"consistency", "availability", "partition"}},
		{Q: "What document records an architectural decision and its rationale?",
			Expected: "an architecture decision record", Keywords: []string{"decision", "adr", "record"}},
		{Q: "What should a schema migration plan include for rollback?",
			Expected: "a reverse migration", Keywords: []string{"reverse", "rollback", "back"}},
	}},
	"data": {Questions: []Question{
		{Q: "Which SQL clause filters rows?",
			Expected: "WHERE", Keywords: []string{"where"}},
		{Q: "In CSV, what character separates fields by default?",
			Expected: "comma", Keywords: []string{"comma", ","}},
		{Q: "What statistic describes the middle value of a sorted sample?",
			Expected: "median", Keywords: []string{"median"}},
		{Q: "What does JSON-lines (JSONL) store one of per line?",
			Expected: "a JSON object", Keywords: []string{"json", "object", "record"}},
	}},
	"general": {Questions: []Question{
		{Q: "What does the shell operator && do between two commands?",
			Expected: "runs the second only if the first succeeds",
			Keywords: []string{"first", "second", "only", "succeed"}},
		{Q: "Where do environment variables live: process or disk?",
			Expected: "the process", Keywords: []string{"process", "runtime"}},
		{Q: "What is the unit of permission bits on Unix files?",
			Expected: "octal modes like 644", Keywords: []string{"octal", "644", "chmod", "mode"}},
		{Q: "What exit code do most shells report for success?",
			Expected: "0", Keywords: []string{"zero", "0"}},
	}},
}

// LoadOrSeedExam returns a class's exam: the editable file on disk when
// present, otherwise the shipped bank, seeded to disk so it can be
// inspected. Unknown classes return an error — never a silent zero
// exam that would grade everyone 100%.
func LoadOrSeedExam(class string) (Exam, error) {
	if ex, err := LoadExam(class); err == nil {
		return ex, nil
	} else if !os.IsNotExist(err) {
		return Exam{}, err
	}
	bank, ok := seedExams[class]
	if !ok {
		return Exam{}, fmt.Errorf("no exam bank for class %q", class)
	}
	ex := Exam{Class: class, Questions: bank.Questions}
	// Best-effort: seed to disk so the bank is inspectable and editable;
	// a read-only home still gets to take the exam.
	_ = SaveExam(ex)
	return ex, nil
}
