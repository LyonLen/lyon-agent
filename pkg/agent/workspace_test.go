package agent

import (
	"path/filepath"
	"testing"
)

// TestSessionWorkspaceOverride: a session can retarget its workspace
// (per-session workspace selection) — tools and surfaces follow
// s.Workspace(); bad roots are rejected and "" clears the override.
func TestSessionWorkspaceOverride(t *testing.T) {
	def := t.TempDir()
	other := t.TempDir()
	eng := NewEngine(Config{Workspace: def}, nil)
	t.Cleanup(eng.Close)
	s := eng.NewSession(nil)

	defAbs, _ := filepath.Abs(def)
	if s.Workspace() != defAbs && s.Workspace() != def {
		t.Fatalf("initial workspace = %q, want %q", s.Workspace(), def)
	}

	if err := s.SetWorkspace(other); err != nil {
		t.Fatalf("SetWorkspace: %v", err)
	}
	otherAbs, _ := filepath.Abs(other)
	if s.Workspace() != otherAbs {
		t.Fatalf("workspace = %q, want %q", s.Workspace(), otherAbs)
	}

	if err := s.SetWorkspace(filepath.Join(def, "no-such-dir")); err == nil {
		t.Fatal("missing root must be rejected")
	}
	if s.Workspace() != otherAbs {
		t.Fatalf("rejected write changed workspace to %q", s.Workspace())
	}

	if err := s.SetWorkspace(""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if s.Workspace() != defAbs && s.Workspace() != def {
		t.Fatalf("cleared workspace = %q, want %q", s.Workspace(), def)
	}
}
