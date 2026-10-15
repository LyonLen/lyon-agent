package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// symlinkOrSkip creates a symlink, skipping the test where the OS refuses
// (Windows without Developer Mode / admin has no SeCreateSymbolicLinkPrivilege).
func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
}

// TestConfineRejectsSymlinkEscape: the whole point of EvalSymlinks in
// confine() — a link planted inside the workspace must not let read_file
// look outside it, and write_file must not follow one out either.
func TestConfineRejectsSymlinkEscape(t *testing.T) {
	root := tempDir(t)
	outside := tempDir(t) // a sibling, definitely not under root
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("classified"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Directory link pointing out of the workspace.
	symlinkOrSkip(t, outside, filepath.Join(root, "out"))

	// File link pointing at an existing file outside.
	symlinkOrSkip(t, filepath.Join(outside, "secret.txt"), filepath.Join(root, "secretlink.txt"))

	// Dangling link: relative, and its target is outside. EvalSymlinks
	// refuses it (target missing), so a naive implementation would accept
	// the path as "inside" — and the write would land outside.
	symlinkOrSkip(t, filepath.Join("..", "dangling_escaped.txt"), filepath.Join(root, "dangle"))

	for _, tc := range []struct{ name, path string }{
		{"read through dir link", "out/secret.txt"},
		{"write through dir link", "out/new.txt"},
		{"nested write through dir link", "out/sub/new.txt"},
		{"read file link", "secretlink.txt"},
		{"write through dangling link", "dangle"},
		{"abs path through dir link", filepath.Join(root, "out", "secret.txt")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := confine(root, tc.path); err == nil {
				t.Fatalf("confine(%q) accepted a path that leaves the workspace", tc.path)
			}
		})
	}
}

// TestConfineSymlinkEscapeWritesNothing: rejection must happen before the
// bytes move — the outside directory stays untouched.
func TestConfineSymlinkEscapeWritesNothing(t *testing.T) {
	root := tempDir(t)
	outside := tempDir(t)
	symlinkOrSkip(t, outside, filepath.Join(root, "out"))

	w := &WriteFile{Root: root}
	in, _ := json.Marshal(writeFileInput{Path: "out/evil.txt", Content: "pwned"})
	if _, err := w.Execute(context.Background(), in); err == nil {
		t.Fatal("write_file followed a symlink out of the workspace")
	}
	if _, err := os.Stat(filepath.Join(outside, "evil.txt")); err == nil {
		t.Fatal("file was created outside the workspace")
	}
}

// TestConfineSymlinkInsideAllowed: links that stay inside are legitimate
// workspace structure (a vendored dir, a checked-in build link) — the check
// must follow them, not forbid them.
func TestConfineSymlinkInsideAllowed(t *testing.T) {
	root := tempDir(t)
	if err := os.MkdirAll(filepath.Join(root, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "real", "a.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, filepath.Join("real"), filepath.Join(root, "alias"))

	got, err := confine(root, "alias/a.txt")
	if err != nil {
		t.Fatalf("confine through an inside link: %v", err)
	}
	rel, err := filepath.Rel(root, got)
	if err != nil || rel == ".." {
		t.Fatalf("resolved %q outside root: %q", got, rel)
	}

	// And the tool really reads it.
	r := &ReadFile{Root: root}
	rb, _ := json.Marshal(readFileInput{Path: "alias/a.txt"})
	out, err := r.Execute(context.Background(), rb)
	if err != nil || len(out) != 1 || out[0].OfText == nil || out[0].OfText.Text != "ok" {
		t.Fatalf("read through inside link: err=%v out=%+v", err, out)
	}
}

// TestConfineWorkspaceRootIsSymlink: /tmp on macOS and junctioned TEMP on
// Windows are links — resolving the root is required or every path under a
// linked workspace would be rejected as an escape.
func TestConfineWorkspaceRootIsSymlink(t *testing.T) {
	real := tempDir(t)
	parent := tempDir(t)
	linked := filepath.Join(parent, "ws-link")
	symlinkOrSkip(t, real, linked)

	if err := os.WriteFile(filepath.Join(real, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := confine(linked, "a.txt")
	if err != nil {
		t.Fatalf("confine under a linked workspace root: %v", err)
	}
	data, err := os.ReadFile(got)
	if err != nil || string(data) != "hi" {
		t.Fatalf("read via linked root: %v %q", err, data)
	}

	w := &WriteFile{Root: linked}
	wb, _ := json.Marshal(writeFileInput{Path: "sub/b.txt", Content: "written"})
	if _, err := w.Execute(context.Background(), wb); err != nil {
		t.Fatalf("write under a linked root: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(real, "sub", "b.txt")); err != nil || string(b) != "written" {
		t.Fatalf("write did not land in the real workspace: %v %q", err, b)
	}
}

// ---------------------------------------------------------------------------
// Deterministic coverage. Creating a real symlink needs a privilege most
// Windows boxes and CI sandboxes do not grant (the OS tests above skip
// there), so the resolution layer is stubbed instead: same code paths,
// same decisions, no admin rights required.
//
// Roots are canonicalised first: Windows temp paths arrive as 8.3 short
// names (LINLIA~1) while EvalSymlinks expands them, and the stub keys are
// exact-path lookups — mixing the two forms would silently miss.
// ---------------------------------------------------------------------------

// canonical expands a directory through EvalSymlinks so the paths handed to
// confine() and the stub's keys agree on spelling.
func canonical(t *testing.T, dir string) string {
	t.Helper()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		return r
	}
	return dir
}

type linkInfo string

func (l linkInfo) Name() string       { return string(l) }
func (l linkInfo) Size() int64        { return 0 }
func (l linkInfo) Mode() os.FileMode  { return os.ModeSymlink | 0o777 }
func (l linkInfo) ModTime() time.Time { return time.Time{} }
func (l linkInfo) IsDir() bool        { return false }
func (l linkInfo) Sys() any           { return nil }

// symlinkStub swaps the resolver for a small in-test filesystem:
//   - links:  paths EvalSymlinks resolves to a real location
//   - dangle: paths that are symlinks whose EvalSymlinks fails (target
//     missing) and whose Lstat reports a link with the given target
type symlinkStub struct {
	links  map[string]string
	dangle map[string]string
}

func newSymlinkStub(t *testing.T) *symlinkStub {
	t.Helper()
	fs := &symlinkStub{links: map[string]string{}, dangle: map[string]string{}}
	eval, lstat, readlink := evalSymlinks, osLstat, osReadlink
	evalSymlinks = func(p string) (string, error) {
		if _, bad := fs.dangle[p]; bad {
			return "", errors.New("dangling symlink")
		}
		if r, ok := fs.links[p]; ok {
			return r, nil
		}
		return eval(p)
	}
	osLstat = func(p string) (os.FileInfo, error) {
		if _, isLink := fs.dangle[p]; isLink {
			return linkInfo(filepath.Base(p)), nil
		}
		return lstat(p)
	}
	osReadlink = func(p string) (string, error) {
		if r, ok := fs.dangle[p]; ok {
			return r, nil
		}
		return readlink(p)
	}
	t.Cleanup(func() { evalSymlinks, osLstat, osReadlink = eval, lstat, readlink })
	return fs
}

// TestConfineSymlinkEscapeStubbed: read/write/edit paths that leave the
// workspace through a link are rejected.
func TestConfineSymlinkEscapeStubbed(t *testing.T) {
	root := canonical(t, tempDir(t))
	outside := canonical(t, tempDir(t)) // physically a sibling, never under root
	fs := newSymlinkStub(t)
	fs.links[filepath.Join(root, "out")] = outside

	for _, tc := range []struct{ name, path string }{
		{"read through dir link", "out/secret.txt"},
		{"write through dir link", "out/new.txt"},
		{"nested write through dir link", "out/sub/new.txt"},
		{"absolute through dir link", filepath.Join(root, "out", "secret.txt")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := confine(root, tc.path); err == nil {
				t.Fatalf("confine(%q) accepted a path that leaves the workspace", tc.path)
			}
		})
	}
}

// TestConfineDanglingSymlinkRejects: EvalSymlinks refuses a link whose
// target is missing, so a naive check sees "nothing to resolve, must be
// inside" — while the write still follows the link straight out.
func TestConfineDanglingSymlinkRejects(t *testing.T) {
	root := canonical(t, tempDir(t))
	fs := newSymlinkStub(t)
	fs.dangle[filepath.Join(root, "dangle")] = filepath.Join("..", "escaped.txt")
	fs.dangle[filepath.Join(root, "dangle-abs")] = filepath.Join(root, "..", "abs-escaped.txt")

	for _, p := range []string{"dangle", "dangle-abs"} {
		if _, err := confine(root, p); err == nil {
			t.Errorf("confine(%q) accepted a dangling link pointing outside", p)
		}
	}
}

// TestConfineLinkCycleFailsClosed: a -> b -> a can never be resolved, so
// the path must be refused rather than falling back to the lexical check
// (which would happily report "inside the workspace").
func TestConfineLinkCycleFailsClosed(t *testing.T) {
	root := canonical(t, tempDir(t))
	fs := newSymlinkStub(t)
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	fs.dangle[a] = "b"
	fs.dangle[b] = "a"

	_, err := confine(root, "a")
	if err == nil {
		t.Fatal("symlink cycle resolved to an accepted path")
	}
	if !strings.Contains(err.Error(), "cannot be resolved") {
		t.Fatalf("cycle must fail closed as unresolvable, got: %v", err)
	}
}

// TestConfineInsideLinkAccepted: links that stay inside the workspace are
// ordinary structure — the check must follow them, not forbid them.
func TestConfineInsideLinkAccepted(t *testing.T) {
	root := canonical(t, tempDir(t))
	if err := os.MkdirAll(filepath.Join(root, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "real", "a.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs := newSymlinkStub(t)
	fs.links[filepath.Join(root, "alias")] = filepath.Join(root, "real")

	got, err := confine(root, "alias/a.txt")
	if err != nil {
		t.Fatalf("inside link rejected: %v", err)
	}
	rel, err := filepath.Rel(root, got)
	if err != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("resolved %q escapes %q (rel=%q err=%v)", got, root, rel, err)
	}
	// A not-yet-created file under the link resolves through it too.
	if _, err := confine(root, "alias/brand-new.txt"); err != nil {
		t.Errorf("write target under an inside link rejected: %v", err)
	}
}

// TestConfineLinkedWorkspaceRoot: /tmp on macOS and junctioned TEMP on
// Windows are links — resolving the root is required or every path under a
// linked workspace would be rejected as an escape.
func TestConfineLinkedWorkspaceRoot(t *testing.T) {
	root := canonical(t, tempDir(t))
	real := canonical(t, tempDir(t))
	if err := os.WriteFile(filepath.Join(real, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs := newSymlinkStub(t)
	fs.links[root] = real

	if _, err := confine(root, "a.txt"); err != nil {
		t.Fatalf("linked workspace root rejected every path: %v", err)
	}
	got, ok := resolveExisting(filepath.Join(root, "a.txt"), linkBudget)
	if !ok || got != filepath.Join(real, "a.txt") {
		t.Fatalf("resolve under a linked root = %q ok=%v, want %q", got, ok, filepath.Join(real, "a.txt"))
	}
}

// TestResolveExistingWalksToNearestAncestor: write_file targets a file that
// is not on disk yet, so resolution stops at the deepest existing
// directory — that directory is where a planted link would sit.
func TestResolveExistingWalksToNearestAncestor(t *testing.T) {
	root := canonical(t, tempDir(t))
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "sub", "new.txt")
	got, ok := resolveExisting(want, linkBudget)
	if !ok {
		t.Fatal("missing tail must still resolve")
	}
	if got != want {
		t.Fatalf("resolveExisting = %q, want %q", got, want)
	}

	// A missing ancestor is fine too: nothing below it exists, so nothing
	// below it can be a link.
	deep := filepath.Join(root, "no", "such", "file.txt")
	got, ok = resolveExisting(deep, linkBudget)
	if !ok || got != deep {
		t.Fatalf("missing ancestor: %q ok=%v, want %q", got, ok, deep)
	}
}
