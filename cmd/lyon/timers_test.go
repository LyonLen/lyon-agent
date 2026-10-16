package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lyon-agent/pkg/timer"
)

// captureOut runs f with os.Stdout swapped for a pipe and returns what
// it printed — for asserting CLI table output without a golden file.
// Stderr goes to devnull: usage messages are content, not test noise.
func captureOut(t *testing.T, f func()) string {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = w, devnull
	defer func() { os.Stdout, os.Stderr = oldOut, oldErr }()
	f()
	w.Close()
	devnull.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return stripANSI(string(out))
}

// loadStored pulls a task straight from the scratch store (LYON_HOME is
// always t.Setenv'd by these tests — the real ~/.lyon is never touched).
func loadStored(t *testing.T, id string) *timer.Task {
	t.Helper()
	task, err := timer.Load(id)
	if err != nil {
		t.Fatalf("load stored timer %s: %v", id, err)
	}
	return task
}

// TestParseTimerAdd: the shared add-line grammar — one parse powers
// /timer add and `lyon timer add`, so every accepted and rejected shape
// is pinned here.
func TestParseTimerAdd(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.Local)

	ok := []struct {
		name, line string
		check      func(t *testing.T, task *timer.Task)
	}{
		{"every command", "beat every 5m command echo hi", func(t *testing.T, task *timer.Task) {
			if task.Kind != timer.KindInterval || task.EveryMS != 5*60*1000 {
				t.Fatalf("kind/every = %s/%d", task.Kind, task.EveryMS)
			}
			if task.Command != "echo hi" || task.Prompt != "" {
				t.Fatalf("payload = cmd %q / prompt %q", task.Command, task.Prompt)
			}
			if !task.Enabled {
				t.Fatal("added task must start enabled")
			}
		}},
		{"cron prompt with workspace default", "standup cron 0 9 * * 1-5 prompt draft the standup", func(t *testing.T, task *timer.Task) {
			if task.Cron != "0 9 * * 1-5" {
				t.Fatalf("cron = %q", task.Cron)
			}
			if task.Prompt != "draft the standup" {
				t.Fatalf("prompt = %q", task.Prompt)
			}
			if task.Workspace != "D:\\ws" {
				t.Fatalf("workspace = %q, want the caller's default", task.Workspace)
			}
		}},
		{"once relative", "remind once +30m prompt stretch", func(t *testing.T, task *timer.Task) {
			want := now.Add(30 * time.Minute)
			if task.At == nil || !task.At.Equal(want) {
				t.Fatalf("at = %v, want %v", task.At, want)
			}
		}},
		{"once rfc3339 zone", "meet once 2026-10-16T09:00:00+08:00 prompt go", func(t *testing.T, task *timer.Task) {
			if task.At == nil || task.At.Format(time.RFC3339) != "2026-10-16T09:00:00+08:00" {
				t.Fatalf("at = %v", task.At)
			}
		}},
		{"once local naive", "meet once 2026-10-16T09:00:00 prompt go", func(t *testing.T, task *timer.Task) {
			if task.At == nil || task.At.Location() != time.Local ||
				task.At.Format("2006-01-02 15:04") != "2026-10-16 09:00" {
				t.Fatalf("at = %v", task.At)
			}
		}},
		{"modifiers", "beat every 5m timeout=90s overlap=queue miss=once command echo hi", func(t *testing.T, task *timer.Task) {
			if task.TimeoutMS != 90_000 || task.Overlap != timer.OverlapQueue || task.Miss != timer.MissOnce {
				t.Fatalf("mods = timeout %d overlap %q miss %q", task.TimeoutMS, task.Overlap, task.Miss)
			}
		}},
		{"interval alias", "beat interval 30s command echo hi", func(t *testing.T, task *timer.Task) {
			if task.Kind != timer.KindInterval || task.EveryMS != 30_000 {
				t.Fatalf("kind/every = %s/%d", task.Kind, task.EveryMS)
			}
		}},
	}
	for _, c := range ok {
		t.Run(c.name, func(t *testing.T) {
			task, err := parseTimerAdd(c.line, `D:\ws`, now)
			if err != nil {
				t.Fatalf("parse %q: %v", c.line, err)
			}
			c.check(t, task)
		})
	}

	bad := []struct{ name, line, wantErr string }{
		{"no payload keyword", "beat every 5m echo hi", "usage:"},
		{"missing payload", "x cron 0 9 * * 1-5 command", "missing payload"},
		{"unknown kind", "beat banana 5m command echo hi", "unknown kind"},
		{"bad duration", "beat every forever command echo hi", "bad interval"},
		{"bad cron", "beat cron 0 9 * * 99 prompt x", ""},
		{"past once", "x once 2020-01-01T00:00:00 prompt y", "not in the future"},
		{"bad relative", "x once +never prompt y", "bad relative time"},
		{"truncated line", "beat every 5m command", "usage:"},
		{"unknown modifier", "beat every 5m hot=1 command echo hi", "unknown modifier"},
		{"ws on command", "beat every 5m ws=/x command echo hi", "ws= applies to prompt"},
		{"bad timeout", "beat every 5m timeout=zz command echo hi", "bad timeout"},
		{"bad id", "b@d every 5m command echo hi", "id must"},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseTimerAdd(c.line, "", now)
			if err == nil {
				t.Fatalf("parse %q succeeded, want error", c.line)
			}
			if c.wantErr != "" && !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error %q does not contain %q", err, c.wantErr)
			}
		})
	}
}

// TestTimerSlashLifecycle: /timer add → list → disable → enable → rm,
// all against a scratch LYON_HOME, plus the usage answer for garbage.
// The scheduler is deliberately NOT started here — this is the
// store-only fallback path the same handlers take outside a session.
func TestTimerSlashLifecycle(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))
	m := newTestModel(t)

	m.slash("/timer")
	if out := m.cmdOutText(); !strings.Contains(out, "no timers") {
		t.Fatalf("empty list missing hint:\n%s", out)
	}

	m.slash("/timer add beat every 5m command echo hi")
	if out := m.cmdOutText(); !strings.Contains(out, "timer added") || !strings.Contains(out, "beat") {
		t.Fatalf("add confirmation missing:\n%s", out)
	}
	task := loadStored(t, "beat")
	if !task.Enabled || task.EveryMS != 5*60*1000 {
		t.Fatalf("stored task = enabled %v every %d", task.Enabled, task.EveryMS)
	}

	m.slash("/timer list")
	out := m.cmdOutText()
	for _, want := range []string{"beat", "every", "5m0s", "in 5m", "on:new", "cmd: echo hi"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list row missing %q:\n%s", want, out)
		}
	}

	m.slash("/timer disable beat")
	if loadStored(t, "beat").Enabled {
		t.Fatal("disable did not persist enabled=false")
	}
	m.slash("/timer list")
	if out := m.cmdOutText(); !strings.Contains(out, "off") || !strings.Contains(out, " -") {
		t.Fatalf("disabled row missing off state:\n%s", out)
	}

	m.slash("/timer enable beat")
	if !loadStored(t, "beat").Enabled {
		t.Fatal("enable did not persist enabled=true")
	}

	m.slash("/timer rm beat")
	if _, err := timer.Load("beat"); !os.IsNotExist(err) {
		t.Fatalf("task file still present after rm: %v", err)
	}
	m.slash("/timer list")
	if out := m.cmdOutText(); !strings.Contains(out, "no timers") {
		t.Fatalf("list after rm:\n%s", out)
	}

	m.slash("/timer banana")
	if out := m.cmdOutText(); !strings.Contains(out, "usage:") {
		t.Fatalf("unknown verb must answer usage:\n%s", out)
	}
	m.slash("/timer enable")
	if out := m.cmdOutText(); !strings.Contains(out, "usage:") {
		t.Fatalf("enable without id must answer usage:\n%s", out)
	}
}

// TestTimerSlashWithScheduler: the same verbs through a live scheduler —
// add arms the heap, disable/rm drop it, and Stop at the end leaves no
// file resurrection behind (deleted-ledger check on the store).
func TestTimerSlashWithScheduler(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))
	stop := startTimers()
	defer stop()
	if timersched() == nil {
		t.Fatal("startTimers did not publish a scheduler")
	}

	m := newTestModel(t)
	m.slash("/timer add beat every 1h command echo hi")
	if out := m.cmdOutText(); !strings.Contains(out, "timer added") {
		t.Fatalf("add via scheduler failed:\n%s", out)
	}
	m.slash("/timer disable beat")
	if loadStored(t, "beat").Enabled {
		t.Fatal("scheduler disable did not persist")
	}
	m.slash("/timer enable beat")
	if !loadStored(t, "beat").Enabled {
		t.Fatal("scheduler enable did not persist")
	}
	m.slash("/timer rm beat")
	if _, err := timer.Load("beat"); !os.IsNotExist(err) {
		t.Fatalf("task file still present after scheduler rm: %v", err)
	}

	stop()
	if timersched() != nil {
		t.Fatal("stop did not clear the published scheduler")
	}
}

// TestCmdTimerCLI: the `lyon timer` verbs — same store helpers as the
// TUI, POSIX-style exit codes (0 ok, 1 operation failed, 2 usage), and
// list output carrying the same columns. register stops at the missing
// file, so no OS scheduler is ever touched here.
func TestCmdTimerCLI(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))

	out := captureOut(t, func() {
		if code := cmdTimer([]string{"add", "beat", "every", "5m", "command", "echo", "hi"}); code != 0 {
			t.Errorf("add exit = %d", code)
		}
	})
	if !strings.Contains(out, "beat") {
		t.Fatalf("add output missing id: %q", out)
	}
	if task := loadStored(t, "beat"); !task.Enabled {
		t.Fatal("cli add stored a disabled task")
	}

	out = captureOut(t, func() {
		if code := cmdTimer([]string{"list"}); code != 0 {
			t.Errorf("list exit = %d", code)
		}
	})
	for _, want := range []string{"ID", "beat", "on:new", "cmd: echo hi"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list output missing %q:\n%s", want, out)
		}
	}

	captureOut(t, func() {
		if code := cmdTimer([]string{"disable", "beat"}); code != 0 {
			t.Errorf("disable exit = %d", code)
		}
	})
	if loadStored(t, "beat").Enabled {
		t.Fatal("cli disable did not persist")
	}
	captureOut(t, func() {
		if code := cmdTimer([]string{"enable", "beat"}); code != 0 {
			t.Errorf("enable exit = %d", code)
		}
	})
	if !loadStored(t, "beat").Enabled {
		t.Fatal("cli enable did not persist")
	}

	// Usage errors: 2. Missing task: 1 (register fails at Load, never
	// reaching schtasks). Help: 0.
	for _, c := range []struct {
		args []string
		code int
	}{
		{nil, 2},
		{[]string{"nope"}, 2},
		{[]string{"add"}, 2},
		{[]string{"add", "x", "every", "5m", "echo"}, 2},
		{[]string{"register", "missing-task"}, 1},
		{[]string{"enable", "missing-task"}, 1},
		{[]string{"rm"}, 2},
		{[]string{"help"}, 0},
	} {
		var code int
		captureOut(t, func() { code = cmdTimer(c.args) })
		if code != c.code {
			t.Errorf("cmdTimer(%v) = %d, want %d", c.args, code, c.code)
		}
	}

	captureOut(t, func() {
		if code := cmdTimer([]string{"rm", "beat"}); code != 0 {
			t.Errorf("rm exit = %d", code)
		}
	})
	if _, err := timer.Load("beat"); !os.IsNotExist(err) {
		t.Fatalf("task file still present after cli rm: %v", err)
	}
}

// TestTimerRunnerCommand: the default Runner's shell path — a success
// logs its output tail into $LYON_HOME/logs/timer-*.log, a failure
// carries the tail in the returned error (the scheduler turns that into
// last_status=error plus an ERROR log line).
func TestTimerRunnerCommand(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))

	ok := &timer.Task{ID: "echo", Command: "echo hello-timer"}
	if err := timerRunner(context.Background(), ok); err != nil {
		t.Fatalf("echo task: %v", err)
	}
	logPath := filepath.Join(os.Getenv("LYON_HOME"), "logs",
		"timer-"+time.Now().Format("2006-01-02")+".log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("timer log not written: %v", err)
	}
	if !strings.Contains(string(data), "timer echo: output: hello-timer") {
		t.Fatalf("output tail missing from log:\n%s", data)
	}

	bad := &timer.Task{ID: "bad", Command: "exit 3"}
	err = timerRunner(context.Background(), bad)
	if err == nil {
		t.Fatal("failing command must return an error")
	}
}

// TestPromptRunArgs: the `lyon run` argv a prompt fire execs — flags
// first, `--` terminator last-but-one, so a prompt starting with a dash
// stays a prompt (the shape OS-level registration also builds).
func TestPromptRunArgs(t *testing.T) {
	got := promptRunArgs(&timer.Task{Prompt: "-not-a-flag do it", Workspace: `D:\ws`})
	want := []string{"run", "-workspace", `D:\ws`, "--", "-not-a-flag do it"}
	if len(got) != len(want) {
		t.Fatalf("argv = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q (full %v)", i, got[i], want[i], got)
		}
	}
	got = promptRunArgs(&timer.Task{Prompt: "x"})
	if len(got) != 3 || got[0] != "run" || got[1] != "--" || got[2] != "x" {
		t.Fatalf("no-workspace argv = %v", got)
	}
}

// TestTimerPopupAndHelp: /timer is discoverable — popup entry filed
// under misc, and both /help bodies name it.
func TestTimerPopupAndHelp(t *testing.T) {
	found := false
	for _, c := range slashCommands {
		if c.cmd == "/timer" {
			found = true
			if c.cat != "misc" {
				t.Fatalf("/timer filed under %q, want misc", c.cat)
			}
		}
	}
	if !found {
		t.Fatal("/timer missing from the slash popup")
	}
	orig := uiLang
	t.Cleanup(func() { uiLang = orig; refreshBakedTexts() })
	for _, lang := range []string{"zh", "en"} {
		setLang(lang)
		if help := strings.Join(helpLines(), "\n"); !strings.Contains(help, "/timer") {
			t.Fatalf("help (%s) missing /timer", lang)
		}
	}
}

// TestStoreFallbackOps: with no scheduler published, the shared helpers
// still persist — this is the path `lyon timer` always takes.
func TestStoreFallbackOps(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))
	if timersched() != nil {
		t.Fatal("no scheduler should be published between sessions")
	}
	task := &timer.Task{ID: "solo", Kind: timer.KindInterval, EveryMS: 60_000,
		Command: "echo hi", Enabled: true}
	if err := addTimerTask(task); err != nil {
		t.Fatal(err)
	}
	if err := setTimerEnabled("solo", false); err != nil {
		t.Fatal(err)
	}
	if loadStored(t, "solo").Enabled {
		t.Fatal("fallback disable did not persist")
	}
	if err := removeTimerTask("solo"); err != nil {
		t.Fatal(err)
	}
	if _, err := timer.Load("solo"); !os.IsNotExist(err) {
		t.Fatalf("file survived remove: %v", err)
	}
}

// TestTimerStoreFileShape: what lands on disk is the documented
// everything-is-a-file contract — one JSON per id under $LYON_HOME/timers.
func TestTimerStoreFileShape(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))
	m := newTestModel(t)
	m.slash("/timer add standup cron 0 9 * * 1-5 prompt draft the standup")
	path := filepath.Join(os.Getenv("LYON_HOME"), "timers", "standup.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("task file: %v", err)
	}
	var wire struct {
		ID      string `json:"id"`
		Kind    string `json:"kind"`
		Cron    string `json:"cron"`
		Prompt  string `json:"prompt"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("task file is not JSON: %v", err)
	}
	if wire.ID != "standup" || wire.Kind != "cron" || wire.Cron != "0 9 * * 1-5" ||
		wire.Prompt != "draft the standup" || !wire.Enabled {
		t.Fatalf("wire shape = %+v", wire)
	}
}
