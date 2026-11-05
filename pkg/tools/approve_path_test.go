package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// msysSpelling renders a Windows path the way git-bash spells it:
// C:\a\b → /c/a/b.
func msysSpelling(win string) string {
	vol := filepath.VolumeName(win)
	if vol == "" {
		return win
	}
	return "/" + strings.ToLower(vol[:1]) + strings.ReplaceAll(win[len(vol):], `\`, "/")
}

// TestCommandCrossesRoot: the detection layer of the default-mode gate —
// in-root spellings (plain words, workspace-absolute, msys forms of the
// same, urls) pass without a card; anything reaching outside the root is
// flagged, in every spelling a shell realistically carries it.
func TestCommandCrossesRoot(t *testing.T) {
	root := t.TempDir()
	inAbs := filepath.Join(root, "docs", "report.md")
	outAbs := filepath.Join(filepath.Dir(root), "other-repo", "README.md")

	cases := []struct {
		name string
		cmd  string
		want bool
	}{
		// Inside the workspace: no card.
		{"plain", "ls -la", false},
		{"go test", "go test ./...", false},
		{"git diff dots", "git diff HEAD~1..HEAD", false},
		{"relative inside", "cat docs/report.md", false},
		{"abs inside", "cat " + inAbs, false},
		{"msys inside", "cat " + msysSpelling(inAbs), false},
		{"assign inside", "--out=" + inAbs, false},
		{"subst", "echo $(date)", false},
		{"semicolon tail", "cat docs/report.md;", false},
		{"remote url", "curl https://example.com", false},

		// Outside the workspace: a card.
		{"abs outside", "cat " + outAbs, true},
		{"msys outside", "cat " + msysSpelling(outAbs), true},
		{"drive literal", `type D:\other-repo\x.txt`, true},
		{"parent escape", "cat ../secrets.txt", true},
		{"cd dotdot", "cd ..", true},
		{"quoted space", `grep x "D:\my dir\file.txt"`, true},
		{"unc", `type \\server\share\file.txt`, true},
		{"home tilde", "cat ~/notes.md", true},
		{"home var", "cat $HOME/notes.md", true},
		{"home bare", "cd $HOME", true},
		{"percent env", `type %USERPROFILE%\ntuser.txt`, true},
		{"assign outside", "--out=" + outAbs, true},
		{"assign rel escape", `--out=..\build\x`, true},
		{"redirect outside", "echo hi>" + outAbs, true},
		{"var path", "cat $FILE", true},
		{"file url", "cat file:///D:/x.txt", true},
		{"null dev stderr", "ls .lyon 2>/dev/null", false},
		{"null dev stdin", "wc -l </dev/null", false},
		{"null dev piped", `go vet ./... 2>/dev/null | head`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := commandCrossesRoot(root, tc.cmd); got != tc.want {
				t.Errorf("commandCrossesRoot(%q) = %v, want %v", tc.cmd, got, tc.want)
			}
		})
	}
}

// TestPathAwareApprove: the wrapper — in-root shell and non-shell
// Approvable tools pass without asking; cross-root shell parks on the
// human and honors the answer; unreadable input and nil-next fail closed.
func TestPathAwareApprove(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	mk := func(name, input string) ExecEvent {
		return ExecEvent{Name: name, Input: json.RawMessage(input)}
	}

	var asked int
	yes := func(context.Context, ExecEvent) bool { asked++; return true }
	gate := PathAwareApprove(root, yes)

	// In-root shell passes without asking anyone.
	if !gate(ctx, mk("run_shell", `{"command":"go test ./..."}`)) {
		t.Fatal("in-root command must pass")
	}
	// A stderr redirect into /dev/null is not a cross-root touch.
	if !gate(ctx, mk("run_shell", `{"command":"ls .lyon 2>/dev/null"}`)) {
		t.Fatal("null-device redirect must pass")
	}
	if asked != 0 {
		t.Fatalf("in-root commands asked the human %d times", asked)
	}
	// Cross-root shell parks on the human and honors the approval.
	if !gate(ctx, mk("run_shell", `{"command":"type D:\\other\\x.txt"}`)) {
		t.Fatal("approved cross-root command must run")
	}
	if asked != 1 {
		t.Fatalf("cross-root command asked %d times, want 1", asked)
	}
	// Non-shell Approvable tools pass untouched (their paths are Confine's job).
	if !gate(ctx, mk("write_file", `{"path":"a.txt"}`)) || asked != 1 {
		t.Fatalf("write_file must pass unconditionally (asked=%d)", asked)
	}
	// Unreadable input fails closed to the human.
	if !gate(ctx, mk("run_shell", `{not json`)) || asked != 2 {
		t.Fatalf("unparseable input must ask (asked=%d)", asked)
	}

	// A denial is honored.
	deny := PathAwareApprove(root, func(context.Context, ExecEvent) bool { return false })
	if deny(ctx, mk("run_shell", `{"command":"type C:\\x.txt"}`)) {
		t.Fatal("declined cross-root command must not run")
	}

	// With no human attached: in-root passes, cross-root is denied.
	lone := PathAwareApprove(root, nil)
	if !lone(ctx, mk("run_shell", `{"command":"ls"}`)) {
		t.Fatal("in-root command must pass with next=nil")
	}
	if lone(ctx, mk("run_shell", `{"command":"type C:\\x.txt"}`)) {
		t.Fatal("cross-root command must fail closed with next=nil")
	}
}
