package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// searchTree builds a small deterministic workspace for the search tests:
// hits in a top-level Go file, a nested Go file and a nested markdown
// file, plus matches planted in noise directories (.git, node_modules)
// that the walk must never surface.
func searchTree(t *testing.T) string {
	t.Helper()
	root := tempDir(t)
	files := map[string]string{
		"a.go":              "package main\n\nfunc alpha() {\n\t// TODO alpha\n}\n",
		"sub/b.go":          "package sub\n\nfunc beta() {\n\t// TODO beta\n}\n",
		"docs/guide.md":     "# Guide\n\nremember the TODO\n",
		".git/config":       "[core]\n# TODO never searched\n",
		"node_modules/x.js": "// TODO dependency noise\n",
	}
	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// runSearch invokes SearchFiles.Execute and joins the text blocks — the
// same path a model's tool_use takes.
func runSearch(t *testing.T, root, input string) (string, error) {
	t.Helper()
	out, err := (&SearchFiles{Root: root}).Execute(context.Background(), json.RawMessage(input))
	if err != nil {
		return "", err
	}
	joined := ""
	for _, c := range out {
		if c.OfText != nil {
			joined += c.OfText.Text
		}
	}
	return joined, nil
}

func mustSearch(t *testing.T, root, input string) string {
	t.Helper()
	out, err := runSearch(t, root, input)
	if err != nil {
		t.Fatalf("search(%s): %v", input, err)
	}
	return out
}

func TestSearchFilesMatches(t *testing.T) {
	out := mustSearch(t, searchTree(t), `{"pattern":"TODO"}`)

	for _, want := range []string{
		"a.go:4:\t// TODO alpha",
		"sub/b.go:4:\t// TODO beta",
		"docs/guide.md:3:remember the TODO",
		"3 matches in 3 files",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// Noise directories stay invisible, hits stay inside the workspace.
	for _, forbid := range []string{".git", "node_modules", "never searched", "dependency noise"} {
		if strings.Contains(out, forbid) {
			t.Errorf("output must not contain %q:\n%s", forbid, out)
		}
	}
}

func TestSearchFilesNoMatch(t *testing.T) {
	out := mustSearch(t, searchTree(t), `{"pattern":"zzz_no_such_symbol_xyz"}`)
	if !strings.Contains(out, "no matches") {
		t.Fatalf("want a no-match report, got: %s", out)
	}
	if !strings.Contains(out, "scanned") {
		t.Errorf("no-match report should say how much was scanned: %s", out)
	}
}

func TestSearchFilesGlob(t *testing.T) {
	root := searchTree(t)

	// Basename glob matches at any depth (rg -g convention).
	out := mustSearch(t, root, `{"pattern":"TODO","glob":"*.go"}`)
	if !strings.Contains(out, "2 matches in 2 files") {
		t.Errorf("glob *.go should hit both Go files:\n%s", out)
	}
	if strings.Contains(out, "guide.md") {
		t.Errorf("glob *.go leaked a markdown hit:\n%s", out)
	}

	// ** spans directories and also matches the top level.
	out = mustSearch(t, root, `{"pattern":"TODO","glob":"**/*.md"}`)
	if !strings.Contains(out, "docs/guide.md:3:") || strings.Contains(out, "a.go") {
		t.Errorf("glob **/*.md wrong hits:\n%s", out)
	}

	// A glob that matches nothing reports cleanly, naming the glob.
	out = mustSearch(t, root, `{"pattern":"TODO","glob":"*.txt"}`)
	if !strings.Contains(out, "no matches") || !strings.Contains(out, `"*.txt"`) {
		t.Errorf("empty glob search should name the glob: %s", out)
	}
}

func TestSearchFilesContextLines(t *testing.T) {
	root := tempDir(t)
	if err := os.WriteFile(filepath.Join(root, "ctx.txt"), []byte("one\ntwo\nthree\nfour\nfive\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := mustSearch(t, root, `{"pattern":"three","context":1}`)
	for _, want := range []string{"ctx.txt:2-two", "ctx.txt:3:three", "ctx.txt:4-four"} {
		if !strings.Contains(out, want) {
			t.Errorf("context output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ctx.txt:1-one") || strings.Contains(out, "ctx.txt:5-five") {
		t.Errorf("context=1 rendered lines outside the window:\n%s", out)
	}

	// Overlapping windows dedupe: adjacent hits share context, and a line
	// that is both a hit and someone's context renders as a hit once.
	out = mustSearch(t, root, `{"pattern":"(?:two|three)","context":1}`)
	for _, want := range []string{"ctx.txt:1-one", "ctx.txt:2:two", "ctx.txt:3:three", "ctx.txt:4-four"} {
		if c := strings.Count(out, want); c != 1 {
			t.Errorf("want exactly one %q (got %d):\n%s", want, c, out)
		}
	}
	if strings.Contains(out, "ctx.txt:5-five") {
		t.Errorf("window over-reached:\n%s", out)
	}
}

func TestSearchFilesCaseSensitivity(t *testing.T) {
	root := searchTree(t)

	if out := mustSearch(t, root, `{"pattern":"todo"}`); !strings.Contains(out, "3 matches") {
		t.Errorf("default must be case-insensitive:\n%s", out)
	}
	if out := mustSearch(t, root, `{"pattern":"todo","case_sensitive":true}`); !strings.Contains(out, "no matches") {
		t.Errorf("case_sensitive=true must not fold case:\n%s", out)
	}
}

func TestSearchFilesPatternErrors(t *testing.T) {
	root := searchTree(t)

	if _, err := runSearch(t, root, `{"pattern":"["}`); err == nil || !strings.Contains(err.Error(), "invalid pattern") {
		t.Errorf("malformed regexp must fail with invalid pattern, got: %v", err)
	}
	if _, err := runSearch(t, root, `{"pattern":""}`); err == nil || !strings.Contains(err.Error(), "pattern is empty") {
		t.Errorf("empty pattern must be rejected, got: %v", err)
	}
}

func TestSearchFilesMaxResultsCap(t *testing.T) {
	root := tempDir(t)
	for i := 0; i < 5; i++ {
		name := filepath.Join(root, fmt.Sprintf("f%d.txt", i))
		if err := os.WriteFile(name, []byte(strings.Repeat("hit\n", 4)), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	out := mustSearch(t, root, `{"pattern":"hit","max_results":3}`)
	if got := strings.Count(out, ":hit"); got != 3 {
		t.Errorf("max_results=3 rendered %d hits:\n%s", got, out)
	}
	if !strings.Contains(out, "stopped at max_results=3") {
		t.Errorf("truncation must be announced:\n%s", out)
	}
}

func TestSearchFilesDefaultResultLimit(t *testing.T) {
	root := tempDir(t)
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte(strings.Repeat("hit\n", 250)), 0o644); err != nil {
		t.Fatal(err)
	}

	out := mustSearch(t, root, `{"pattern":"hit"}`)
	if got := strings.Count(out, ":hit"); got != searchDefaultMax {
		t.Errorf("default cap rendered %d hits, want %d", got, searchDefaultMax)
	}
	if !strings.Contains(out, fmt.Sprintf("stopped at max_results=%d", searchDefaultMax)) {
		t.Errorf("default-cap truncation must be announced:\n%s", out)
	}
}

func TestSearchFilesSkipsBinary(t *testing.T) {
	root := tempDir(t)
	// NUL makes it non-text even though the bytes are valid UTF-8.
	if err := os.WriteFile(filepath.Join(root, "blob"), []byte("TODO\x00\x01binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := mustSearch(t, root, `{"pattern":"TODO"}`)
	if !strings.Contains(out, "no matches") {
		t.Errorf("binary files must be skipped silently, got: %s", out)
	}
}

func TestSearchFilesResultCapThroughAdapter(t *testing.T) {
	root := tempDir(t)
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte(strings.Repeat("needle\n", 500)), 0o644); err != nil {
		t.Fatal(err)
	}
	a := Adapt(&SearchFiles{Root: root}, AdapterConfig{ResultCap: 512})
	out, err := a.Execute(context.Background(), json.RawMessage(`{"pattern":"needle"}`))
	if err != nil {
		t.Fatal(err)
	}
	text := out[0].OfText.Text
	if len(text) > 512+128 {
		t.Fatalf("adapter cap not applied: %d bytes", len(text))
	}
	if !strings.Contains(text, "truncated") {
		t.Fatal("missing truncation marker")
	}
}

// TestSearchFilesConfineSymlinkEscape: a link planted in the workspace
// (file or directory) must not pull outside content into search results.
// Skips where the OS refuses symlinks; the stubbed variant below covers
// the same gate deterministically.
func TestSearchFilesConfineSymlinkEscape(t *testing.T) {
	root := tempDir(t)
	outside := tempDir(t)
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("classified TODO outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "inside.txt"), []byte("TODO inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, filepath.Join(outside, "secret.txt"), filepath.Join(root, "leak.txt"))
	symlinkOrSkip(t, outside, filepath.Join(root, "out"))

	out := mustSearch(t, root, `{"pattern":"TODO|classified"}`)
	if !strings.Contains(out, "inside.txt") {
		t.Errorf("in-workspace hits disappeared:\n%s", out)
	}
	for _, forbid := range []string{"classified", "leak.txt", "out/"} {
		if strings.Contains(out, forbid) {
			t.Errorf("search escaped the workspace (%q):\n%s", forbid, out)
		}
	}
}

// TestSearchFilesConfineSymlinkEscapeStubbed: the same gate without OS
// link privileges — the resolver is stubbed so a regular file on disk
// resolves outside, exactly what confine() must reject before any read.
func TestSearchFilesConfineSymlinkEscapeStubbed(t *testing.T) {
	root := canonical(t, tempDir(t))
	outside := canonical(t, tempDir(t))
	if err := os.WriteFile(filepath.Join(root, "leak.txt"), []byte("SECRET TODO bait"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "inside.txt"), []byte("TODO fine"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs := newSymlinkStub(t)
	fs.links[filepath.Join(root, "leak.txt")] = filepath.Join(outside, "leak.txt")

	out := mustSearch(t, root, `{"pattern":"TODO"}`)
	if !strings.Contains(out, "inside.txt") {
		t.Errorf("in-workspace hits disappeared:\n%s", out)
	}
	if strings.Contains(out, "SECRET") || strings.Contains(out, "leak.txt") {
		t.Errorf("stubbed escape leaked outside content:\n%s", out)
	}
}

// TestSearchFilesConcurrent: five searches over one workspace at once —
// the smoke that ParallelSafe=true delivers real overlap (no serialMu,
// no shell). Correctness only, not timing.
func TestSearchFilesConcurrent(t *testing.T) {
	root := searchTree(t)
	s := &SearchFiles{Root: root}
	if !s.ParallelSafe() {
		t.Fatal("search_files must be parallel-safe")
	}

	var wg sync.WaitGroup
	results := make([]string, 5)
	errs := make([]error, 5)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, err := s.Execute(context.Background(), json.RawMessage(`{"pattern":"TODO"}`))
			if err != nil {
				errs[i] = err
				return
			}
			for _, c := range out {
				if c.OfText != nil {
					results[i] += c.OfText.Text
				}
			}
		}(i)
	}
	wg.Wait()

	for i := range results {
		if errs[i] != nil {
			t.Errorf("concurrent search %d: %v", i, errs[i])
			continue
		}
		if !strings.Contains(results[i], "3 matches in 3 files") {
			t.Errorf("concurrent search %d wrong result:\n%s", i, results[i])
		}
	}
}

func TestGlobRegexp(t *testing.T) {
	cases := []struct {
		glob, path string
		want       bool
	}{
		{"*.go", "a.go", true},
		{"*.go", "sub/a.go", true},
		{"*.go", "sub/a.md", false},
		{"**/*.md", "a.md", true},
		{"**/*.md", "x/y/a.md", true},
		{"**/*.md", "x/y/a.go", false},
		{"src/*.go", "src/a.go", true},
		{"src/*.go", "src/sub/a.go", false},
		{"**/*.go", "x/a.go", true},
		{"sub/**", "sub/a/b.go", true},
		{"?.go", "a.go", true},
		{"?.go", "ab.go", false},
		{"./docs/*.md", "docs/a.md", true},
	}
	for _, tc := range cases {
		if got := globRegexp(tc.glob).MatchString(tc.path); got != tc.want {
			t.Errorf("glob %q vs %q = %v, want %v", tc.glob, tc.path, got, tc.want)
		}
	}
}
