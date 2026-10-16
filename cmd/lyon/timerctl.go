// Timer wiring (#8): the in-process scheduler's session lifecycle, the
// default Runner, and the parsing / store operations shared by the TUI's
// /timer command and the `lyon timer` CLI. pkg/timer owns scheduling,
// persistence and OS registration — this file only connects it to
// cmd/lyon: one scheduler per chat session, fires as subprocesses, all
// UI-facing reads as local store snapshots.

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"lyon-agent/pkg/timer"
)

// timerSchedMu guards timerSched: the slash handlers read it from the
// UI goroutine while start/stop run on the session goroutine.
var (
	timerSchedMu sync.Mutex
	timerSched   *timer.Scheduler
)

// startTimers boots the in-process scheduler for a chat session (#8).
// Start loads $LYON_HOME/timers and returns once the load is done; the
// returned func stops the scheduler as a drain barrier — in-flight runs
// finish (bounded by the package's 2s grace) before the process exits.
// Fires never touch the UI goroutine: the Runner starts subprocesses on
// the scheduler's own goroutines.
func startTimers() func() {
	sch, err := timer.NewScheduler(timerRunner, 0)
	if err != nil { // unreachable — timerRunner is non-nil
		timer.LogError("timers: scheduler unavailable: %v", err)
		return func() {}
	}
	sch.Start(context.Background())
	timerSchedMu.Lock()
	timerSched = sch
	timerSchedMu.Unlock()
	return func() {
		timerSchedMu.Lock()
		timerSched = nil
		timerSchedMu.Unlock()
		sch.Stop()
	}
}

// timersched returns the live scheduler, or nil outside a chat session.
// Store-only operations still work without it (save/load/delete); they
// just cannot re-arm a running heap.
func timersched() *timer.Scheduler {
	timerSchedMu.Lock()
	defer timerSchedMu.Unlock()
	return timerSched
}

// ── default Runner ─────────────────────────────────────────────────

// timerRunner is the default in-process Runner (DESIGN §4): command
// tasks run through the platform shell, prompt tasks through `lyon run`
// as a subprocess. exec.CommandContext ties the child's life to the
// run's timeout context, so Stop and per-task timeouts kill the child
// and Wait returns promptly — the runner contract (return once ctx is
// done) holds without extra plumbing.
func timerRunner(ctx context.Context, t *timer.Task) error {
	if t.Command != "" {
		name, args := shellCommand(t.Command)
		return timerRunCmd(ctx, t, name, args)
	}
	exe, err := timer.LyonExe()
	if err != nil {
		return err
	}
	return timerRunCmd(ctx, t, exe, promptRunArgs(t))
}

// promptRunArgs builds the `lyon run` argv for a prompt task — the same
// shape the OS-level backends register (flags, then `--`, then the
// single-line prompt, so a prompt starting with a dash stays a prompt).
func promptRunArgs(t *timer.Task) []string {
	args := []string{"run"}
	if t.Workspace != "" {
		args = append(args, "-workspace", t.Workspace)
	}
	return append(args, "--", t.Prompt)
}

// shellCommand picks the platform shell for command payloads — the same
// semantics the OS-level crontab line gets from `sh`.
func shellCommand(command string) (string, []string) {
	if runtime.GOOS == "windows" {
		comspec := os.Getenv("COMSPEC")
		if comspec == "" {
			comspec = "cmd.exe"
		}
		return comspec, []string{"/C", command}
	}
	return "sh", []string{"-c", command}
}

// timerRunCmd runs one fire and folds the result into the timer log:
// a failure carries the output tail in the returned error (the
// scheduler logs it and stores it as last_error), a success logs the
// tail at INFO — the tail holds the verdict, and the cap keeps one fire
// from flooding a daily log file.
func timerRunCmd(ctx context.Context, t *timer.Task, name string, args []string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		if tail := timerTail(out); tail != "" {
			return fmt.Errorf("%w: %s", err, tail)
		}
		return err
	}
	if tail := timerTail(out); tail != "" {
		timer.LogInfo("timer %s: output: %s", t.ID, tail)
	}
	return nil
}

// timerTailMax bounds subprocess output folded into one log line/error.
const timerTailMax = 1200

// timerTail trims output to its last timerTailMax runes, prefixing the
// cut with an ellipsis. Rune-safe so a CJK transcript never splits a
// character at the seam.
func timerTail(out []byte) string {
	s := strings.TrimSpace(string(out))
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) > timerTailMax {
		s = "…" + string(r[len(r)-timerTailMax:])
	}
	return s
}

// ── add-line parsing (shared verbatim by /timer add and `lyon timer add`) ──

// parseTimerAdd turns one add line into a validated task:
//
//	<id> <once|every|cron> <spec…> <command|prompt> <payload…> [k=v…]
//
// spec is one token for once (+duration or RFC3339) and every
// (duration), five for cron; k=v modifiers (ws= overlap= miss= timeout=)
// sit between the kind and the payload keyword. defWS is the prompt
// task's workspace when ws= is absent — the TUI passes its
// cfg.Workspace, the CLI the launch directory — stored absolute so an
// OS-level fire resolves the same root the session would.
func parseTimerAdd(line, defWS string, now time.Time) (*timer.Task, error) {
	usage := "usage: add <id> <once|every|cron> <spec> <command|prompt> <text> (e.g. add beat every 5m command echo hi)"
	toks := strings.Fields(line)
	// Minimum shape: id kind spec keyword payload.
	if len(toks) < 5 {
		return nil, fmt.Errorf("%s", usage)
	}
	kw := -1
	for i := 2; i < len(toks); i++ {
		if toks[i] == "command" || toks[i] == "prompt" {
			kw = i
			break
		}
	}
	if kw < 3 { // no keyword, or an empty spec before it
		return nil, fmt.Errorf("%s", usage)
	}
	specToks, mods, err := splitModifiers(toks[2:kw])
	if err != nil {
		return nil, err
	}
	payload := strings.TrimSpace(strings.Join(toks[kw+1:], " "))
	if payload == "" {
		return nil, fmt.Errorf("missing payload after %q", toks[kw])
	}

	t := &timer.Task{ID: toks[0], Enabled: true}
	switch toks[1] {
	case "once":
		t.Kind = timer.KindOnce
	case "every", "interval":
		t.Kind = timer.KindInterval
	case "cron":
		t.Kind = timer.KindCron
	default:
		return nil, fmt.Errorf("unknown kind %q (want once|every|cron)", toks[1])
	}

	spec := strings.Join(specToks, " ")
	switch t.Kind {
	case timer.KindOnce:
		at, err := parseOnceSpec(spec, now)
		if err != nil {
			return nil, err
		}
		t.At = &at
	case timer.KindInterval:
		d, err := time.ParseDuration(spec)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("bad interval %q (want a positive duration like 30s, 5m, 2h)", spec)
		}
		t.EveryMS = int64(d / time.Millisecond)
	case timer.KindCron:
		if spec == "" {
			return nil, fmt.Errorf("%s", usage)
		}
		t.Cron = spec
	}

	switch toks[kw] {
	case "command":
		t.Command = payload
	case "prompt":
		t.Prompt = payload
		t.Workspace = mods.ws
		if t.Workspace == "" {
			t.Workspace = defWS
		}
	}
	if mods.ws != "" && t.Prompt == "" {
		return nil, fmt.Errorf("ws= applies to prompt tasks only")
	}
	t.Overlap = mods.overlap
	t.Miss = mods.miss
	if mods.timeout > 0 {
		t.TimeoutMS = int64(mods.timeout / time.Millisecond)
	}
	if err := t.Validate(); err != nil {
		return nil, err
	}
	return t, nil
}

// parseOnceSpec resolves a once schedule: "+<duration>" relative to now,
// RFC3339 with zone, or the same without zone in local time. The instant
// must lie ahead — a past one-shot could never fire, and Save would
// leave a dead file behind.
func parseOnceSpec(spec string, now time.Time) (time.Time, error) {
	if spec == "" {
		return time.Time{}, fmt.Errorf("once needs an at: +duration (e.g. +30m) or RFC3339 (2026-10-16T09:00:00)")
	}
	var at time.Time
	if strings.HasPrefix(spec, "+") {
		d, err := time.ParseDuration(spec[1:])
		if err != nil || d <= 0 {
			return time.Time{}, fmt.Errorf("bad relative time %q (want +duration like +30m)", spec)
		}
		at = now.Add(d)
	} else {
		ok := false
		for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05"} {
			if parsed, err := time.ParseInLocation(layout, spec, time.Local); err == nil {
				at, ok = parsed, true
				break
			}
		}
		if !ok {
			return time.Time{}, fmt.Errorf("bad at %q (want +duration like +30m, or RFC3339 like 2026-10-16T09:00:00)", spec)
		}
	}
	if !at.After(now) {
		return time.Time{}, fmt.Errorf("at %s is not in the future", at.Format(time.RFC3339))
	}
	return at, nil
}

// timerMods are the recognized k=v modifiers on an add line.
type timerMods struct {
	ws      string
	overlap string
	miss    string
	timeout time.Duration
}

// splitModifiers separates k=v modifier tokens from schedule-spec tokens
// (cron lists and RFC3339 stamps never contain "=", so the split is
// unambiguous) and validates the modifier values.
func splitModifiers(toks []string) (spec []string, mods timerMods, err error) {
	for _, tok := range toks {
		key, val, ok := strings.Cut(tok, "=")
		if !ok {
			spec = append(spec, tok)
			continue
		}
		switch key {
		case "ws", "workspace":
			mods.ws = val
		case "overlap":
			mods.overlap = val
		case "miss":
			mods.miss = val
		case "timeout":
			d, derr := time.ParseDuration(val)
			if derr != nil || d <= 0 {
				return nil, mods, fmt.Errorf("bad timeout=%q (want a duration like 90s, 5m)", val)
			}
			mods.timeout = d
		default:
			return nil, mods, fmt.Errorf("unknown modifier %q (want ws= overlap= miss= timeout=)", tok)
		}
	}
	return spec, mods, nil
}

// ── store operations (scheduler-aware, shared by both surfaces) ────

// addTimerTask persists through the live scheduler when one is running
// (which also arms the heap) and falls back to a plain save otherwise.
func addTimerTask(t *timer.Task) error {
	if sch := timersched(); sch != nil {
		return sch.Add(t)
	}
	return timer.Save(t)
}

// setTimerEnabled flips enabled on a stored task. Through the scheduler
// this also re-arms/drops the heap entry live; without one it is a
// load-edit-save of the task file.
func setTimerEnabled(id string, on bool) error {
	if sch := timersched(); sch != nil {
		return sch.Enable(id, on)
	}
	t, err := timer.Load(id)
	if err != nil {
		return err
	}
	t.Enabled = on
	return timer.Save(t)
}

// removeTimerTask deletes a task: heap drop + file delete through the
// scheduler, file delete alone otherwise. The scheduler's deleted-ledger
// keeps a still-running fire from resurrecting the file.
func removeTimerTask(id string) error {
	if sch := timersched(); sch != nil {
		return sch.Remove(id)
	}
	return timer.Delete(id)
}

// ── presentation (shared by /timer list and `lyon timer list`) ─────

// timerListLines renders a snapshot of the store — one row per task
// with id, kind, schedule, next fire, run status and payload. The read
// is timer.LoadAll (a saveMu-bounded local file pass), so listing never
// touches the OS scheduler and cannot block the UI on schtasks/crontab.
func timerListLines(now time.Time) ([]string, error) {
	tasks, err := timer.LoadAll()
	if err != nil {
		return nil, err
	}
	if len(tasks) == 0 {
		return []string{paint(cDim, tr(
			"no timers — add one: /timer add beat every 5m command echo hi",
			"暂无定时器 — 添加一条：/timer add beat every 5m command echo hi"))}, nil
	}
	lines := []string{paint(cBold, fmt.Sprintf("%-14s %-6s %-12s %-11s %-10s %4s  %s",
		"ID", "KIND", "SCHEDULE", "NEXT", "STATUS", "RUNS", "PAYLOAD"))}
	for _, t := range tasks {
		lines = append(lines, timerRow(t, now))
	}
	return lines, nil
}

// timerRow is one table row. The status cell is ASCII by construction,
// so it can carry color without breaking the column math.
func timerRow(t *timer.Task, now time.Time) string {
	status, col := "off", cDim
	if t.Enabled {
		status = "on:new"
		col = cDim
		switch t.LastStatus {
		case "ok":
			status, col = "on:ok", cGreen
		case "error":
			status, col = "on:error", cRed
		case "timeout":
			status, col = "on:timeout", cYellow
		}
	}
	next := "-"
	if t.Enabled {
		if n, err := t.Next(now); err == nil {
			next = timerIn(now, n)
		}
	}
	head := fmt.Sprintf("%-14s %-6s %-12s %-11s ",
		truncRunes(t.ID, 14), timerKindStr(t), truncRunes(timerSchedStr(t), 12), next)
	return head + paint(col, status) +
		strings.Repeat(" ", 10-len(status)) +
		fmt.Sprintf(" %4d  %s", t.RunCount, truncRunes(timerPayloadStr(t), 36))
}

// timerKindStr normalizes the kind for display (interval → "every",
// matching the add syntax).
func timerKindStr(t *timer.Task) string {
	if t.Kind == timer.KindInterval {
		return "every"
	}
	return string(t.Kind)
}

// timerSchedStr renders the schedule half of a task: the at stamp for
// once, the duration for every, the expression for cron.
func timerSchedStr(t *timer.Task) string {
	switch t.Kind {
	case timer.KindOnce:
		if t.At != nil {
			return t.At.Format("01-02 15:04")
		}
	case timer.KindInterval:
		if t.EveryMS > 0 {
			return (time.Duration(t.EveryMS) * time.Millisecond).String()
		}
	case timer.KindCron:
		return t.Cron
	}
	return "-"
}

// timerPayloadStr renders the payload half: cmd/prompt tag plus text.
func timerPayloadStr(t *timer.Task) string {
	if t.Command != "" {
		return "cmd: " + t.Command
	}
	return "prompt: " + t.Prompt
}

// timerDescribe is a task's one-line summary — schedule, payload and
// next fire — used by add/enable confirmations on both surfaces.
func timerDescribe(t *timer.Task, now time.Time) string {
	next := "none"
	switch {
	case !t.Enabled:
		next = "off"
	default:
		if n, err := t.Next(now); err == nil {
			next = timerIn(now, n)
		}
	}
	return fmt.Sprintf("%s %s → %s · next %s",
		timerKindStr(t), timerSchedStr(t), timerPayloadStr(t), next)
}

// timerIn formats a next-fire instant relative to now: "in 4m32s" while
// close, an absolute stamp once the horizon stops being useful.
func timerIn(now, next time.Time) string {
	d := next.Sub(now)
	if d <= 0 {
		return "now"
	}
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("in %ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("in %dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 24*time.Hour:
		return fmt.Sprintf("in %dh%dm", int(d.Hours()), int(d.Minutes())%60)
	case d < 30*24*time.Hour:
		return fmt.Sprintf("in %dd%dh", int(d.Hours()/24), int(d.Hours())%24)
	default:
		return next.Format("01-02 15:04")
	}
}

// truncRunes caps s to max runes with a trailing ellipsis.
func truncRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return string(r[:max])
	}
	return string(r[:max-1]) + "…"
}
