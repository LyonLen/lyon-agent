package tools

import (
	"context"
	"encoding/json"
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
			got, err := confine(root, tc.in)
			if err != nil {
				t.Fatalf("confine(%q) = error %v", tc.in, err)
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
			if got, err := confine(root, in); err == nil {
				t.Fatalf("confine(%q) = %q, want escape error", in, got)
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
			if got, err := confine(root, in); err == nil {
				t.Fatalf("confine(%q) = %q, want escape error", in, got)
			}
		})
	}

	// An absolute path INSIDE the root stays allowed.
	inside := filepath.Join(root, "ok.txt")
	if got, err := confine(root, inside); err != nil {
		t.Fatalf("confine(inside absolute) = %v", err)
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
