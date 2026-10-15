package flow

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

//go:embed builtins/*.json
var builtinFS embed.FS

// slugPat keeps file names and slugs tame — a workflow is referenced by
// slug on the CLI and on disk.
var slugPat = regexp.MustCompile(`[^a-z0-9_-]+`)

// slugify normalizes a slug candidate; empty results are rejected.
func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = slugPat.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// Builtins parses the embedded templates (design §3.3) in stable slug
// order. An error means a shipped file is malformed — surface it, never
// silently ship half a toolbox.
func Builtins() ([]Workflow, error) {
	entries, err := fs.ReadDir(builtinFS, "builtins")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var out []Workflow
	for _, name := range names {
		data, err := builtinFS.ReadFile("builtins/" + name)
		if err != nil {
			return nil, err
		}
		var wf Workflow
		if err := json.Unmarshal(data, &wf); err != nil {
			return nil, fmt.Errorf("builtin %s: %w", name, err)
		}
		if err := validate(wf); err != nil {
			return nil, fmt.Errorf("builtin %s: %w", name, err)
		}
		out = append(out, wf)
	}
	return out, nil
}

// validate catches the structural mistakes that would only surface
// mid-run: no nodes, nameless nodes, unknown exit kinds, dangling jumps.
func validate(wf Workflow) error {
	if slugify(wf.Slug) == "" {
		return fmt.Errorf("empty slug")
	}
	if len(wf.Nodes) == 0 {
		return fmt.Errorf("no nodes")
	}
	names := map[string]bool{}
	for _, n := range wf.Nodes {
		if n.Name == "" {
			return fmt.Errorf("nameless node")
		}
		if names[n.Name] {
			return fmt.Errorf("duplicate node %q", n.Name)
		}
		names[n.Name] = true
	}
	for _, n := range wf.Nodes {
		switch k := n.Exit.Kind; k {
		case "", ExitAlways, ExitCommand, ExitContains, ExitJudge:
		default:
			return fmt.Errorf("node %q: unknown exit kind %q", n.Name, k)
		}
		if n.Exit.Kind == ExitCommand && n.Exit.Command == "" {
			return fmt.Errorf("node %q: command exit without a command", n.Name)
		}
		if n.Exit.Kind == ExitContains && n.Exit.Contains == "" {
			return fmt.Errorf("node %q: contains exit without a marker", n.Name)
		}
		if n.Exit.Kind == ExitJudge && n.Exit.Judge == "" {
			return fmt.Errorf("node %q: judge exit without a question", n.Name)
		}
	}
	// Jumps must land somewhere.
	for _, n := range wf.Nodes {
		if n.OnPass != "" && !names[n.OnPass] {
			return fmt.Errorf("node %q: on_pass target %q not found", n.Name, n.OnPass)
		}
		if strings.HasPrefix(n.Fallback, GotoPrefix) {
			if target := strings.TrimPrefix(n.Fallback, GotoPrefix); !names[target] {
				return fmt.Errorf("node %q: goto target %q not found", n.Name, target)
			}
		}
	}
	return nil
}

// UserDir is where editable copies live ($LYON_HOME/workflows).
func UserDir() string {
	home := os.Getenv("LYON_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			home = ".lyon"
		} else {
			home = filepath.Join(h, ".lyon")
		}
	}
	return filepath.Join(home, "workflows")
}

// LoadDir reads every *.json workflow in dir; a broken file is skipped
// (a typo in one saved workflow must not take the toolbox down).
func LoadDir(dir string) []Workflow {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Workflow
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var wf Workflow
		if err := json.Unmarshal(data, &wf); err != nil {
			continue
		}
		if validate(wf) != nil {
			continue
		}
		out = append(out, wf)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}

// Load is the effective toolbox: builtins overlaid by user workflows of
// the same slug (the "checkout, modify, override" loop of §3.1), then
// any new slugs the user added.
func Load() ([]Workflow, error) {
	base, err := Builtins()
	if err != nil {
		return nil, err
	}
	bySlug := map[string]int{}
	for i, wf := range base {
		bySlug[wf.Slug] = i
	}
	for _, wf := range LoadDir(UserDir()) {
		if i, ok := bySlug[wf.Slug]; ok {
			base[i] = wf
		} else {
			base = append(base, wf)
		}
	}
	return base, nil
}

// Save writes a workflow to the user directory (the /flow save target).
func Save(wf Workflow) error {
	wf.Slug = slugify(wf.Slug)
	if err := validate(wf); err != nil {
		return err
	}
	dir := UserDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(wf, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, wf.Slug+".json")
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
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

// Checkout copies a builtin into the user directory so it can be edited
// without touching the shipped file. Returns the user-dir path.
func Checkout(slug string) (string, error) {
	all, err := Builtins()
	if err != nil {
		return "", err
	}
	for _, wf := range all {
		if wf.Slug == slugify(slug) {
			if err := Save(wf); err != nil {
				return "", err
			}
			return filepath.Join(UserDir(), wf.Slug+".json"), nil
		}
	}
	return "", fmt.Errorf("no builtin workflow %q", slug)
}
