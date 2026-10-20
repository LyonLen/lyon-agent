package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
)

// Shell command concurrency (#33). A shell command is not inherently a
// mutation: `ls`, `cat`, `git status` can all run side by side with another
// command or a file edit. Two commands that write the tree cannot. The
// decision is a cheap keyword screen first — a command that hits nothing
// runs concurrently with no model call at all — and only a hit is escalated
// to the session's judge (a tiny small-model call), because keywords are
// heuristics: `git status` mentions git, `grep "rm "` mentions rm.
//
// Safety direction: when no judge is wired (probes, flows, tests), a hit
// falls back to the exclusive lock. The screen only widens concurrency for
// commands it recognized as read-only.

// shellAlwaysMutate are command words that modify the tree whatever their
// arguments say.
var shellAlwaysMutate = map[string]struct{}{
	// POSIX / Git bash
	"rm": {}, "mv": {}, "cp": {}, "mkdir": {}, "rmdir": {}, "touch": {},
	"chmod": {}, "chown": {}, "ln": {}, "truncate": {}, "dd": {}, "tee": {},
	"install": {}, "patch": {}, "sed": {}, "unzip": {}, "tar": {}, "rsync": {},
	"python": {}, "python3": {}, "node": {}, "npx": {}, "deno": {},
	"sh": {}, "bash": {}, "zsh": {}, "pwsh": {}, "powershell": {}, "cmd": {},
	"source": {}, "eval": {}, "make": {}, "cmake": {}, "gradle": {}, "mvn": {},
	// Windows cmd / PowerShell
	"del": {}, "erase": {}, "copy": {}, "move": {}, "ren": {}, "rename": {},
	"md": {}, "rd": {}, "xcopy": {}, "robocopy": {}, "copy-item": {},
	"move-item": {}, "remove-item": {}, "new-item": {}, "set-content": {},
	"add-content": {}, "out-file": {}, "rename-item": {}, "clear-content": {},
	"invoke-webrequest": {}, "invoke-restmethod": {}, "start-process": {},
	"curl": {}, "wget": {},
}

// shellReadOnlySub are package managers and VCS drivers whose default action
// is inspection: only a subcommand OUTSIDE these sets escalates. So
// `git status` and `go test` overlap with everything else, while
// `git commit`, `go get`, `go generate` and `npm install` escalate.
// Missing subcommand (`go`, bare `npm`) also escalates.
var shellReadOnlySub = map[string]map[string]struct{}{
	"go": setOf("build", "test", "vet", "env", "version", "list", "doc"),
	// `git config`/`git remote` are deliberately absent: without --get they
	// write. They escalate, and the judge is free to call them parallel.
	"git":  setOf("status", "log", "diff", "show", "rev-parse", "blame", "ls-files", "describe"),
	"npm":  setOf("ls", "list", "view", "outdated", "prefix", "--version", "-v"),
	"pnpm": setOf("ls", "list", "view", "outdated", "--version", "-v"),
	"yarn": setOf("list", "info", "--version", "-v"),
	"pip":  setOf("list", "show", "freeze", "--version"),
	"pip3": setOf("list", "show", "freeze", "--version"),
}

// shellOutputFlags are flags that turn an otherwise read-only command into a
// writer: `--output`/`--out` name a destination file, `find -delete` removes
// files, `-exec` runs an arbitrary command on what it found.
var shellOutputFlags = setOf("--output", "--out", "-delete", "-exec", "-execdir")

// shellBuildWords are compilers whose `-o` means "write an artifact here".
// Checked separately because `grep -o` and `ls -o` are read-only.
var shellBuildWords = setOf("go", "cargo", "rustc", "gcc", "g++", "cc", "clang", "tsc", "javac", "dotnet", "msbuild")

func setOf(words ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(words))
	for _, w := range words {
		m[w] = struct{}{}
	}
	return m
}

// shellSerialKey classifies one command: "" means read-only and free to
// overlap; GlobalKey means it may write the workspace. Only the latter is
// escalated to the judge, so the common case costs no tokens.
func shellSerialKey(ctx context.Context, command string) string {
	if !shellMayMutate(command) {
		return ""
	}
	if judge := SerialJudgeFrom(ctx); judge != nil && !judge(ctx, "run_shell", command) {
		return "" // the judge says read-only: let it overlap
	}
	return GlobalKey
}

// shellMayMutate is the keyword screen. It splits the command into words on
// whitespace and shell metacharacters, treats a path's basename as the word
// (`/bin/rm` is rm), and flags output redirection anywhere in the line.
// Quoted text is not parsed: a keyword inside a quoted string still hits,
// which is why a hit is only a suspicion.
func shellMayMutate(command string) bool {
	// A `>` writes a file (> , >> , 2> , &>); 2>&1 is only a stream dup.
	if strings.Contains(strings.ReplaceAll(command, "2>&1", ""), ">") {
		return true
	}
	words := shellWords(command)
	builds := false
	for i, w := range words {
		if _, ok := shellAlwaysMutate[w]; ok {
			return true
		}
		if _, ok := shellOutputFlags[w]; ok {
			return true
		}
		if _, ok := shellBuildWords[w]; ok {
			builds = true
		}
		// `-o` writes an artifact only for a compiler; `grep -o`/`ls -o` read.
		if w == "-o" && builds {
			return true
		}
		if subs, ok := shellReadOnlySub[w]; ok {
			// The next word decides: an unknown or absent subcommand is a
			// suspicion (`go install`, bare `npm`), a listed one is read-only.
			if i+1 >= len(words) {
				return true
			}
			if _, ro := subs[words[i+1]]; !ro {
				return true
			}
		}
	}
	return false
}

// shellWords lowercases and splits a command into path-basename words,
// dropping quotes and splitting on the shell's control characters.
func shellWords(command string) []string {
	fields := strings.FieldsFunc(strings.ToLower(command), func(r rune) bool {
		switch r {
		case ';', '|', '&', '(', ')', '\n', '\r', '\t', ' ', '"', '\'', '`', '=', ',':
			return true
		}
		return false
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		out = append(out, strings.TrimSuffix(filepath.Base(f), ".exe"))
	}
	return out
}

// SerialKey implements SerialScoped: the command's own keywords decide
// whether it needs the exclusive lock, so read-only commands overlap and
// only a suspicious one asks the judge.
func (s *Shell) SerialKey(ctx context.Context, input json.RawMessage) string {
	var in shellInput
	if err := json.Unmarshal(input, &in); err != nil {
		return GlobalKey // unparseable: run alone rather than risk a race
	}
	return shellSerialKey(ctx, in.Command)
}
