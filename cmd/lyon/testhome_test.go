package main

import (
	"os"
	"testing"
)

// TestMain points LYON_HOME at a scratch dir when unset so engine-based
// tests never write audit/stats into the developer's real ~/.lyon. The
// real-snapshot geometry test reads ~/.lyon via os.UserHomeDir directly
// and is unaffected.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "lyon-cmd-test-home")
	if err == nil && os.Getenv("LYON_HOME") == "" {
		os.Setenv("LYON_HOME", dir)
	}
	// The planner (pkg/agent/planner.go) would consume a scripted mock
	// turn on any gate-passing prompt; tests opt in per-case.
	os.Setenv("LYON_PLANNER", "off")
	code := m.Run()
	if dir != "" {
		os.RemoveAll(dir)
	}
	os.Exit(code)
}
