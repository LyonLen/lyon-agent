// Package logx is lyon's structured run log — the durable trace for
// execution errors that the audit JSONL was never meant to carry. Every
// line is one event; the file rotates daily; logging can never take the
// host down.
//
// # Where
//
// $LYON_HOME/logs/lyon-YYYY-MM-DD.log (default ~/.lyon/logs/). The first
// write per (directory, day) prunes lyon-*.log files older than
// RetentionDays (30), best-effort. Only the lyon- prefix is touched:
// sibling logs (timer-*.log) are never pruned by this package.
//
// # Line format
//
//	2026-10-15T14:23:45.678+08:00 error tools tool execution failed tool=write_file duration_ms=12 err="exit status 1"
//
//	timestamp(RFC3339, millis) level component msg key=value ...
//
// msg is a free-text sentence (whitespace-collapsed, one line); kv pairs
// are greppable. A value containing whitespace/quotes/control bytes is
// strconv.Quote'd; errors are stringified. An odd trailing key logs as
// key=!MISSING.
//
// # How to use
//
//	logx.Error("tools", "tool execution failed", "tool", name, "err", err.Error())
//	logx.Warn("apiguard", "transport error", "url", u, "err", err)
//	logx.Debug("engine", "warm cache hit")
//
// Levels: debug < info < warn < error, default info; LYON_DEBUG enables
// debug at init, SetLevel overrides at runtime. Debug calls below the
// threshold return before touching the disk, so debug sprinkles are free
// in production.
//
// Wiring an error path: log at the seam where the error is final (the
// point that returns it to the caller or swallows it), include enough
// key=value context to identify the subject, and never let a logx call
// sit on a hot success path.
//
// # Concurrency model
//
// One package-level logger guarded by a mutex. Every log line is
// open(O_APPEND) + one Write + close, executed under that mutex:
//
//   - goroutine-safe: one line is one Write, so two goroutines can never
//     interleave inside a line;
//   - multi-process-safe: O_APPEND makes every write go to the current
//     EOF (POSIX atomic append; Windows honors it via FILE_APPEND_DATA),
//     so N lyon processes sharing one $LYON_HOME append without
//     corruption (the same trick pkg/hooks uses for the audit JSONL);
//   - no file handle is ever held between writes. That is deliberate:
//     on Windows a file opened by a process cannot be deleted, so a held
//     handle would block day-old file removal AND break any per-test
//     LYON_HOME temp-dir cleanup the moment a test that logged anything
//     ended. One extra open/close per line is cheap for an error-path
//     log and buys crash-durability: when the call returns, the line is
//     already in the OS file — there is no buffered handle to lose.
//
// Day rollover is detected per write; expiry pruning runs once per
// (directory, day), and a day-old file another process still holds open
// survives pruning (Windows) and is retried on the next day boundary.
//
// # Failure policy
//
// A broken log file must never break the agent. Every failure degrades:
// at most ONE stderr notice is printed per process ("logx: ..." once;
// later failures fully silent), and the next log call retries the open,
// so a transient error self-heals.
//
// # Close/Flush lifecycle
//
// Nothing is buffered and no handle is held, so Flush and Close are
// no-ops kept for lifecycle symmetry — Engine.Close calls Close, and
// several Engines in one process (tests) may all call it safely.
//
// # Suspicious concurrent-write audit (repo scan 2026-10-15)
//
// Direct overwrites (tear risk, no tmp+rename):
//
//   - pkg/eval/exam.go SaveExam — os.WriteFile straight onto the shared
//     exam file. Fix: os.CreateTemp + rename, like hooks.writeStatsFile.
//   - pkg/flow/builtin.go Save — same. Fix: same.
//   - pkg/tools/files.go WriteFile/EditFile — workspace file writes,
//     serialized only per-session via SerialMu; a subagent or second
//     session editing the same path can tear. Errors are now logged by
//     the tools adapter seam; fix if needed: write tmp + rename.
//
// Fixed tmp name (in-process safe under a mutex, but cross-process the
// last writer wins and the shared ".tmp" can be renamed while another
// process is mid-write):
//
//   - pkg/agent/snapshot.go SaveSnapshot + writeArchive
//   - pkg/routing/routing.go saveLocked, pkg/market/market.go,
//     pkg/eval/leaderboard.go saveLocked, pkg/eval/candidates.go,
//     pkg/memory/store.go, pkg/flow/streak.go, pkg/timer/timer.go Save
//
// Fix for all of the above: os.CreateTemp(dir, name+"-*.tmp") — unique
// temp names make the race harmless (this is what hooks.writeStatsFile
// already does, with a comment).
//
// Minor: pkg/tools/paste.go uniquePath checks existence then writes
// (TOCTOU window; mitigated by the rotate-after path).
//
// Audited safe: pkg/agent/dispatch.go fan-out writes results[i] by
// disjoint index.
package logx

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Level is the severity of one log line.
type Level int

// Severity levels, ordered.
const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// String renders the level as it appears in a log line.
func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "debug"
	case LevelInfo:
		return "info"
	case LevelWarn:
		return "warn"
	case LevelError:
		return "error"
	}
	return "unknown"
}

const (
	// RetentionDays bounds the daily files: dated lyon-*.log older than
	// this many days is removed at the next day-boundary open (best-effort).
	RetentionDays = 30

	prefix     = "lyon-"
	suffix     = ".log"
	dayLayout  = "2006-01-02"
	lineLayout = "2006-01-02T15:04:05.000Z07:00"
)

var (
	mu     sync.Mutex
	pruned struct { // last (directory, day) the prune pass ran for
		dir string
		day string
	}
	minLevel = LevelInfo
	noticed  bool // at most one stderr notice about logx's own failures
)

func init() {
	if os.Getenv("LYON_DEBUG") != "" {
		minLevel = LevelDebug
	}
}

// SetLevel raises or lowers the threshold for what gets written.
func SetLevel(l Level) {
	mu.Lock()
	minLevel = l
	mu.Unlock()
}

// Debug logs at debug level — free when the threshold is above it.
func Debug(component, msg string, kv ...any) { log(LevelDebug, component, msg, kv) }

// Info logs at info level (the default).
func Info(component, msg string, kv ...any) { log(LevelInfo, component, msg, kv) }

// Warn logs at warn level.
func Warn(component, msg string, kv ...any) { log(LevelWarn, component, msg, kv) }

// Error logs at error level.
func Error(component, msg string, kv ...any) { log(LevelError, component, msg, kv) }

// log is the single write seam. It never panics and never returns an
// error: a broken log must not break the caller.
func log(level Level, component, msg string, kv []any) {
	mu.Lock()
	defer mu.Unlock()
	if level < minLevel {
		return
	}
	now := time.Now()
	today := now.Format(dayLayout)
	root, err := homeDir()
	if err != nil {
		notice("logx: cannot resolve log home: " + err.Error())
		return
	}
	dir := filepath.Join(root, "logs")
	if pruned.dir != dir || pruned.day != today {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			notice(fmt.Sprintf("logx: cannot create %s: %v", dir, err))
			return
		}
		prune(dir, now)
		pruned.dir, pruned.day = dir, today
	}
	path := filepath.Join(dir, prefix+today+suffix)
	line := formatLine(now, level, component, msg, kv)
	// Open-per-write, never a held handle: see the concurrency model in
	// the package comment. O_APPEND + one Write = one intact line.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		notice(fmt.Sprintf("logx: cannot open %s: %v", path, err))
		return
	}
	_, werr := f.WriteString(line)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		notice(fmt.Sprintf("logx: writing %s failed: %v", path, werr))
	}
}

// formatLine renders one record ending in "\n". msg is collapsed to a
// single line so one event can never tear the file into two lines.
func formatLine(now time.Time, level Level, component, msg string, kv []any) string {
	var b strings.Builder
	b.WriteString(now.Format(lineLayout))
	b.WriteByte(' ')
	b.WriteString(level.String())
	b.WriteByte(' ')
	b.WriteString(component)
	b.WriteByte(' ')
	b.WriteString(oneLine(msg))
	for i := 0; i < len(kv); i += 2 {
		k := fmt.Sprint(kv[i])
		var v string
		if i+1 >= len(kv) {
			v = "!MISSING"
		} else {
			v = formatVal(kv[i+1])
		}
		b.WriteByte(' ')
		b.WriteString(oneLine(k))
		b.WriteByte('=')
		b.WriteString(v)
	}
	b.WriteByte('\n')
	return b.String()
}

// oneLine flattens embedded newlines so a multi-line error string stays
// one greppable log line.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", `\r`)
	return strings.ReplaceAll(s, "\n", `\n`)
}

// formatVal renders a kv value: strings with whitespace/quotes/control
// bytes (or empty) get strconv.Quote so the pair stays machine-splittable;
// errors stringify through the same path.
func formatVal(v any) string {
	if err, ok := v.(error); ok {
		v = err.Error()
	}
	if s, ok := v.(string); ok {
		if s == "" || strings.ContainsAny(s, " \t\r\n\"=") {
			return strconv.Quote(s)
		}
		return oneLine(s)
	}
	return fmt.Sprint(v)
}

// prune removes dated lyon-*.log older than RetentionDays. Best-effort:
// a file another process still holds open survives (Windows refuses to
// delete open files) and is retried at the next day boundary.
func prune(dir string, now time.Time) {
	matches, err := filepath.Glob(filepath.Join(dir, prefix+"*"+suffix))
	if err != nil {
		return
	}
	cutoff := now.AddDate(0, 0, -RetentionDays).Format(dayLayout)
	for _, p := range matches {
		d := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), prefix), suffix)
		if len(d) != len(dayLayout) {
			continue // not a dated file; never touch foreign names
		}
		if _, err := time.Parse(dayLayout, d); err != nil {
			continue
		}
		if d < cutoff { // yyyy-mm-dd compares correctly as a string
			_ = os.Remove(p)
		}
	}
}

// notice prints at most one stderr line about logx's own failures per
// process — visibility without log spam, per the degrade contract.
func notice(msg string) {
	if noticed {
		return
	}
	noticed = true
	fmt.Fprintln(os.Stderr, msg+" (further logx failures silent)")
}

// Flush is a no-op kept for lifecycle symmetry: every line is written
// directly (one unbuffered Write per line), so nothing is ever buffered
// and there is nothing to flush.
func Flush() {}

// Close is a no-op kept for the Engine lifecycle: no file handle is held
// between writes, so there is nothing to flush or release. Idempotent,
// and safe even when several Engines (tests) share one process.
func Close() {}

// homeDir resolves the store root: $LYON_HOME when set (containers, CI,
// tests), else ~/.lyon — the same convention as hooks/timer/agent.
func homeDir() (string, error) {
	if h := os.Getenv("LYON_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".lyon"), nil
}
