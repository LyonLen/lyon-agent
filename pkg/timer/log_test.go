package timer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLogLinesLandInDatedFile(t *testing.T) {
	home := scratchHome(t)
	LogInfo("hello %d", 42)
	LogWarn("careful")
	LogError("boom %s", "x")
	log := readTodayLog(t, home)
	for _, want := range []string{"[INFO] hello 42", "[WARN] careful", "[ERROR] boom x"} {
		if !strings.Contains(log, want) {
			t.Fatalf("log missing %q:\n%s", want, log)
		}
	}
	// File name carries today's date, one dir under the store root.
	name := "timer-" + time.Now().Format("2006-01-02") + ".log"
	if _, err := os.Stat(filepath.Join(home, "logs", name)); err != nil {
		t.Fatalf("dated log file: %v", err)
	}
}

func TestLogAppendsAcrossCalls(t *testing.T) {
	home := scratchHome(t)
	LogInfo("one")
	LogInfo("two")
	log := readTodayLog(t, home)
	if strings.Index(log, "one") > strings.Index(log, "two") {
		t.Fatalf("lines out of order:\n%s", log)
	}
}
