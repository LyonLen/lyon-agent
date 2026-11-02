package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"unicode"
)

// PathAwareApprove wraps next into the default-mode approval gate: only a
// run_shell command whose text reaches outside the workspace root parks a
// card for the human; every other execution passes untouched. This is the
// mechanism behind "default mode restricts cross-root access" — file tools
// cannot cross (Confine judges every path they take) and in-root shell is
// the normal work the gate must not tax, while a command touching another
// repo is exactly the case worth a second pair of eyes.
//
// Detection is lexical and best-effort — the same philosophy as the shell
// allowlist: the command text is scanned for absolute spellings (drive
// paths, UNC shares, msys /d/..., bare /..., ~ and $VAR/%VAR% expansions)
// and each candidate is judged with Confine; ".." traversal on relative
// words is judged the same way. Constructed paths (a variable assembled
// from pieces) can slip past it, so this raises the bar and puts a human
// on the interesting cases — it is not a sandbox.
//
// Failure behavior is conservative: a command whose JSON cannot be read
// goes to next (denied outright when next is nil), because an unreadable
// invocation is not provably safe. Non-shell Approvable tools pass
// unconditionally: their paths already go through Confine at execution,
// and default mode does not tax them — that is what -approve is for.
func PathAwareApprove(root string, next ApproveFunc) ApproveFunc {
	rootAbs := root
	if a, err := filepath.Abs(root); err == nil {
		rootAbs = filepath.Clean(a)
	}
	return func(ctx context.Context, ev ExecEvent) bool {
		if ev.Name != "run_shell" {
			return true
		}
		var in shellInput
		if err := json.Unmarshal(ev.Input, &in); err != nil {
			return next != nil && next(ctx, ev)
		}
		if strings.TrimSpace(in.Command) == "" {
			return true // the tool rejects empty commands itself
		}
		if !commandCrossesRoot(rootAbs, in.Command) {
			return true
		}
		return next != nil && next(ctx, ev)
	}
}

// commandCrossesRoot reports whether cmd's text references a path outside
// root. The command is tokenized on whitespace with quotes honored (so
// "D:\my dir\x" stays one token), then each token is split on "=" — the
// value side of --out=D:\x carries the path — and each part judged.
func commandCrossesRoot(root, cmd string) bool {
	for _, tok := range splitCommandTokens(cmd) {
		for _, part := range strings.Split(tok, "=") {
			if partCrossesRoot(root, part) {
				return true
			}
		}
	}
	return false
}

// partCrossesRoot judges one path-shaped fragment of the command. Order
// matters: URLs are not filesystem access (file:// is), shell control
// operators are split out first so `echo hi>D:\x` yields the path as its
// own fragment, and absolute spellings are converted (msys) or taken as-is
// before the bare-prefix rules — so an absolute path INSIDE the workspace
// (D:\ws\f, /d/ws/f) passes while everything else absolute asks.
func partCrossesRoot(root, p string) bool {
	p = trimPartPunct(p)
	if p == "" {
		return false
	}
	if strings.Contains(p, "://") {
		return strings.HasPrefix(p, "file://") // file:// reads the fs; http(s):// does not
	}
	if strings.ContainsAny(p, "<>;|&") {
		for _, sub := range strings.FieldsFunc(p, func(r rune) bool {
			return r == '<' || r == '>' || r == ';' || r == '|' || r == '&'
		}) {
			if partCrossesRoot(root, sub) {
				return true
			}
		}
		return false
	}
	if hasDrivePrefix(p) {
		return confineEscapes(root, p)
	}
	if w, ok := msysToWindows(p); ok {
		return confineEscapes(root, w)
	}
	if strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, `//`) {
		return true // a UNC share is outside a local root (a UNC root is rare enough to ask)
	}
	if strings.HasPrefix(p, "/") {
		return true // the workspace is a Windows path; a bare /... can only be outside it
	}
	if strings.HasPrefix(p, "~") || isEnvExpansion(p) {
		return true // $HOME, %USERPROFILE%, ~ — statically unresolvable, so ask
	}
	return confineEscapes(root, p) // relative words: ".." escapes flag, plain names resolve inside
}

// splitCommandTokens splits a command on whitespace, keeping quoted spans
// together (quotes are consumed, content kept — `cat "D:\my dir\x"` yields
// the path as ONE token so its spaces do not shred it).
func splitCommandTokens(cmd string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range cmd {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote = r
		case unicode.IsSpace(r):
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// trimPartPunct strips shell punctuation clinging to a fragment: the
// redirect/separator chars at the very edges (`cat f;`, `>D:\x`) plus
// quote leftovers, so the shape rules see the path itself.
func trimPartPunct(p string) string {
	return strings.TrimFunc(p, func(r rune) bool {
		switch r {
		case '<', '>', '|', '&', '(', ')', ',', ';', '`', '"', '\'':
			return true
		}
		return false
	})
}

// hasDrivePrefix reports whether p starts with a Windows drive-absolute
// path (D:\x or D:/x). Drive-RELATIVE spellings (D:x) fall to the
// relative rule — bash never writes them, and flagging letter:colon
// blindly would card every "a:b" grep pattern.
func hasDrivePrefix(p string) bool {
	if len(p) < 3 || p[1] != ':' || (p[2] != '\\' && p[2] != '/') {
		return false
	}
	c := p[0]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// msysToWindows converts a leading msys/git-bash absolute path (/d/x →
// D:/x, bare /d → D:\) so Confine can judge it. ok is false when p does
// not start with that spelling — an embedded /a/b inside a longer word
// (foo/a/b) is a relative path, not a msys volume.
func msysToWindows(p string) (string, bool) {
	if len(p) < 2 || p[0] != '/' {
		return "", false
	}
	c := p[1]
	if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
		return "", false
	}
	up := c
	if up >= 'a' && up <= 'z' {
		up = up - 'a' + 'A'
	}
	if len(p) == 2 {
		return string(rune(up)) + `:\`, true
	}
	if p[2] != '/' && p[2] != '\\' {
		return "", false
	}
	return string(rune(up)) + ":" + p[2:], true
}

// isEnvExpansion reports whether p starts with a shell variable
// expansion ($HOME, %USERPROFILE%) — statically unresolvable to a path,
// so it goes to the human.
func isEnvExpansion(p string) bool {
	if len(p) < 2 {
		return false
	}
	ident := func(b byte) bool {
		return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_'
	}
	switch p[0] {
	case '$', '%':
		return ident(p[1])
	}
	return false
}

// confineEscapes judges one path fragment with Confine: an error means
// the fragment cannot be proven to stay inside the root (escape,
// unresolvable, symlink out) — for an approval gate that is "ask", never
// "pass".
func confineEscapes(root, p string) bool {
	if strings.TrimSpace(p) == "" {
		return false
	}
	_, err := Confine(root, p)
	return err != nil
}
