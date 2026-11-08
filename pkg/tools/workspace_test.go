package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWorkspaceRootFromCtx: no resolver → the configured fallback; a
// resolver wins; an empty answer falls back.
func TestWorkspaceRootFromCtx(t *testing.T) {
	if got := WorkspaceRoot(context.Background(), "fallback"); got != "fallback" {
		t.Fatalf("no resolver = %q, want fallback", got)
	}
	ctx := WithWorkspaceFunc(context.Background(), func() string { return "live-root" })
	if got := WorkspaceRoot(ctx, "fallback"); got != "live-root" {
		t.Fatalf("resolver = %q, want live-root", got)
	}
	empty := WithWorkspaceFunc(context.Background(), func() string { return "" })
	if got := WorkspaceRoot(empty, "fallback"); got != "fallback" {
		t.Fatalf("empty resolver = %q, want fallback", got)
	}
}

// TestReadFileUsesLiveWorkspace: the per-session workspace seam — a tool
// registered with one root executes under the ctx resolver's root, and
// without a resolver the configured root still confines.
func TestReadFileUsesLiveWorkspace(t *testing.T) {
	def := t.TempDir()
	live := t.TempDir()
	if err := os.WriteFile(filepath.Join(live, "only-in-live.txt"), []byte("LIVE"), 0o644); err != nil {
		t.Fatal(err)
	}
	rf := &ReadFile{Root: def}
	ctx := WithWorkspaceFunc(context.Background(), func() string { return live })
	out, err := rf.Execute(ctx, json.RawMessage(`{"path":"only-in-live.txt"}`))
	if err != nil {
		t.Fatalf("live workspace read: %v", err)
	}
	if !strings.Contains(out[0].OfText.Text, "LIVE") {
		t.Fatalf("read = %q, want LIVE", out[0].OfText.Text)
	}
	// No resolver: the same path is outside the configured root.
	if _, err := rf.Execute(context.Background(), json.RawMessage(`{"path":"only-in-live.txt"}`)); err == nil {
		t.Fatal("configured root must still confine without a resolver")
	}
}
