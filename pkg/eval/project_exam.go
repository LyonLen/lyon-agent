package eval

// Project exam (2026-10-23 重定义): the eval set is generated FROM the
// project and stays dynamic — go.mod, README, cmd/ layout and the
// project memory layer are hashed into a fingerprint; when the project
// changes, the stored bank regenerates. Class "project" has no seed
// bank on purpose (LoadOrSeedExam refuses it): its questions are this
// workspace's facts, not world knowledge.
//
// The answerer still gets no tools (closed-book stands), but it DOES
// carry the project's standing context — the same digest a production
// session would hold in memory. The exam therefore grades what the
// agent actually carries about this project: context pipeline freshness
// plus extraction, never lookup.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"lyon-agent/pkg/memory"
)

// ProjectExamClass is the dynamic bank's class key (exams/project.json).
const ProjectExamClass = "project"

// projectFactCap bounds how many memory facts become questions — the
// exam stays a short closed-book sit-down, not a memory dump recital.
const projectFactCap = 6

// projectContextCap bounds the standing context injected into the
// answerer's system prompt.
const projectContextCap = 6000

// projectInputs are the deterministic sources a project exam derives
// from — everything here feeds both the questions and the fingerprint.
type projectInputs struct {
	Module  string // go.mod module path
	GoVer   string // go.mod go directive
	Title   string // first README heading
	Cmds    []string
	Facts   []memory.Fact // project-scope, name-sorted
	FP      string
	Context string // standing context for the answerer
}

// LoadProjectExam returns the project's dynamic bank, regenerating it
// when the fingerprint of its sources changed (the "dynamic" half of
// 评测集基于项目生成). A stored bank whose fingerprint still matches is
// returned as-is, so hand edits survive until the project itself moves.
func LoadProjectExam(workspace string) (Exam, error) {
	in, err := gatherProjectInputs(workspace)
	if err != nil {
		return Exam{}, err
	}
	if cur, err := LoadExam(ProjectExamClass); err == nil &&
		cur.Fingerprint == in.FP && len(cur.Questions) > 0 {
		return cur, nil
	}
	ex := generateProjectExam(in)
	ex.Fingerprint = in.FP
	// Best-effort persist: a read-only LYON_HOME would otherwise make
	// every /exam project regenerate from scratch, but must not block
	// the exam that already has its questions in hand.
	_ = SaveExam(ex)
	return ex, nil
}

// ProjectSystem is the answerer's system prompt for the project class:
// the closed-book rule plus the standing context the questions derive
// from. Other classes keep the plain closed-book prompt.
func ProjectSystem(ex Exam, workspace string) string {
	if ex.Class != ProjectExamClass {
		return ClosedBookSystem
	}
	in, err := gatherProjectInputs(workspace)
	if err != nil {
		return ClosedBookSystem
	}
	return ClosedBookSystem + "\n\nYou carry this project's standing context:\n" + in.Context
}

// gatherProjectInputs reads every source once and hashes them into the
// fingerprint. Missing pieces (no go.mod, no README, no cmd/) are fine
// — only a project with NO material at all is an error, because an
// empty exam would grade nothing.
func gatherProjectInputs(workspace string) (projectInputs, error) {
	var in projectInputs
	var fpParts []string

	if workspace == "" {
		return in, errors.New("project exam: no workspace")
	}
	if raw, err := os.ReadFile(filepath.Join(workspace, "go.mod")); err == nil {
		text := string(raw)
		in.Module = modDirective(text, "module")
		in.GoVer = modDirective(text, "go")
		fpParts = append(fpParts, "module:"+in.Module, "go:"+in.GoVer)
	}
	in.Title = readmeTitle(workspace)
	fpParts = append(fpParts, "title:"+in.Title)
	in.Cmds = cmdDirs(workspace)
	fpParts = append(fpParts, "cmd:"+strings.Join(in.Cmds, ","))

	in.Facts = memory.Open(workspace).List(memory.ScopeProject)
	sort.Slice(in.Facts, func(i, j int) bool { return in.Facts[i].Name < in.Facts[j].Name })
	if len(in.Facts) > projectFactCap {
		in.Facts = in.Facts[:projectFactCap]
	}
	for _, f := range in.Facts {
		fpParts = append(fpParts, "fact:"+f.Name+"|"+f.Updated.UTC().Format(time.RFC3339)+
			"|"+hashBody(f.Body))
	}

	sum := sha256.Sum256([]byte(strings.Join(fpParts, "\n")))
	in.FP = hex.EncodeToString(sum[:])

	in.Context = renderProjectContext(in)
	if in.Module == "" && in.Title == "" && len(in.Cmds) == 0 && len(in.Facts) == 0 {
		return in, errors.New("project exam: no project material to generate questions from")
	}
	return in, nil
}

// modDirective pulls one top-level go.mod directive ("module x" / "go 1.25").
func modDirective(text, key string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, key+" ") {
			return strings.TrimSpace(strings.TrimPrefix(line, key+" "))
		}
	}
	return ""
}

// readmeTitle returns the first markdown heading of the first README
// variant present, or "" when the project has none.
func readmeTitle(workspace string) string {
	for _, name := range []string{"README.md", "README.markdown", "README", "readme.md"} {
		raw, err := os.ReadFile(filepath.Join(workspace, name))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "# ") {
				return strings.TrimSpace(strings.TrimPrefix(line, "# "))
			}
		}
	}
	return ""
}

// cmdDirs lists the command entrypoint directories under cmd/, sorted.
func cmdDirs(workspace string) []string {
	entries, err := os.ReadDir(filepath.Join(workspace, "cmd"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// hashBody hashes a fact body so an edited fact changes the fingerprint
// without embedding the body in the hash input twice.
func hashBody(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:8])
}

// renderProjectContext is the standing context the answerer carries:
// the structural digest plus the full text of the fact set the
// questions are drawn from, clipped to the context cap.
func renderProjectContext(in projectInputs) string {
	var b strings.Builder
	if in.Module != "" {
		fmt.Fprintf(&b, "module: %s\n", in.Module)
	}
	if in.GoVer != "" {
		fmt.Fprintf(&b, "go: %s\n", in.GoVer)
	}
	if in.Title != "" {
		fmt.Fprintf(&b, "title: %s\n", in.Title)
	}
	if len(in.Cmds) > 0 {
		fmt.Fprintf(&b, "cmd: %s\n", strings.Join(in.Cmds, ", "))
	}
	for _, f := range in.Facts {
		fmt.Fprintf(&b, "\n## %s\n", f.Name)
		if f.Description != "" {
			b.WriteString(f.Description + "\n")
		}
		b.WriteString(strings.TrimSpace(f.Body) + "\n")
	}
	return clipRunes(b.String(), projectContextCap)
}

// generateProjectExam turns gathered inputs into the bank: one question
// per structural fact that exists, plus one per memory fact.
func generateProjectExam(in projectInputs) Exam {
	ex := Exam{Class: ProjectExamClass}
	if in.Module != "" {
		ex.Questions = append(ex.Questions, Question{
			Q:        "What is this project's Go module path?",
			Expected: in.Module,
		})
	}
	if in.GoVer != "" {
		ex.Questions = append(ex.Questions, Question{
			Q:        "Which Go language version does this project target?",
			Expected: in.GoVer,
		})
	}
	if in.Title != "" {
		ex.Questions = append(ex.Questions, Question{
			Q:        "What is this project's title (its README heading)?",
			Expected: in.Title,
		})
	}
	if len(in.Cmds) > 0 {
		ex.Questions = append(ex.Questions, Question{
			Q:        "Name the command entrypoints (directories under cmd/) in this project.",
			Keywords: in.Cmds,
		})
	}
	for _, f := range in.Facts {
		exp := f.Description
		if exp == "" {
			exp = firstLine(f.Body)
		}
		if strings.TrimSpace(exp) == "" {
			continue
		}
		ex.Questions = append(ex.Questions, Question{
			Q:        fmt.Sprintf("According to this project's memory, what is %q?", f.Name),
			Expected: exp,
		})
	}
	return ex
}

// firstLine is the fallback expected value when a fact carries a body
// but no description.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return clipRunes(line, 200)
		}
	}
	return ""
}
