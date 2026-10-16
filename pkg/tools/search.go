package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/anthropics/anthropic-sdk-go"
)

const (
	// searchDefaultMax caps matching lines per call when max_results is
	// unset. The adapter's ResultCap is the byte-level second line of
	// defense; this keeps the walk from grinding through a huge repo once
	// the answer already fits.
	searchDefaultMax = 200
	// searchMaxFileBytes skips files larger than this. A minified bundle or
	// a checked-in database dump would otherwise cost seconds to scan and
	// flood the result cap with one line.
	searchMaxFileBytes = 10 << 20
	// searchMaxContext clamps the context window. A hundred lines either
	// side already dwarfs any real need; unbounded input would turn one
	// match into a whole-file dump.
	searchMaxContext = 1000
)

// searchSkipDirs are noise directories the walk never descends into:
// version-control metadata, dependency trees and compiler caches hold no
// source anybody means to search, and on a modern repo they dominate both
// file count and size.
var searchSkipDirs = map[string]bool{
	".git":             true,
	".hg":              true,
	".svn":             true,
	"node_modules":     true,
	"bower_components": true,
	"__pycache__":      true,
	".venv":            true,
	"venv":             true,
	".tox":             true,
	".nox":             true,
	".mypy_cache":      true,
	".pytest_cache":    true,
	".ruff_cache":      true,
	".cache":           true,
	".gradle":          true,
	".terraform":       true,
	".idea":            true,
}

// searchSkipExts are files never opened: compiled binaries, archives and
// media are not searchable text, and skipping them by extension avoids
// even the read. Temp/backup files (.tmp, .swp) are editor droppings.
var searchSkipExts = map[string]bool{
	".exe": true, ".dll": true, ".so": true, ".dylib": true,
	".a": true, ".o": true, ".obj": true, ".lib": true, ".pdb": true,
	".zip": true, ".tar": true, ".gz": true, ".bz2": true, ".xz": true,
	".7z": true, ".rar": true, ".jar": true, ".war": true, ".class": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
	".ico": true, ".bmp": true, ".pdf": true,
	".woff": true, ".woff2": true, ".ttf": true, ".otf": true, ".eot": true,
	".mp3": true, ".mp4": true, ".wav": true, ".ogg": true, ".mov": true,
	".sqlite": true, ".db": true, ".bin": true, ".dat": true,
	".tmp": true, ".swp": true, ".swo": true,
}

// SearchFiles scans workspace files for a regular expression. Parallel-safe
// (read-only): it spawns nothing and never takes the session's serial lock,
// so a model firing several searches in one turn overlaps them — where the
// same query through run_shell would queue one bash after another.
type SearchFiles struct {
	Root string
}

func (s *SearchFiles) Name() string { return "search_files" }

func (s *SearchFiles) Description() string {
	return "Search workspace file contents for a regular expression (Go syntax) and return path:line:content matches grouped by file — parallel-safe and in-process, no shell involved. " +
		"Prefer this over run_shell grep/rg for finding code: several search_files calls in one turn run concurrently, while shell commands queue serially one behind another. " +
		"glob filters which files are searched (*.go matches at any depth, **/*.md spans directories); context adds N lines around each match (rendered path:line-content). " +
		"Case-insensitive unless case_sensitive=true; results stop at max_results (default 200) and are byte-capped by the session result cap. " +
		"Binary files, .git, node_modules and other noise directories are skipped."
}

func (s *SearchFiles) ParallelSafe() bool { return true }

type searchFilesInput struct {
	Pattern       string `json:"pattern" jsonschema:"required,description=Go regular expression matched against each file's lines (required; e.g. \"func\\s+Main\")"`
	Glob          string `json:"glob" jsonschema:"description=Only search files whose workspace-relative path matches this glob — e.g. *.go at any depth or **/*.md across directories; empty = every file"`
	Context       int    `json:"context" jsonschema:"description=Lines of context to show before and after each match (default 0)"`
	CaseSensitive bool   `json:"case_sensitive" jsonschema:"description=Match case-sensitively; default false = case-insensitive"`
	MaxResults    int    `json:"max_results" jsonschema:"description=Stop after this many matching lines (default 200)"`
}

func (s *SearchFiles) InputSchema() anthropic.BetaToolInputSchemaParam {
	return schemaFor[searchFilesInput]()
}

func (s *SearchFiles) Execute(ctx context.Context, input json.RawMessage) ([]anthropic.BetaToolResultBlockParamContentUnion, error) {
	var in searchFilesInput
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, fmt.Errorf("invalid input: %w", err)
	}
	if in.Pattern == "" {
		return nil, fmt.Errorf("pattern is empty; provide a regular expression (e.g. \"TODO\" or \"func\\s+Main\")")
	}
	pat := in.Pattern
	if !in.CaseSensitive {
		pat = "(?i)" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return nil, fmt.Errorf("invalid pattern %q: %w", in.Pattern, err)
	}
	var globRe *regexp.Regexp
	if g := strings.TrimSpace(in.Glob); g != "" {
		globRe = globRegexp(g)
	}
	ctxLines := in.Context
	if ctxLines < 0 {
		ctxLines = 0
	}
	if ctxLines > searchMaxContext {
		ctxLines = searchMaxContext
	}
	limit := in.MaxResults
	if limit <= 0 {
		limit = searchDefaultMax
	}

	walkRoot, err := Confine(s.Root, ".")
	if err != nil {
		return nil, err
	}

	var (
		out       strings.Builder
		scanned   int  // files opened for matching (post glob/extension/size filters)
		matches   int  // matching lines rendered so far
		hitFiles  int  // files contributing at least one rendered line
		truncated bool // hit max_results before the walk finished
	)
	err = filepath.WalkDir(walkRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if d == nil {
				return walkErr // the workspace root itself is unreadable
			}
			return nil // an unreadable entry: skip it, keep scanning
		}
		if err := ctx.Err(); err != nil {
			return err // deadline/cancel: fail the call rather than half-answer
		}
		rel, err := filepath.Rel(walkRoot, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if searchSkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if globRe != nil && !globRe.MatchString(rel) {
			return nil
		}
		// Regular files and symlinks only (a link is vetted by confine
		// below before any read); devices, sockets and the like are out.
		if t := d.Type(); t&fs.ModeType != 0 && t&fs.ModeSymlink == 0 {
			return nil
		}
		if searchSkipExts[strings.ToLower(filepath.Ext(rel))] {
			return nil
		}
		if info, err := d.Info(); err == nil && info.Size() > searchMaxFileBytes {
			return nil
		}
		scanned++
		// Every read goes through confine: a symlink planted in the
		// workspace cannot pull outside content into the results, and the
		// walk itself never follows directory links out of the root.
		abs, err := Confine(s.Root, rel)
		if err != nil {
			return nil
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return nil
		}
		if len(data) > searchMaxFileBytes || !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
			return nil // binary or oversized: not searchable text
		}
		lines := splitSearchLines(string(data))
		var hitIdx []int
		for i, ln := range lines {
			if re.MatchString(ln) {
				hitIdx = append(hitIdx, i)
			}
		}
		if len(hitIdx) == 0 {
			return nil
		}
		if remain := limit - matches; remain <= 0 {
			// The cap was met exactly by an earlier file: stop without
			// rendering this one (an empty group would read as a bug).
			truncated = true
			return fs.SkipAll
		} else if len(hitIdx) > remain {
			hitIdx, truncated = hitIdx[:remain], true
		}
		matches += len(hitIdx)
		// Union of the [hit-ctx, hit+ctx] windows over a per-line bitmap,
		// so overlapping windows render each line exactly once and a line
		// that is both a match and someone's context renders as a match.
		want := make([]bool, len(lines))
		isHit := make([]bool, len(lines))
		for _, i := range hitIdx {
			isHit[i] = true
			for j := i - ctxLines; j <= i+ctxLines; j++ {
				if j >= 0 && j < len(lines) {
					want[j] = true
				}
			}
		}
		if hitFiles > 0 {
			out.WriteString("\n") // blank line between file groups
		}
		hitFiles++
		for j := 0; j < len(lines); j++ {
			if !want[j] {
				continue
			}
			sep := "-"
			if isHit[j] {
				sep = ":"
			}
			fmt.Fprintf(&out, "%s:%d%s%s\n", rel, j+1, sep, lines[j])
		}
		if truncated {
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if matches == 0 {
		globNote := ""
		if strings.TrimSpace(in.Glob) != "" {
			globNote = fmt.Sprintf(" (glob %q)", in.Glob)
		}
		return TextResult(fmt.Sprintf("no matches for pattern %q%s; scanned %d files", in.Pattern, globNote, scanned)), nil
	}
	if truncated {
		fmt.Fprintf(&out, "\n... [stopped at max_results=%d: %d matches in %d files, %d files scanned — narrow the pattern or raise max_results]\n",
			limit, matches, hitFiles, scanned)
	} else {
		fmt.Fprintf(&out, "\n%d matches in %d files (%d files scanned)\n", matches, hitFiles, scanned)
	}
	return TextResult(out.String()), nil
}

// globRegexp compiles glob into an anchored regular expression matched
// against slash-separated workspace-relative paths. ** spans directories
// (a trailing ** swallows the rest of the path, **\/ also matches zero
// directories), * stays inside one segment, ? one non-slash character. A
// glob without a separator matches the basename at any depth — ripgrep's
// -g convention, so "*.go" finds sub/pkg.go too. Backslashes are treated
// as separators (Windows-style globs) — the glob language has no escape
// syntax to lose.
func globRegexp(glob string) *regexp.Regexp {
	glob = strings.ReplaceAll(glob, "\\", "/")
	glob = strings.TrimPrefix(glob, "./")
	var b strings.Builder
	b.WriteString("^")
	if !strings.Contains(glob, "/") {
		b.WriteString(`(?:.*/)?`)
	}
	for i := 0; i < len(glob); i++ {
		switch glob[i] {
		case '*':
			switch {
			case i+2 < len(glob) && glob[i+1] == '*' && glob[i+2] == '/':
				b.WriteString(`(?:.*/)?`) // "**/" spans zero or more dirs
				i += 2                    // loop increment lands past "**/"
			case i+1 < len(glob) && glob[i+1] == '*':
				b.WriteString(`.*`) // trailing "**" swallows the rest
				i++                 // loop increment lands past "**"
			default:
				b.WriteString(`[^/]*`)
			}
		case '?':
			b.WriteString(`[^/]`)
		default:
			b.WriteString(regexp.QuoteMeta(string(glob[i])))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

// splitSearchLines splits file content into display lines: on "\n", with
// one trailing "\r" per line dropped (CRLF files render and match like
// LF) and no phantom empty line after a final newline — "a\n" is one line.
func splitSearchLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if n := len(lines); lines[n-1] == "" {
		lines = lines[:n-1]
	}
	for i, ln := range lines {
		lines[i] = strings.TrimSuffix(ln, "\r")
	}
	return lines
}
