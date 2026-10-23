package eval

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lyon-agent/pkg/memory"
)

// projectFixture writes a small but complete workspace — go.mod, README,
// cmd/ entrypoints — plus one project memory fact, then isolates
// $LYON_HOME so the bank never touches the real exam store.
func projectFixture(t *testing.T) string {
	t.Helper()
	scratchHome(t)
	ws := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module github.com/example/lyon-agent\n\ngo 1.25\n")
	write("README.md", "# Lyon Agent\n\nan agent.\n")
	write("cmd/lyon/.keep", "")
	write("cmd/lyond/.keep", "")
	if _, err := memory.Open(ws).Save(memory.Fact{
		Name:        "deploy-target",
		Description: "production deploys to loopback lyond only",
		Scope:       memory.ScopeProject,
		Body:        "lyond binds loopback; no auth by design.",
	}); err != nil {
		t.Fatalf("seed fact: %v", err)
	}
	return ws
}

// TestProjectExamGeneratedFromWorkspace: the bank derives from go.mod,
// README, cmd/ and the project memory layer, carries a fingerprint, and
// grades what the standing context carries (the answerer context is in
// the system prompt — here we simulate it by answering from inputs).
func TestProjectExamGeneratedFromWorkspace(t *testing.T) {
	ws := projectFixture(t)
	ex, err := LoadProjectExam(ws)
	if err != nil {
		t.Fatalf("LoadProjectExam: %v", err)
	}
	if ex.Class != ProjectExamClass {
		t.Errorf("class = %q, want %q", ex.Class, ProjectExamClass)
	}
	if ex.Fingerprint == "" {
		t.Error("generated bank carries no fingerprint")
	}
	// module + go + title + cmd + one fact = 5 questions
	if len(ex.Questions) != 5 {
		t.Fatalf("questions = %d, want 5: %+v", len(ex.Questions), ex.Questions)
	}
	// A perfect answerer built from the same standing context passes.
	answers := map[string]string{
		"What is this project's Go module path?":                          "github.com/example/lyon-agent",
		"Which Go language version does this project target?":              "go 1.25",
		"What is this project's title (its README heading)?":               "Lyon Agent",
		"Name the command entrypoints (directories under cmd/) in this project.": "lyon and lyond",
		`According to this project's memory, what is "deploy-target"?`:     "production deploys to loopback lyond only",
	}
	res := RunExam(context.Background(), ex, func(_ context.Context, q string) (string, error) {
		return answers[q], nil
	})
	if !res.Passed || res.Pct != 1.0 {
		t.Fatalf("perfect context answers must pass: %s", res.Summary())
	}
}

// TestProjectExamRegeneratesOnFingerprintChange: the bank is dynamic —
// a changed project (new cmd entrypoint) changes the fingerprint and
// regenerates the stored questions; an unchanged project returns the
// stored bank (hand edits survive).
func TestProjectExamRegeneratesOnFingerprintChange(t *testing.T) {
	ws := projectFixture(t)
	ex1, err := LoadProjectExam(ws)
	if err != nil {
		t.Fatalf("LoadProjectExam: %v", err)
	}
	// Hand-edit the stored bank; unchanged project must keep the edit.
	ex1.Questions = ex1.Questions[:1]
	if err := SaveExam(ex1); err != nil {
		t.Fatalf("SaveExam: %v", err)
	}
	ex2, err := LoadProjectExam(ws)
	if err != nil {
		t.Fatalf("LoadProjectExam (unchanged): %v", err)
	}
	if len(ex2.Questions) != 1 {
		t.Fatalf("unchanged project regenerated over hand edit: %d questions", len(ex2.Questions))
	}
	// The project changes → fingerprint changes → regenerate.
	if err := os.MkdirAll(filepath.Join(ws, "cmd", "lyondctl"), 0o755); err != nil {
		t.Fatal(err)
	}
	ex3, err := LoadProjectExam(ws)
	if err != nil {
		t.Fatalf("LoadProjectExam (changed): %v", err)
	}
	if ex3.Fingerprint == ex2.Fingerprint {
		t.Error("fingerprint did not change after the project changed")
	}
	if len(ex3.Questions) != 5 {
		t.Fatalf("regenerated questions = %d, want 5", len(ex3.Questions))
	}
}

// TestProjectExamDeterministic: two loads over the same project produce
// the same fingerprint and question order — a flapping eval set would
// grade noise, not knowledge.
func TestProjectExamDeterministic(t *testing.T) {
	ws := projectFixture(t)
	ex1, err := LoadProjectExam(ws)
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(ExamPath(ProjectExamClass)) // force a fresh generation
	ex2, err := LoadProjectExam(ws)
	if err != nil {
		t.Fatal(err)
	}
	if ex1.Fingerprint != ex2.Fingerprint {
		t.Errorf("fingerprint not deterministic: %s vs %s", ex1.Fingerprint, ex2.Fingerprint)
	}
	if len(ex1.Questions) != len(ex2.Questions) {
		t.Fatalf("question count not deterministic")
	}
	for i := range ex1.Questions {
		if ex1.Questions[i].Q != ex2.Questions[i].Q {
			t.Errorf("question %d order differs", i)
		}
	}
}

// TestProjectExamEmptyWorkspace: a workspace with no material at all is
// an error — an empty exam grades nothing and must not file a score.
func TestProjectExamEmptyWorkspace(t *testing.T) {
	scratchHome(t)
	if _, err := LoadProjectExam(t.TempDir()); err == nil {
		t.Fatal("empty workspace must error")
	}
	if _, err := LoadProjectExam(""); err == nil {
		t.Fatal("empty workspace path must error")
	}
}

// TestProjectSystemCarriesContext: the project class's answerer system
// prompt includes the standing context (module, title, fact text);
// other classes keep the plain closed-book prompt.
func TestProjectSystemCarriesContext(t *testing.T) {
	ws := projectFixture(t)
	ex, err := LoadProjectExam(ws)
	if err != nil {
		t.Fatal(err)
	}
	sys := ProjectSystem(ex, ws)
	if !strings.Contains(sys, "github.com/example/lyon-agent") ||
		!strings.Contains(sys, "Lyon Agent") ||
		!strings.Contains(sys, "loopback lyond only") {
		t.Fatalf("project system prompt missing standing context:\n%s", sys)
	}
	if !strings.HasPrefix(sys, ClosedBookSystem) {
		t.Error("closed-book rule must stay first")
	}
	// A non-project exam keeps the plain prompt.
	got := ProjectSystem(Exam{Class: "coding"}, ws)
	if got != ClosedBookSystem {
		t.Errorf("non-project class got context appended: %q", got)
	}
}
