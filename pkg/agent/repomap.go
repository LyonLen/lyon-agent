package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Repo-map: a compact orientation note — the workspace's file list plus a
// handful of top-level symbols — injected into the system prompt so the
// model knows the lay of the land before its first search (BACKLOG 1.4;
// ANALYSIS: "repo-map 上下文, Aider 杀手锏 — 省探索轮 = 直接省轮次").
//
// Contract:
//   - byte-stable per session: newSession captures the note once into
//     s.repoMapText, so the prompt-cache prefix never shifts mid-run;
//   - the engine caches the scan for repoMapTTL and the text carries no
//     timestamp — a warm hit returns identical bytes, keeping the
//     cross-session system prefix shareable;
//   - budget repoMapBudget runes, overflow announced explicitly;
//   - "" for an empty/unknown workspace: no header, no bytes, and every
//     exact-equality system-prompt assertion keeps holding.
const (
	// repoMapBudget bounds the note (runes) — it rides inside the cached
	// system prefix, so bloat here taxes every turn.
	repoMapBudget = 4000
	// repoMapTTL bounds staleness of the engine-level scan cache. The
	// note is orientation, not truth: live detail is search_files'
	// job, so a minute of staleness is free.
	repoMapTTL = 60 * time.Second
	// repoMapFileCap caps listed files (path order); the header keeps
	// the real total, the tail says how many were dropped.
	repoMapFileCap = 800
	// repoMapScanCaps bound the symbol pass: per file, symbols per
	// file, and total bytes read — a pathological workspace must not
	// turn session creation into a full-disk scan.
	repoMapSymbolFiles = 400
	repoMapSymsPerFile = 8
	repoMapFileBytes   = 256 << 10
	repoMapScanBytes   = 8 << 20
	// repoMapWalkCap bounds the fallback walk's visited entries.
	repoMapWalkCap = 10000
	// repoMapGitTimeout bounds the ls-files probe; a missing or hung
	// git falls back to the walk.
	repoMapGitTimeout = 5 * time.Second
)

// repoMapSection returns the engine-cached orientation note for the
// workspace, scanning on first use (and after repoMapTTL). "" when the
// workspace is empty or yields no files.
func (e *Engine) repoMapSection() string {
	ws := e.cfg.Workspace
	if ws == "" {
		return ""
	}
	e.repoMapMu.Lock()
	defer e.repoMapMu.Unlock()
	if !e.repoMapAt.IsZero() && time.Since(e.repoMapAt) < repoMapTTL {
		return e.repoMapText
	}
	text := buildRepoMap(ws)
	e.repoMapText = text
	e.repoMapAt = time.Now()
	return text
}

// buildRepoMap scans ws and renders the note. Pure: no cache, no time.
func buildRepoMap(ws string) string {
	files, total := listRepoFiles(ws)
	if len(files) == 0 {
		return ""
	}
	return renderRepoMap(files, total, scanRepoSymbols(ws, files))
}

// listRepoFiles prefers git ls-files (respects .gitignore, covers tracked
// + untracked-but-not-ignored in one shot) and falls back to a bounded
// walk when git is absent or the directory is not a repo. Returns the
// paths to show (capped at repoMapFileCap, slash-normalized, sorted) and
// the real total for the truncation marker.
func listRepoFiles(ws string) ([]string, int) {
	if paths, total, ok := gitListFiles(ws); ok {
		return paths, total
	}
	return walkListFiles(ws)
}

func gitListFiles(ws string) ([]string, int, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), repoMapGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", ws, "ls-files",
		"--cached", "--others", "--exclude-standard")
	out, err := cmd.Output()
	if err != nil {
		return nil, 0, false
	}
	var all []string
	for _, ln := range strings.Split(string(out), "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			all = append(all, filepath.ToSlash(ln))
		}
	}
	if len(all) == 0 {
		// A git repo with no files is still "no map" — but the probe
		// worked, so don't second-guess it with a walk.
		return nil, 0, true
	}
	sort.Strings(all)
	total := len(all)
	if total > repoMapFileCap {
		all = all[:repoMapFileCap]
	}
	return all, total, true
}

// walkListFiles is the non-git fallback: skip heavy/vendor/hidden dirs,
// stop after repoMapWalkCap visited entries, sort for determinism.
func walkListFiles(ws string) ([]string, int) {
	var all []string
	visited := 0
	_ = filepath.WalkDir(ws, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entry: skip, keep walking
		}
		visited++
		if visited > repoMapWalkCap {
			return filepath.SkipAll
		}
		if d.IsDir() {
			if repoMapSkipDir[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(ws, path)
		if rerr != nil {
			return nil
		}
		all = append(all, filepath.ToSlash(rel))
		return nil
	})
	if len(all) == 0 {
		return nil, 0
	}
	sort.Strings(all)
	total := len(all)
	if total > repoMapFileCap {
		all = all[:repoMapFileCap]
	}
	return all, total
}

// repoMapSkipDir names directories the map never lists: VCS internals,
// dependency forests, build outputs, editor state.
var repoMapSkipDir = map[string]bool{
	".git":          true,
	"node_modules":  true,
	"vendor":        true,
	"dist":          true,
	"build":         true,
	"out":           true,
	"target":        true,
	".next":         true,
	".nuxt":         true,
	".venv":         true,
	"venv":          true,
	"__pycache__":   true,
	".idea":         true,
	".vscode":       true,
	".tox":          true,
	".mypy_cache":   true,
	".pytest_cache": true,
	"coverage":      true,
	".cache":        true,
}

// repoMapLangs maps file extension → symbol patterns (simple line
// regexes, ctags-lite: declarations only, no bodies). Languages cover
// this repo's stack plus the common web/py targets; anything else lists
// its path without symbols.
var repoMapLangs = map[string][]*regexp.Regexp{
	".go": {
		regexp.MustCompile(`^\s*func\s+(?:\([^)]*\)\s*)?(\w+)`),
		regexp.MustCompile(`^\s*type\s+(\w+)`),
	},
	".py": {
		regexp.MustCompile(`^\s*(?:async\s+)?def\s+(\w+)`),
		regexp.MustCompile(`^\s*class\s+(\w+)`),
	},
	".ts":  repoMapJSPatterns,
	".tsx": repoMapJSPatterns,
	".js":  repoMapJSPatterns,
	".jsx": repoMapJSPatterns,
	".mjs": repoMapJSPatterns,
	".cjs": repoMapJSPatterns,
	".rs": {
		regexp.MustCompile(`^\s*(?:pub\s+)?(?:async\s+)?fn\s+(\w+)`),
		regexp.MustCompile(`^\s*(?:pub\s+)?(?:struct|enum|trait)\s+(\w+)`),
	},
	".java": repoMapClassPatterns,
	".kt": {
		regexp.MustCompile(`^\s*(?:public\s+|private\s+|internal\s+)?(?:abstract\s+|open\s+|data\s+|sealed\s+)?(?:class|interface|object)\s+(\w+)`),
		regexp.MustCompile(`^\s*(?:public\s+|private\s+|internal\s+)?(?:suspend\s+)?fun\s+(?:<[^>]+>\s*)?(\w+)`),
	},
	".c":   repoMapClassPatterns,
	".h":   repoMapClassPatterns,
	".cpp": repoMapClassPatterns,
	".hpp": repoMapClassPatterns,
	".cc":  repoMapClassPatterns,
	".cs": {
		regexp.MustCompile(`^\s*(?:public\s+|internal\s+|private\s+)?(?:static\s+|sealed\s+|abstract\s+|partial\s+)*(?:class|interface|struct|enum)\s+(\w+)`),
	},
}

var repoMapJSPatterns = []*regexp.Regexp{
	regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s*\*?\s*(\w+)`),
	regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:abstract\s+)?class\s+(\w+)`),
	regexp.MustCompile(`^\s*(?:export\s+)?(?:interface|type)\s+(\w+)`),
}

var repoMapClassPatterns = []*regexp.Regexp{
	regexp.MustCompile(`^\s*(?:class|struct)\s+(\w+)`),
}

// scanRepoSymbols reads up to repoMapSymbolFiles of the given paths (in
// order) and collects up to repoMapSymsPerFile declarations per file,
// stopping once repoMapScanBytes has been read. Map key = slash path.
func scanRepoSymbols(ws string, files []string) map[string][]string {
	syms := make(map[string][]string)
	scanned := 0
	for _, rel := range files {
		if len(syms) >= repoMapSymbolFiles || scanned >= repoMapScanBytes {
			break
		}
		pats, ok := repoMapLangs[strings.ToLower(filepath.Ext(rel))]
		if !ok {
			continue
		}
		path := filepath.Join(ws, filepath.FromSlash(rel))
		b, err := os.ReadFile(path)
		if err != nil || len(b) == 0 {
			continue
		}
		if len(b) > repoMapFileBytes {
			b = b[:repoMapFileBytes]
		}
		scanned += len(b)
		var got []string
		for _, ln := range strings.Split(string(b), "\n") {
			for _, re := range pats {
				if m := re.FindStringSubmatch(ln); m != nil && m[1] != "" {
					got = append(got, m[1])
					break
				}
			}
			if len(got) >= repoMapSymsPerFile {
				break
			}
		}
		if len(got) > 0 {
			syms[rel] = got
		}
	}
	return syms
}

// renderRepoMap lays out the note: header with the real file count, one
// line per shown path (symbols appended after an em dash), and an
// explicit "+N more" tail when the budget or file cap cut the list.
func renderRepoMap(files []string, total int, syms map[string][]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Repo map (%d files)\n", total)
	used := utf8.RuneCountInString(b.String())
	shown := 0
	for _, p := range files {
		line := p
		if s := syms[p]; len(s) > 0 {
			line += " — " + strings.Join(s, ", ")
		}
		n := utf8.RuneCountInString(line) + 1 // + newline
		if used+n > repoMapBudget {
			break
		}
		b.WriteString(line)
		b.WriteByte('\n')
		used += n
		shown++
	}
	if rest := total - shown; rest > 0 {
		fmt.Fprintf(&b, "(+%d more files not shown)\n", rest)
	}
	return b.String()
}
