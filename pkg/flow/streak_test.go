package flow

import (
	"os"
	"path/filepath"
	"testing"
)

// TestBumpStreak: the auto-freeze clock counts consecutive greens per
// slug, resets on a red run, and survives a reopen (persisted state).
func TestBumpStreak(t *testing.T) {
	home := t.TempDir()
	t.Setenv("LYON_HOME", home)

	if n := BumpStreak("bugfix", true); n != 1 {
		t.Fatalf("first green = %d, want 1", n)
	}
	if n := BumpStreak("bugfix", true); n != 2 {
		t.Fatalf("second green = %d, want 2", n)
	}
	// A different slug's clock is independent.
	if n := BumpStreak("release", true); n != 1 {
		t.Fatalf("release green = %d, want 1", n)
	}
	// A red run resets only its own slug.
	if n := BumpStreak("bugfix", false); n != 0 {
		t.Fatalf("red resets to %d, want 0", n)
	}
	if n := BumpStreak("bugfix", true); n != 1 {
		t.Fatalf("green after reset = %d, want 1", n)
	}

	// The file is real JSON on disk at the expected path.
	if _, err := os.Stat(filepath.Join(home, "flow-streak.json")); err != nil {
		t.Fatalf("streak file missing: %v", err)
	}
	// Every bump stages through a unique CreateTemp name that is renamed
	// away — consecutive bumps must leave no *.tmp droppings behind.
	residue, err := filepath.Glob(filepath.Join(home, "*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(residue) != 0 {
		t.Errorf("temp residue after bumps: %v", residue)
	}
}
