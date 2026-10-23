package flow

// Script fixation (2026-10-23 重定义): a tool sequence that keeps working
// should stop costing model tokens. Freeze turns a green run's command
// list into a plain script on disk — execution calls no AI at all — plus
// a JSON manifest so tools and panels can list it. The script is a
// composite unit: the model invokes the whole sequence as ONE flow tool
// call instead of re-deriving every step.
//
// Both artifacts land in $LYON_HOME/flows/:
//   <slug>.json  manifest — steps, declared args, provenance, run count
//   <slug>.sh    the script itself (also what a human runs by hand)
//
// Safety stays with the executor, not the file: steps run with the
// workspace as cwd and the same approval gate as any run_shell — freezing
// a sequence never grants it new privileges.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Script is one frozen workflow: an ordered command list with named
// argument slots. {{name}} placeholders in a step are substituted from
// Args at run time; a placeholder with no supplied value fails the run
// before any step executes — a half-substituted command must never run.
type Script struct {
	Slug        string            `json:"slug"`
	Description string            `json:"description,omitempty"`
	Steps       []string          `json:"steps"`
	Args        []string          `json:"args,omitempty"`
	Workspace   string            `json:"workspace,omitempty"`
	Source      string            `json:"source,omitempty"` // workflow slug or prompt it was distilled from
	Runs        int               `json:"runs,omitempty"`   // green runs backing this freeze
	FrozenAt    time.Time         `json:"frozen_at,omitempty"`
	Extra       map[string]string `json:"extra,omitempty"`
}

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

var scriptMu sync.Mutex

// FlowsDir is the fixation directory: $LYON_HOME/flows, mirroring the
// streak ledger's home resolution.
func FlowsDir() string {
	home := os.Getenv("LYON_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			h = "."
		}
		home = filepath.Join(h, ".lyon")
	}
	return filepath.Join(home, "flows")
}

// FreezeScript validates s and writes the manifest + script file
// atomically (tmp + rename). An existing slug is replaced only when the
// new freeze carries at least as much evidence (Runs never regresses a
// script to a thinner one on accident).
func FreezeScript(s Script) error {
	if !slugRe.MatchString(s.Slug) {
		return fmt.Errorf("flow: slug %q must match %s", s.Slug, slugRe)
	}
	if len(s.Steps) == 0 {
		return errors.New("flow: a frozen script needs at least one step")
	}
	for _, st := range s.Steps {
		if strings.TrimSpace(st) == "" {
			return errors.New("flow: empty step")
		}
	}
	if s.FrozenAt.IsZero() {
		s.FrozenAt = time.Now().UTC()
	}

	scriptMu.Lock()
	defer scriptMu.Unlock()

	if old, err := loadScriptLocked(s.Slug); err == nil && old.Runs > s.Runs {
		s.Runs = old.Runs
	}
	dir := FlowsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(dir, s.Slug+".json"), data); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, s.Slug+".sh"), []byte(renderScript(s)))
}

// renderScript turns the manifest into a POSIX sh file a human (or any
// shell) can run directly. Named args become $1..$n positionals in
// declared order — the .sh is the manual twin of the JSON manifest.
func renderScript(s Script) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "#!/bin/sh\n# frozen workflow %q — %s\n", s.Slug, s.Description)
	if len(s.Args) > 0 {
		fmt.Fprintf(&b, "# usage: %s.sh", s.Slug)
		for _, a := range s.Args {
			fmt.Fprintf(&b, " <%s>", a)
		}
		b.WriteString("\n")
		for i, a := range s.Args {
			fmt.Fprintf(&b, "%s=\"$%d\"\n", shVar(a), i+1)
		}
	}
	b.WriteString("set -e\n")
	if s.Workspace != "" {
		fmt.Fprintf(&b, "cd %s\n", s.Workspace)
	}
	for _, st := range s.Steps {
		if len(s.Args) > 0 {
			for _, a := range s.Args {
				st = strings.ReplaceAll(st, "{{"+a+"}}", "$"+shVar(a))
			}
		}
		b.WriteString(st + "\n")
	}
	return b.String()
}

// shVar maps an arg name to a shell-safe variable name, one rune at a
// time — a whole-match regexp would swallow the entire name.
func shVar(name string) string {
	var b strings.Builder
	b.WriteString("LYON_ARG_")
	for _, r := range strings.ToUpper(name) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}

func writeAtomic(path string, data []byte) error {
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

func loadScriptLocked(slug string) (Script, error) {
	var s Script
	data, err := os.ReadFile(filepath.Join(FlowsDir(), slug+".json"))
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("flow: parse %s: %w", slug, err)
	}
	return s, nil
}

// LoadScript reads one frozen workflow (os.ErrNotExist when absent).
func LoadScript(slug string) (Script, error) {
	if !slugRe.MatchString(slug) {
		return Script{}, fmt.Errorf("flow: slug %q must match %s", slug, slugRe)
	}
	scriptMu.Lock()
	defer scriptMu.Unlock()
	return loadScriptLocked(slug)
}

// ListScripts returns every frozen workflow, sorted by slug.
func ListScripts() []Script {
	entries, err := os.ReadDir(FlowsDir())
	if err != nil {
		return nil
	}
	var out []Script
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if s, err := LoadScript(strings.TrimSuffix(e.Name(), ".json")); err == nil {
			out = append(out, s)
		}
	}
	return out
}

// StepResult is one executed step's verdict.
type StepResult struct {
	Index  int    `json:"index"`
	Step   string `json:"step"`
	Exit   int    `json:"exit"`
	Output string `json:"output,omitempty"` // combined tail, clipped
	Err    string `json:"err,omitempty"`
}

// ScriptResult is a whole run: OK means every step exited 0.
type ScriptResult struct {
	Slug  string       `json:"slug"`
	OK    bool         `json:"ok"`
	Steps []StepResult `json:"steps"`
}

// scriptStepCap clips one step's captured output in the result.
const scriptStepCap = 4000

// scriptTimeout bounds one whole script run.
const scriptTimeout = 10 * time.Minute

// RunScript executes the frozen steps in order, substituting {{name}}
// placeholders from args. It stops at the first failing step: a frozen
// sequence is a chain, and steps after a broken link were never validated
// against this state. No model is consulted — this is the point.
func RunScript(ctx context.Context, slug string, args map[string]string, workspace string) (ScriptResult, error) {
	s, err := LoadScript(slug)
	if err != nil {
		return ScriptResult{}, err
	}
	for _, a := range s.Args {
		if _, ok := args[a]; !ok {
			return ScriptResult{}, fmt.Errorf("flow: missing arg %q (declared by %s)", a, slug)
		}
	}
	for k := range args {
		found := false
		for _, a := range s.Args {
			if a == k {
				found = true
				break
			}
		}
		if !found {
			return ScriptResult{}, fmt.Errorf("flow: unexpected arg %q (declared: %v)", k, s.Args)
		}
	}
	root := workspace
	if root == "" {
		root = s.Workspace
	}

	ctx, cancel := context.WithTimeout(ctx, scriptTimeout)
	defer cancel()

	// Substitute every step up front and reject any leftover placeholder
	// before executing anything: a half-substituted chain must never run
	// even its early steps.
	steps := make([]string, len(s.Steps))
	for i, raw := range s.Steps {
		step := raw
		for name, val := range args {
			step = strings.ReplaceAll(step, "{{"+name+"}}", val)
		}
		if strings.Contains(step, "{{") {
			return ScriptResult{Slug: slug, OK: false}, fmt.Errorf(
				"flow: step %d has an unsubstituted placeholder: %s", i, step)
		}
		steps[i] = step
	}

	res := ScriptResult{Slug: slug, OK: true}
	for i, step := range steps {
		out, code, runErr := runStep(ctx, step, root)
		sr := StepResult{Index: i, Step: step, Exit: code, Output: clipTail(out, scriptStepCap)}
		if runErr != nil {
			sr.Err = runErr.Error()
		}
		res.Steps = append(res.Steps, sr)
		if code != 0 || runErr != nil {
			res.OK = false
			return res, nil // stop at the first failing step
		}
	}

	// A completed green run is evidence: bump the manifest's run count
	// (best-effort — the run itself already succeeded).
	scriptMu.Lock()
	if cur, err := loadScriptLocked(slug); err == nil {
		cur.Runs++
		if data, err := json.MarshalIndent(cur, "", "  "); err == nil {
			_ = writeAtomic(filepath.Join(FlowsDir(), slug+".json"), data)
		}
	}
	scriptMu.Unlock()
	return res, nil
}

// runStep executes one shell line with the workspace as cwd.
func runStep(ctx context.Context, step, dir string) (string, int, error) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/c", step)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", step)
	}
	if dir != "" {
		cmd.Dir = dir
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
		err = nil // a non-zero exit is a verdict, not an execution error
	} else if err != nil {
		code = -1
	}
	return buf.String(), code, err
}

// clipTail keeps the tail of a step's output — the end is where errors
// explain themselves.
func clipTail(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return "…" + string(r[len(r)-max:])
}
