package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The @ file picker claims "../ escapes confined" (atfiles.go). This pins
// that claim against the symlink case, which is the one a lexical
// filepath.Rel check cannot see — and which pkg/tools/confine has the
// same hole in (see files.go). A red here is the picker listing (and
// offering to insert) paths outside the workspace.

// tryDirSymlink creates a directory symlink, reporting whether the
// platform let us. Windows needs Developer Mode or admin for a dir link,
// so a refusal skips rather than fails — the POSIX run still covers it.
func tryDirSymlink(t *testing.T, link, target string) bool {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Logf("skipping: cannot create directory symlink here (%v)", err)
		return false
	}
	return true
}

// TestAtEntriesConfinesSymlinkedDir: a symlink inside the workspace that
// points outside it must not be browsable as a way out. The token is
// written exactly as the user would type it.
func TestAtEntriesConfinesSymlinkedDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Directory symlinks on Windows commonly require elevation; the
		// check below still runs when tryDirSymlink succeeds.
		t.Log("windows: attempting symlink, skipping if unsupported")
	}
	ws := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("s"), 0o600); err != nil {
		t.Fatalf("seed outside file: %v", err)
	}
	if !tryDirSymlink(t, filepath.Join(ws, "escape"), outside) {
		t.Skip("directory symlinks unsupported here")
	}

	// Browsing the symlinked directory itself lists the target's contents.
	for _, tok := range []string{"escape/", "escape/se"} {
		got := atEntries(ws, tok)
		for _, item := range got {
			if item.cmd == "@escape/secret.txt" {
				t.Errorf("atEntries(%q) offered %q — a symlinked dir escaped the workspace; "+
					"the picker must resolve the link before listing", tok, item.cmd)
			}
		}
	}

	// And the top-level listing must not present it as a browsable dir
	// that leads out.
	got := atEntries(ws, "")
	for _, item := range got {
		if item.cmd == "@escape/" {
			t.Errorf("top-level listing offers %q; either confine it or label it as a "+
				"link out of the workspace", item.cmd)
		}
	}
}

// TestAtEntriesConfinesDotDot: the lexical half of the same claim, kept as
// a regression guard so a future refactor of withinDir cannot silently
// reopen it.
func TestAtEntriesConfinesDotDot(t *testing.T) {
	ws := t.TempDir()
	// Seed something outside so a leak would actually be visible.
	outside := filepath.Join(filepath.Dir(ws), "outside-marker.txt")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed outside marker: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })

	for _, tok := range []string{"../", "../outside-marker", "..", "../outside-marker.txt"} {
		if got := atEntries(ws, tok); len(got) != 0 {
			t.Errorf("atEntries(%q) = %v, want nothing — ../ escapes must list empty", tok, got)
		}
	}
}

// TestWithinDirIsLexicalOnly documents the actual guarantee so the gap is
// visible in the test suite rather than only in review: withinDir decides
// on the literal string, so a child that reads as inside may still resolve
// outside once the OS follows links.
func TestWithinDirIsLexicalOnly(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	if !tryDirSymlink(t, filepath.Join(ws, "link"), outside) {
		t.Skip("directory symlinks unsupported here")
	}
	if !withinDir(ws, filepath.Join(ws, "link")) {
		t.Log("withinDir already rejects the symlinked child — hardening landed")
		return
	}
	t.Errorf("withinDir(%q, %q/link) = true; it is purely lexical and does not "+
		"resolve symlinks — resolve the link (or its nearest existing ancestor) "+
		"before the Rel check", ws, ws)
}
