//go:build windows

package timer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSchtasksCommandTriggerIntegration proves the defect-A fix
// end to end on a real Windows Task Scheduler: it registers a ONCE
// command task through the Backend and waits for its `cmd /C`
// action to produce a probe file. The old `<lyon.exe> /C ...` action
// registered fine but could never fire correctly, so no probe file
// would ever appear. The probe directory contains a space on purpose
// to stress the /C quoting.
//
// Skipped unless LYON_TIMER_ITEST=2 — it waits for a real scheduler
// tick (up to two minutes).
func TestSchtasksCommandTriggerIntegration(t *testing.T) {
	if getenv("LYON_TIMER_ITEST") != "2" {
		t.Skip("set LYON_TIMER_ITEST=2 to run the real trigger test (waits ~1-2 min)")
	}
	home := scratchHome(t)
	probeDir := filepath.Join(home, "probe dir")
	if err := os.MkdirAll(probeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(probeDir, "fired.txt")
	at := time.Now().Add(2 * time.Minute).Truncate(time.Minute)
	tk := &Task{ID: "trigger-probe", Kind: KindOnce, At: &at,
		Command: `echo fired> "` + probe + `"`, Enabled: true}
	b := NewBackend()
	if err := b.Register(tk); err != nil {
		t.Fatalf("Register: %v", err)
	}
	defer func() {
		if err := b.Unregister(tk.ID); err != nil {
			t.Errorf("Unregister: %v", err)
		}
	}()
	deadline := time.Now().Add(150 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(probe); err == nil {
			if !strings.Contains(string(data), "fired") {
				t.Fatalf("probe content wrong: %q", data)
			}
			t.Logf("probe fired at %s (registered for %s)", time.Now().Format(time.RFC3339), at.Format(time.RFC3339))
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("probe file never appeared — the registered cmd /C action did not fire by %s", at.Format(time.RFC3339))
}
