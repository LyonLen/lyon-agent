package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/anthropics/anthropic-sdk-go"
)

// Symlink resolution is indirected through these variables so the
// containment logic can be exercised against a stubbed filesystem: creating
// a symlink needs a privilege (SeCreateSymbolicLinkPrivilege) that Windows
// sandboxes and most CI boxes do not grant, and shipping an untestable
// security check is not an option. Production wiring is the standard lib.
var (
	evalSymlinks = filepath.EvalSymlinks
	osLstat      = os.Lstat
	osReadlink   = os.Readlink
)

// linkBudget caps how many symlinks a single path may expand. The
// component walk itself terminates at the volume root; only expansions can
// cycle (a -> b -> a), so the budget is spent there and exhaustion fails
// closed — an unresolvable path is never allowed through.
const linkBudget = 32

// confine resolves p relative to root and guarantees the result stays inside
// root. Absolute paths, ".." traversal, drive-relative paths and UNC paths
// that would escape are rejected — and so is anything that only *looks*
// inside the workspace: the path is expanded through symlinks first and
// containment is judged on the real location, so a link planted in the
// workspace cannot point read_file/write_file/edit_file/view_image at the
// rest of the disk.
//
// The returned path keeps the caller's spelling (cleaned, unresolved) —
// only the decision uses real paths, so callers and their tests see no
// change. A path that does not exist yet (write_file's normal case) is
// resolved as far as the nearest existing ancestor, because the symlink
// that matters sits in the directories above the file to be created.
//
// Residual TOCTOU — a component swapped for a symlink between this check
// and the open — is inherent to path-based checks; the approval gate and
// the workspace root are the backstops there.
func confine(root, p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("empty path")
	}
	if root == "" {
		root = "."
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}
	rootAbs = filepath.Clean(rootAbs)
	// The workspace itself is often a link (/tmp on macOS, a junction on
	// Windows). Judge against its real location too, or every path below an
	// unresolved root would compare as an escape.
	resolvedRoot := rootAbs
	if r, err := evalSymlinks(rootAbs); err == nil {
		resolvedRoot = r
	}

	target := p
	if !filepath.IsAbs(p) {
		target = filepath.Join(rootAbs, p)
	}
	target = filepath.Clean(target)

	real, ok := resolveExisting(target, linkBudget)
	if !ok {
		return "", fmt.Errorf("path %q cannot be resolved inside workspace %q", p, rootAbs)
	}
	rel, err := filepath.Rel(resolvedRoot, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes workspace %q", p, rootAbs)
	}
	return target, nil
}

// resolveExisting expands every symlink in path and returns its real
// location. A tail that is not on disk yet is fine: once a component is
// missing it can no longer be a link, so it is appended back verbatim.
// ok is false only when the real location cannot be established (symlink
// cycle beyond the budget, a link that vanished mid-walk) — callers must
// treat that as an escape.
func resolveExisting(path string, budget int) (real string, ok bool) {
	if r, err := evalSymlinks(path); err == nil {
		return r, true
	}
	dir, base := filepath.Dir(path), filepath.Base(path)
	if dir == path { // volume root reached without resolving: use it lexically
		return path, true
	}
	// Walk upward (this recursion strictly shortens the path, so it always
	// terminates), then expand this component if it is a link.
	resolvedDir, ok := resolveExisting(dir, budget)
	if !ok {
		return "", false
	}
	joined := filepath.Join(resolvedDir, base)
	if fi, err := osLstat(joined); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if budget <= 0 {
			return "", false // link cycle: a -> b -> a
		}
		link, err := osReadlink(joined)
		if err != nil {
			return "", false
		}
		if !filepath.IsAbs(link) {
			link = filepath.Join(resolvedDir, link)
		}
		return resolveExisting(filepath.Clean(link), budget-1)
	}
	return joined, true
}

// ReadFile reads a UTF-8 text file inside the workspace, optionally a
// line range (offset/limit) so a large file's segment costs one call
// instead of whole-file reads plus shell paging. Parallel-safe.
type ReadFile struct {
	Root string
}

func (f *ReadFile) Name() string { return "read_file" }

func (f *ReadFile) Description() string {
	return "Read a UTF-8 text file from the workspace. Paths are relative to the workspace root; absolute paths and .. traversal are rejected. " +
		"Optional offset (1-based first line) and limit (max lines) read just a slice of a large file in one call — offset 0 or omitted starts at line 1, limit 0 or omitted runs to the end of the file."
}

func (f *ReadFile) ParallelSafe() bool { return true }

type readFileInput struct {
	Path   string `json:"path" jsonschema:"required,description=File path relative to the workspace root"`
	Offset int    `json:"offset" jsonschema:"description=1-based first line to read; 0 or omitted starts at the beginning of the file"`
	Limit  int    `json:"limit" jsonschema:"description=Maximum number of lines to read; 0 or omitted reads from offset to the end of the file"`
}

func (f *ReadFile) InputSchema() anthropic.BetaToolInputSchemaParam {
	return schemaFor[readFileInput]()
}

func (f *ReadFile) Execute(ctx context.Context, input json.RawMessage) ([]anthropic.BetaToolResultBlockParamContentUnion, error) {
	var in readFileInput
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, fmt.Errorf("invalid input: %w", err)
	}
	abs, err := confine(f.Root, in.Path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", in.Path, err)
	}
	if !utf8.Valid(data) {
		if i := strings.IndexByte(string(data), 0); i >= 0 {
			return nil, fmt.Errorf("%s looks like a binary file; call view_image to view images", in.Path)
		}
		return nil, fmt.Errorf("%s is not valid UTF-8 text", in.Path)
	}
	text := string(data)
	if in.Offset < 0 {
		return nil, fmt.Errorf("offset must be >= 0 (got %d); line numbers are 1-based", in.Offset)
	}
	if in.Limit < 0 {
		return nil, fmt.Errorf("limit must be >= 0 (got %d); 0 means no limit", in.Limit)
	}
	// Offset 0 with limit 0 is the historical call: the whole file, byte
	// for byte — no splitting, so old callers see no change. The session
	// adapter's ResultCap then trims an oversized slice, as it always has.
	if in.Offset > 0 || in.Limit > 0 {
		lines := splitFileLines(text)
		start := 0
		if in.Offset > 0 {
			start = in.Offset - 1
			if start >= len(lines) {
				return nil, fmt.Errorf("offset %d is past the end of %s (%d lines; offsets are 1-based)", in.Offset, in.Path, len(lines))
			}
		}
		end := len(lines)
		if in.Limit > 0 && start+in.Limit < end {
			end = start + in.Limit
		}
		text = strings.Join(lines[start:end], "\n")
	}
	return TextResult(text), nil
}

// splitFileLines splits text into lines addressed by 1-based offsets: on
// "\n", with a final newline not counted as an extra line ("a\n" is one
// line, "a\n\n" is two) — the numbering sed/grep use. \r is kept, so
// joining a full sweep of slices reproduces the original bytes minus that
// final newline.
func splitFileLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	if n := len(lines); lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

// WriteFile writes a UTF-8 text file inside the workspace, creating parent
// directories as needed. Serial (mutating).
type WriteFile struct {
	Root string
}

func (f *WriteFile) Name() string { return "write_file" }

func (f *WriteFile) Description() string {
	return "Write a UTF-8 text file in the workspace, creating parent directories as needed. Overwrites existing content. Paths are relative to the workspace root."
}

func (f *WriteFile) ParallelSafe() bool { return false }

// NeedsApproval implements Approvable: writing mutates the workspace.
func (f *WriteFile) NeedsApproval() bool { return true }

type writeFileInput struct {
	Path    string `json:"path" jsonschema:"required,description=File path relative to the workspace root"`
	Content string `json:"content" jsonschema:"required,description=Full content to write"`
}

func (f *WriteFile) InputSchema() anthropic.BetaToolInputSchemaParam {
	return schemaFor[writeFileInput]()
}

func (f *WriteFile) Execute(ctx context.Context, input json.RawMessage) ([]anthropic.BetaToolResultBlockParamContentUnion, error) {
	var in writeFileInput
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, fmt.Errorf("invalid input: %w", err)
	}
	abs, err := confine(f.Root, in.Path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return nil, fmt.Errorf("create dirs: %w", err)
	}
	if err := os.WriteFile(abs, []byte(in.Content), 0o644); err != nil {
		return nil, fmt.Errorf("write %s: %w", in.Path, err)
	}
	return TextResult(fmt.Sprintf("wrote %d bytes to %s", len(in.Content), in.Path)), nil
}

// EditFile replaces one exact occurrence of old_string in a file. Serial.
type EditFile struct {
	Root string
}

func (f *EditFile) Name() string { return "edit_file" }

func (f *EditFile) Description() string {
	return "Replace an exact substring in a UTF-8 text file. old_string must match exactly once. Paths are relative to the workspace root."
}

func (f *EditFile) ParallelSafe() bool { return false }

// NeedsApproval implements Approvable: editing mutates the workspace.
func (f *EditFile) NeedsApproval() bool { return true }

type editFileInput struct {
	Path      string `json:"path" jsonschema:"required,description=File path relative to the workspace root"`
	OldString string `json:"old_string" jsonschema:"required,description=Exact text to replace (must occur exactly once)"`
	NewString string `json:"new_string" jsonschema:"required,description=Replacement text"`
}

func (f *EditFile) InputSchema() anthropic.BetaToolInputSchemaParam {
	return schemaFor[editFileInput]()
}

func (f *EditFile) Execute(ctx context.Context, input json.RawMessage) ([]anthropic.BetaToolResultBlockParamContentUnion, error) {
	var in editFileInput
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, fmt.Errorf("invalid input: %w", err)
	}
	abs, err := confine(f.Root, in.Path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", in.Path, err)
	}
	oldB := []byte(in.OldString)
	if len(oldB) == 0 {
		return nil, fmt.Errorf("old_string is empty")
	}
	n := strings.Count(string(data), in.OldString)
	switch {
	case n == 0:
		return nil, fmt.Errorf("old_string not found in %s", in.Path)
	case n > 1:
		return nil, fmt.Errorf("old_string occurs %d times in %s; must be unique", n, in.Path)
	}
	out := strings.Replace(string(data), in.OldString, in.NewString, 1)
	if err := os.WriteFile(abs, []byte(out), 0o644); err != nil {
		return nil, fmt.Errorf("write %s: %w", in.Path, err)
	}
	return TextResult(fmt.Sprintf("replaced 1 occurrence in %s", in.Path)), nil
}
