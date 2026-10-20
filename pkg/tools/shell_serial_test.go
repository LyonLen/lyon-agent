package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestShellSerialKeyReadOnly: a command that mentions no mutating word takes
// no lock, so shells overlap by default (#33).
func TestShellSerialKeyReadOnly(t *testing.T) {
	bg := context.Background()
	for _, cmd := range []string{
		"ls -la",
		"cat a.txt",
		"git status",
		"git log --oneline -5",
		"git diff HEAD~1",
		"go test ./...",
		"go vet ./...",
		"npm ls",
		"echo hi 2>&1",
		"grep -o 'foo' a.txt",
		"find . -name '*.go'",
		"wc -l src/main.go",
	} {
		if k := shellSerialKey(bg, cmd); k != "" {
			t.Errorf("%q classified as %q, want read-only", cmd, k)
		}
	}
}

// TestShellSerialKeyMutating: a suspicious command with no judge wired fails
// closed to the exclusive class — probes, flows and tests keep the old safe
// behavior.
func TestShellSerialKeyMutating(t *testing.T) {
	bg := context.Background()
	for _, cmd := range []string{
		"rm -rf build",
		"mv a.txt b.txt",
		"sed -i 's/x/y/' a.txt",
		"git checkout .",
		"git reset --hard",
		"git commit -m x",
		"npm install",
		"go get ./...",
		"go build -o bin/x ./cmd/x",
		"echo hi > out.txt",
		"make",
		"python gen.py",
		"find . -name '*.tmp' -delete",
	} {
		if k := shellSerialKey(bg, cmd); k != GlobalKey {
			t.Errorf("%q classified as %q, want %q", cmd, k, GlobalKey)
		}
	}
}

// TestShellSerialKeyJudgeEscalation: only the suspicious command reaches the
// judge (a token cost); the read-only one never calls it.
func TestShellSerialKeyJudgeEscalation(t *testing.T) {
	var calls int
	judge := func(context.Context, string, string) bool { calls++; return false }
	ctx := WithSerialJudge(context.Background(), judge)

	if k := shellSerialKey(ctx, "ls -la"); k != "" {
		t.Fatalf("read-only command = %q, want no lock", k)
	}
	if calls != 0 {
		t.Fatalf("read-only command cost %d judge calls, want 0", calls)
	}
	// `git status` mentions git but is read-only: the screen never asks.
	if k := shellSerialKey(ctx, "git status"); k != "" {
		t.Fatalf("git status = %q, want no lock", k)
	}
	if calls != 0 {
		t.Fatalf("git status cost %d judge calls, want 0", calls)
	}
	// A hit is escalated; the judge says read-only, so it overlaps.
	if k := shellSerialKey(ctx, `grep "rm " notes.md`); k != "" {
		t.Fatalf("judged read-only = %q, want no lock", k)
	}
	if calls != 1 {
		t.Fatalf("judge calls = %d, want 1", calls)
	}
	// A judge that says serial keeps the exclusive class.
	serialJudge := func(context.Context, string, string) bool { return true }
	if k := shellSerialKey(WithSerialJudge(context.Background(), serialJudge), "rm -rf x"); k != GlobalKey {
		t.Fatalf("judged serial = %q, want %q", k, GlobalKey)
	}
}

// TestShellSerialKeyUnparseable: bad input must not buy concurrency.
func TestShellSerialKeyUnparseable(t *testing.T) {
	sh := &Shell{}
	if k := sh.SerialKey(context.Background(), json.RawMessage(`not json`)); k != GlobalKey {
		t.Fatalf("unparseable input = %q, want %q", k, GlobalKey)
	}
}

// shellLikeTool is a Shell stand-in that classifies its command exactly like
// the real tool without spawning a process, so the lock behavior is testable.
type shellLikeTool struct {
	funcTool
	cmd string
}

func (t *shellLikeTool) SerialKey(ctx context.Context, _ json.RawMessage) string {
	return shellSerialKey(ctx, t.cmd)
}

// TestShellConcurrencyEndToEnd: through the adapter, a read-only shell
// overlaps another execution while a mutating one takes the exclusive class
// and blocks a file holder.
func TestShellConcurrencyEndToEnd(t *testing.T) {
	probe := func(p *overlapProbe) func(context.Context) error { return p.run }

	// Read-only shell vs a mismatched file edit: they must overlap.
	read := &overlapProbe{holdTime: 60 * time.Millisecond}
	var ser Serializer
	runPair(
		Adapt(&shellLikeTool{funcTool: funcTool{name: "run_shell", fn: probe(read)}, cmd: "ls -la"}, AdapterConfig{Serial: &ser}),
		Adapt(&scopedTool{funcTool: funcTool{name: "edit_file", fn: probe(read)}, key: "file:x"}, AdapterConfig{Serial: &ser}),
	)
	if read.peak() < 2 {
		t.Fatalf("read-only shell did not overlap a file edit: peak=%d", read.peak())
	}

	// A mutating shell takes the exclusive class: no overlap.
	mut := &overlapProbe{holdTime: 60 * time.Millisecond}
	runPair(
		Adapt(&shellLikeTool{funcTool: funcTool{name: "run_shell", fn: probe(mut)}, cmd: "rm -rf build"}, AdapterConfig{Serial: &ser}),
		Adapt(&scopedTool{funcTool: funcTool{name: "edit_file", fn: probe(mut)}, key: "file:x"}, AdapterConfig{Serial: &ser}),
	)
	if mut.peak() > 1 {
		t.Fatalf("mutating shell overlapped a file edit: peak=%d", mut.peak())
	}
}
