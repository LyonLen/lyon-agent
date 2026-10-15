package logx

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// useTempHome redirects LYON_HOME to a fresh temp dir and resets logger
// state. The returned root is where $LYON_HOME/logs must land. No file
// handle survives a write, so t.TempDir's cleanup can always remove the
// tree on Windows without any Close ordering games.
func useTempHome(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("LYON_HOME", root)
	reset()
	t.Cleanup(Close)
	return root
}

// reset clears all package state between tests (same-package access).
func reset() {
	mu.Lock()
	defer mu.Unlock()
	pruned.dir, pruned.day = "", ""
	noticed = false
	minLevel = LevelInfo
}

// todayPath is where this instant's log line must land.
func todayPath(root string) string {
	return filepath.Join(root, "logs", prefix+time.Now().Format(dayLayout)+suffix)
}

func TestBasicWriteAndFormat(t *testing.T) {
	root := useTempHome(t)

	Info("engine", "warmup failed", "err", "connection reset", "attempt", 2)

	Close() // lifecycle no-op; every line is already on disk
	data, err := os.ReadFile(todayPath(root))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d: %q", len(lines), data)
	}
	// TS info engine warmup failed err="connection reset" attempt=2
	re := regexp.MustCompile(`^(\S+) info engine warmup failed err="connection reset" attempt=2$`)
	m := re.FindStringSubmatch(lines[0])
	if m == nil {
		t.Fatalf("line does not match format: %q", lines[0])
	}
	if _, err := time.Parse(lineLayout, m[1]); err != nil {
		t.Fatalf("timestamp %q not parseable as %s: %v", m[1], lineLayout, err)
	}

	// Multi-line error strings must stay one line.
	Error("tools", "boom\nsecond line", "detail", "a\tb")
	Close()
	data, _ = os.ReadFile(todayPath(root))
	lines = strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines after multi-line msg, got %d: %q", len(lines), data)
	}
	if !strings.Contains(lines[1], `boom\nsecond line`) || !strings.Contains(lines[1], `detail="a\tb"`) {
		t.Fatalf("newlines not escaped: %q", lines[1])
	}
}

func TestConcurrentGoroutines(t *testing.T) {
	root := useTempHome(t)

	const goroutines, per = 8, 100
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				Info("conc", "line", "g", g, "i", i)
			}
		}(g)
	}
	wg.Wait()
	Close()

	data, err := os.ReadFile(todayPath(root))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	want := goroutines * per
	if len(lines) != want {
		t.Fatalf("want %d lines, got %d", want, len(lines))
	}
	re := regexp.MustCompile(`^\S+ info conc line g=(\d+) i=(\d+)$`)
	seen := make(map[[2]int]int)
	for _, ln := range lines {
		m := re.FindStringSubmatch(ln)
		if m == nil {
			t.Fatalf("torn or malformed line: %q", ln)
		}
		var key [2]int
		fmt.Sscanf(m[1], "%d", &key[0])
		fmt.Sscanf(m[2], "%d", &key[1])
		seen[key]++
	}
	if len(seen) != want {
		t.Fatalf("want %d distinct (g,i) pairs, got %d", want, len(seen))
	}
	for key, n := range seen {
		if n != 1 {
			t.Fatalf("pair %v logged %d times", key, n)
		}
	}
}

func TestDailyFileAndPrune(t *testing.T) {
	root := useTempHome(t)
	dir := filepath.Join(root, "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	expired := filepath.Join(dir, "lyon-2020-01-01.log")
	foreign := filepath.Join(dir, "timer-2020-01-01.log") // sibling log — untouchable
	notDated := filepath.Join(dir, "lyon-backup.log")     // prefix but no date
	recent := filepath.Join(dir, prefix+now.AddDate(0, 0, -1).Format(dayLayout)+suffix)
	for _, p := range []string{expired, foreign, notDated, recent} {
		if err := os.WriteFile(p, []byte("keep-or-prune\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	Info("test", "rotation trigger")
	Close()

	if _, err := os.Stat(expired); !os.IsNotExist(err) {
		t.Errorf("expired file not pruned (stat err=%v)", err)
	}
	for _, p := range []string{foreign, notDated, recent} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s must survive pruning: %v", filepath.Base(p), err)
		}
	}
	if _, err := os.Stat(todayPath(root)); err != nil {
		t.Errorf("today's file missing: %v", err)
	}
}

func TestWriteFailureDegrades(t *testing.T) {
	root := useTempHome(t)
	// A file where the logs directory should be: MkdirAll fails forever.
	if err := os.WriteFile(filepath.Join(root, "logs"), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}

	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	defer func() { os.Stderr = old }()

	func() {
		defer func() {
			if rec := recover(); rec != nil {
				t.Fatalf("log call panicked: %v", rec)
			}
		}()
		for i := 0; i < 5; i++ {
			Debug("c", "d")
			Info("c", "i")
			Warn("c", "w")
			Error("c", "e", "err", fmt.Errorf("boom"))
		}
		Flush()
		Close() // must also be safe while broken
	}()

	w.Close()
	os.Stderr = old
	out, _ := io.ReadAll(r)
	if n := strings.Count(string(out), "logx:"); n > 1 {
		t.Fatalf("want at most one stderr notice, got %d: %s", n, out)
	}
}

func TestLYONHomeRedirect(t *testing.T) {
	root := useTempHome(t)

	Error("api", "boom", "status", 500)
	Close()

	entries, err := os.ReadDir(filepath.Join(root, "logs"))
	if err != nil {
		t.Fatalf("logs dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want exactly one log file, got %d", len(entries))
	}
	name := entries[0].Name()
	if !regexp.MustCompile(`^lyon-\d{4}-\d{2}-\d{2}\.log$`).MatchString(name) {
		t.Fatalf("unexpected file name %q", name)
	}
}

func TestLevelFilter(t *testing.T) {
	root := useTempHome(t)

	SetLevel(LevelWarn)
	Debug("c", "no")
	Info("c", "no")
	Warn("c", "yes")
	Error("c", "yes")

	// Raising the threshold mid-run also works on the open handle.
	SetLevel(LevelError)
	Warn("c", "no after raise")
	Error("c", "yes after raise")
	Close()

	data, err := os.ReadFile(todayPath(root))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 lines above threshold, got %d: %q", len(lines), data)
	}
	if !regexp.MustCompile(`^\S+ warn c yes$`).MatchString(lines[0]) {
		t.Fatalf("unexpected first line: %q", lines[0])
	}
	if !regexp.MustCompile(`^\S+ error c yes$`).MatchString(lines[1]) {
		t.Fatalf("unexpected second line: %q", lines[1])
	}
	if !regexp.MustCompile(`^\S+ error c yes after raise$`).MatchString(lines[2]) {
		t.Fatalf("unexpected third line: %q", lines[2])
	}
}

// TestNoHandleHeldBetweenWrites is the regression guard for the design
// rule: no file handle may stay open between writes. On Windows an open
// file cannot be deleted, and a held daily-file handle broke per-test
// LYON_HOME temp-dir cleanup (surfaced as cmd/lyon TestMemoryTablePlain
// "file is being used by another process"). Removing the live log file
// mid-process must therefore always succeed.
func TestNoHandleHeldBetweenWrites(t *testing.T) {
	root := useTempHome(t)
	path := todayPath(root)

	Info("c", "line")
	if err := os.Remove(path); err != nil {
		t.Fatalf("log file still locked after write (handle leaked?): %v", err)
	}

	// The logger self-heals: the next write recreates the file.
	Info("c", "line again")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("log not recreated after removal: %v", err)
	}
}
