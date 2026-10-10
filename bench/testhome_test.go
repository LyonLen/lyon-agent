package bench

import (
	"os"
	"testing"
)

// TestMain points LYON_HOME at a scratch dir when unset: the hooks layer
// writes audit/stats under it, and the load benchmarks would otherwise
// hammer (and pollute) the developer's real ~/.lyon — skewing both the
// numbers and the user's counters.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "lyon-bench-home")
	if err == nil && os.Getenv("LYON_HOME") == "" {
		os.Setenv("LYON_HOME", dir)
	}
	code := m.Run()
	if dir != "" {
		os.RemoveAll(dir)
	}
	os.Exit(code)
}
