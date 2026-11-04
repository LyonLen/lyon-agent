package agent

import (
	"os"
	"testing"
)

// TestMain points LYON_HOME at a scratch dir when unset: the hooks layer
// writes audit/stats under it, and defaulting to the real ~/.lyon would
// let test runs inflate the developer's counters. Tests that care set
// their own LYON_HOME with t.Setenv on top of this.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "lyon-agent-test-home")
	if err == nil && os.Getenv("LYON_HOME") == "" {
		os.Setenv("LYON_HOME", dir)
	}
	// The planner (planner.go) would consume a scripted mock turn on any
	// gate-passing prompt; tests opt in per-case with t.Setenv.
	os.Setenv("LYON_PLANNER", "off")
	code := m.Run()
	if dir != "" {
		os.RemoveAll(dir)
	}
	os.Exit(code)
}
