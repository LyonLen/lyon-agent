package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestConfine(t *testing.T) {
	root := tempDir(t)

	valid := []struct {
		name string
		in   string
	}{
		{"relative simple", "a.txt"},
		{"relative nested", "sub/dir/a.txt"},
		{"dot prefix", "./a.txt"},
		{"internal dotdot ok", "sub/../a.txt"},
		{"absolute inside", filepath.Join(root, "a.txt")},
	}
	for _, tc := range valid {
		t.Run("ok/"+tc.name, func(t *testing.T) {
			got, err := Confine(root, tc.in)
			if err != nil {
				t.Fatalf("Confine(%q) = error %v", tc.in, err)
			}
			rel, err := filepath.Rel(root, got)
			if err != nil || strings.HasPrefix(rel, "..") {
				t.Fatalf("resolved %q outside root: %q", tc.in, got)
			}
		})
	}

	escapes := []string{
		"../evil.txt",
		"..\\evil.txt",
		"sub/../../evil.txt",
		"../../etc/passwd",
		filepath.Join(root, "..", "evil.txt"),
		"",
	}
	for _, in := range escapes {
		t.Run("reject/"+in, func(t *testing.T) {
			if got, err := Confine(root, in); err == nil {
				t.Fatalf("Confine(%q) = %q, want escape error", in, got)
			}
		})
	}
}

// TestConfineWindowsVolumes covers Windows-specific escape shapes: UNC
// shares and absolute paths on the workspace volume but outside its root.
func TestConfineWindowsVolumes(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-only paths")
	}
	root := tempDir(t)

	// Absolute path on the same drive as root, but outside the root.
	vol := filepath.VolumeName(root)
	outside := vol + string(filepath.Separator) + "__lyon_confine_probe__" + string(filepath.Separator) + "evil.txt"
	// UNC and device paths always escape.
	escapes := []string{
		outside,
		`\\server\share\evil.txt`,
		`\\?\C:\evil.txt`,
	}
	for _, in := range escapes {
		t.Run("reject/"+in, func(t *testing.T) {
			if got, err := Confine(root, in); err == nil {
				t.Fatalf("Confine(%q) = %q, want escape error", in, got)
			}
		})
	}

	// An absolute path INSIDE the root stays allowed.
	inside := filepath.Join(root, "ok.txt")
	if got, err := Confine(root, inside); err != nil {
		t.Fatalf("Confine(inside absolute) = %v", err)
	} else if got != inside {
		t.Fatalf("got %q, want %q", got, inside)
	}
}

func TestReadWriteEditRoundTrip(t *testing.T) {
	root := tempDir(t)
	w := &WriteFile{Root: root}
	r := &ReadFile{Root: root}
	e := &EditFile{Root: root}
	ctx := context.Background()

	wb, _ := json.Marshal(writeFileInput{Path: "sub/hello.txt", Content: "hi there"})
	if _, err := w.Execute(ctx, wb); err != nil {
		t.Fatalf("write: %v", err)
	}

	rb, _ := json.Marshal(readFileInput{Path: "sub/hello.txt"})
	out, err := r.Execute(ctx, rb)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(out) != 1 || out[0].OfText == nil || out[0].OfText.Text != "hi there" {
		t.Fatalf("read returned unexpected content: %+v", out)
	}

	eb, _ := json.Marshal(editFileInput{Path: "sub/hello.txt", OldString: "there", NewString: "lyon"})
	if _, err := e.Execute(ctx, eb); err != nil {
		t.Fatalf("edit: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(root, "sub", "hello.txt"))
	if string(data) != "hi lyon" {
		t.Fatalf("after edit: %q", data)
	}

	// Non-unique old_string must fail.
	wb2, _ := json.Marshal(writeFileInput{Path: "dup.txt", Content: "aa bb aa"})
	if _, err := w.Execute(ctx, wb2); err != nil {
		t.Fatalf("write dup: %v", err)
	}
	eb2, _ := json.Marshal(editFileInput{Path: "dup.txt", OldString: "aa", NewString: "y"})
	if _, err := e.Execute(ctx, eb2); err == nil {
		t.Fatal("expected non-unique old_string error")
	}
}

// readSlice runs read_file with an explicit offset/limit input.
func readSlice(t *testing.T, r *ReadFile, in readFileInput) (string, error) {
	t.Helper()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Execute(context.Background(), b)
	if err != nil {
		return "", err
	}
	if len(out) != 1 || out[0].OfText == nil {
		t.Fatalf("unexpected read result: %+v", out)
	}
	return out[0].OfText.Text, nil
}

func TestReadFileOffsetLimit(t *testing.T) {
	root := tempDir(t)
	lines := make([]string, 12)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i+1)
	}
	content := strings.Join(lines, "\n") // no trailing newline
	if err := os.WriteFile(filepath.Join(root, "big.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &ReadFile{Root: root}
	read := func(in readFileInput) (string, error) { return readSlice(t, r, in) }

	// Backward compatible: no offset/limit is the old call, byte for byte.
	if got, err := read(readFileInput{Path: "big.txt"}); err != nil || got != content {
		t.Fatalf("full read = %q, %v; want the whole file verbatim", got, err)
	}

	// Offset only: from that line to the end.
	if got, err := read(readFileInput{Path: "big.txt", Offset: 4}); err != nil || got != strings.Join(lines[3:], "\n") {
		t.Errorf("offset=4 = %q, %v", got, err)
	}

	// Offset + limit: the [offset, offset+limit) window.
	if got, err := read(readFileInput{Path: "big.txt", Offset: 4, Limit: 3}); err != nil || got != "line 4\nline 5\nline 6" {
		t.Errorf("offset=4 limit=3 = %q, %v", got, err)
	}

	// Limit only: the first N lines.
	if got, err := read(readFileInput{Path: "big.txt", Limit: 2}); err != nil || got != "line 1\nline 2" {
		t.Errorf("limit=2 = %q, %v", got, err)
	}

	// The last line is a valid offset; past it is a clear error.
	if got, err := read(readFileInput{Path: "big.txt", Offset: 12}); err != nil || got != "line 12" {
		t.Errorf("offset=12 = %q, %v", got, err)
	}
	if _, err := read(readFileInput{Path: "big.txt", Offset: 13}); err == nil ||
		!strings.Contains(err.Error(), "past the end") || !strings.Contains(err.Error(), "12 lines") {
		t.Errorf("offset past EOF must name the line count, got: %v", err)
	}

	// A limit overrunning the file just stops at the end.
	if got, err := read(readFileInput{Path: "big.txt", Offset: 11, Limit: 100}); err != nil || got != "line 11\nline 12" {
		t.Errorf("limit beyond EOF = %q, %v", got, err)
	}

	// Negative values are rejected, not silently reinterpreted.
	if _, err := read(readFileInput{Path: "big.txt", Offset: -1}); err == nil || !strings.Contains(err.Error(), "offset must be >= 0") {
		t.Errorf("negative offset = %v", err)
	}
	if _, err := read(readFileInput{Path: "big.txt", Limit: -1}); err == nil || !strings.Contains(err.Error(), "limit must be >= 0") {
		t.Errorf("negative limit = %v", err)
	}

	// Paged re-read: consecutive slices stitch back into the original file
	// (the 3-round sed dance, now one call per page).
	var rebuilt strings.Builder
	for start := 1; start <= len(lines); start += 5 {
		seg, err := read(readFileInput{Path: "big.txt", Offset: start, Limit: 5})
		if err != nil {
			t.Fatalf("segment at %d: %v", start, err)
		}
		if rebuilt.Len() > 0 {
			rebuilt.WriteString("\n")
		}
		rebuilt.WriteString(seg)
	}
	if rebuilt.String() != content {
		t.Errorf("segments reassemble to %q, want %q", rebuilt.String(), content)
	}
}

// TestReadFileOffsetLimitTrailingNewline pins the line numbering against
// the sed/grep convention: a final newline does not create a phantom line,
// while an explicit full read still returns the bytes verbatim.
func TestReadFileOffsetLimitTrailingNewline(t *testing.T) {
	root := tempDir(t)
	if err := os.WriteFile(filepath.Join(root, "nl.txt"), []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &ReadFile{Root: root}
	read := func(in readFileInput) (string, error) { return readSlice(t, r, in) }

	if got, err := read(readFileInput{Path: "nl.txt"}); err != nil || got != "a\nb\n" {
		t.Errorf("full read = %q, %v; trailing newline must survive", got, err)
	}
	if got, err := read(readFileInput{Path: "nl.txt", Offset: 1, Limit: 2}); err != nil || got != "a\nb" {
		t.Errorf("offset=1 limit=2 = %q, %v", got, err)
	}
	if got, err := read(readFileInput{Path: "nl.txt", Offset: 2}); err != nil || got != "b" {
		t.Errorf("offset=2 = %q, %v", got, err)
	}
	if _, err := read(readFileInput{Path: "nl.txt", Offset: 3}); err == nil ||
		!strings.Contains(err.Error(), "2 lines") {
		t.Errorf("a\nb\n is two lines; offset=3 must fail with the count, got: %v", err)
	}
}
