package timer

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Log line format: "2006-01-02 15:04:05.000 [INFO] message".
// File: $LYON_HOME/logs/timer-YYYY-MM-DD.log, append-only, one mutex
// for the process. This is a minimal stopgap until pkg/logx lands —
// every write funnels through logLine, so swapping in logx later is a
// one-function change and no call site moves.

var logMu sync.Mutex

// logLine appends one leveled line to today's timer log file. Logging
// failures are silent by design: a broken log file must never take a
// timer down.
func logLine(level, format string, args ...any) {
	root, err := homeDir()
	if err != nil {
		return
	}
	dir := filepath.Join(root, "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	path := filepath.Join(dir, "timer-"+time.Now().Format("2006-01-02")+".log")
	line := time.Now().Format("2006-01-02 15:04:05.000") + " [" + level + "] " + fmt.Sprintf(format, args...) + "\n"
	logMu.Lock()
	defer logMu.Unlock()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(line)
}

// LogInfo / LogWarn / LogError are the package's logging surface.
func LogInfo(format string, args ...any)  { logLine("INFO", format, args...) }
func LogWarn(format string, args ...any)  { logLine("WARN", format, args...) }
func LogError(format string, args ...any) { logLine("ERROR", format, args...) }
