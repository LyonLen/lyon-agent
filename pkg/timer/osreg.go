package timer

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// OS-level registration turns the same Task definition into a Windows
// scheduled task (schtasks, names prefixed "lyon-") or a POSIX crontab
// line (tagged with a "# lyon:<id>" comment anchor). Construction is
// pure — testable on any platform — and only Register/Unregister/List
// execute the platform tools, chosen at runtime by GOOS.

// OSPrefix namespaces every artifact we create in the OS scheduler.
const OSPrefix = "lyon-"

// OSTaskName is the Windows task name for a task id.
func OSTaskName(id string) string { return OSPrefix + id }

// LyonExe resolves the lyon binary for OS-level execution: the current
// executable (EvalSymlinks best-effort), overridable in tests.
func LyonExe() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("timer: resolve lyon executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, nil
}

// --- Windows: schtasks -----------------------------------------------------

// SchtasksCreateArgs builds the `schtasks /Create` argv for t. exe is
// the lyon binary, now seeds /ST for interval schedules. The schedule
// mapping:
//
//	kind=once            -> /SC ONCE    /ST hh:mm /SD yyyy/mm/dd
//	kind=interval        -> /SC MINUTE|HOURLY|DAILY /MO n (largest exact divisor)
//	cron "m h * * *"     -> /SC DAILY   /ST h:m
//	cron "m h * * <dow>" -> /SC WEEKLY  /D MON,TUE.. /ST h:m
//	cron "m h d * *"     -> /SC MONTHLY /D d /ST h:m
//	cron "*/n * * * *"   -> /SC MINUTE  /MO n
//
// Anything else is rejected with a clear error rather than a mangled
// half-registration — the task simply stays in-process.
func SchtasksCreateArgs(t *Task, exe string, now time.Time) ([]string, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}
	tr, err := SchtasksTaskRun(t, exe)
	if err != nil {
		return nil, err
	}
	args := []string{"/Create", "/TN", OSTaskName(t.ID), "/TR", tr, "/F"}
	sched, err := schtasksSchedule(t, now)
	if err != nil {
		return nil, err
	}
	return append(args, sched...), nil
}

// SchtasksTaskRun builds the /TR command line: exe + `run` + prompt.
// The exe path is quoted when it contains spaces; the prompt follows
// CommandLineToArgvW quoting so Chinese text and embedded quotes
// survive the round trip through the task scheduler.
func SchtasksTaskRun(t *Task, exe string) (string, error) {
	var b strings.Builder
	b.WriteString(winQuoteArg(exe))
	if t.Command != "" {
		// Raw command tasks: hand the command to the shell, matching
		// the in-process runner's semantics.
		b.WriteString(" /C ")
		b.WriteString(winQuoteArg(t.Command))
		return b.String(), nil
	}
	b.WriteString(" run")
	if t.Workspace != "" {
		b.WriteString(" -workspace ")
		b.WriteString(winQuoteArg(t.Workspace))
	}
	b.WriteString(" ")
	b.WriteString(winQuoteArg(t.Prompt))
	return b.String(), nil
}

func schtasksSchedule(t *Task, now time.Time) ([]string, error) {
	switch t.Kind {
	case KindOnce:
		at := t.At
		return []string{
			"/SC", "ONCE",
			"/ST", at.Format("15:04"),
			"/SD", at.Format("2006/01/02"),
		}, nil
	case KindInterval:
		if t.EveryMS < 60_000 {
			return nil, fmt.Errorf("timer %s: OS-level interval must be >= 1 minute (got %dms); keep it in-process", t.ID, t.EveryMS)
		}
		if t.EveryMS%60_000 != 0 {
			return nil, fmt.Errorf("timer %s: OS-level interval must be whole minutes (got %dms)", t.ID, t.EveryMS)
		}
		mins := int64(t.EveryMS / 60_000)
		switch {
		case mins%1440 == 0 && mins/1440 <= 365:
			return []string{"/SC", "DAILY", "/MO", strconv.FormatInt(mins/1440, 10)}, nil
		case mins%60 == 0 && mins/60 <= 23:
			return []string{"/SC", "HOURLY", "/MO", strconv.FormatInt(mins/60, 10)}, nil
		case mins <= 1439:
			return []string{"/SC", "MINUTE", "/MO", strconv.FormatInt(mins, 10)}, nil
		default:
			return nil, fmt.Errorf("timer %s: interval %d minutes not expressible in schtasks; keep it in-process", t.ID, mins)
		}
	case KindCron:
		return schtasksCronSchedule(t)
	}
	return nil, fmt.Errorf("timer %s: unknown kind %q", t.ID, t.Kind)
}

func schtasksCronSchedule(t *Task) ([]string, error) {
	spec, err := parseCron(t.Cron)
	if err != nil {
		return nil, fmt.Errorf("timer %s: %w", t.ID, err)
	}
	fields := strings.Fields(t.Cron)
	// */n * * * * — every n minutes (phase starts at registration).
	if fields[0] != "*" && strings.HasPrefix(fields[0], "*/") &&
		fields[1] == "*" && fields[2] == "*" && fields[3] == "*" && fields[4] == "*" {
		n, _ := strconv.Atoi(strings.TrimPrefix(fields[0], "*/"))
		if n < 1 || n > 1439 {
			return nil, fmt.Errorf("timer %s: minute step %d out of schtasks range 1-1439", t.ID, n)
		}
		return []string{"/SC", "MINUTE", "/MO", strconv.Itoa(n)}, nil
	}
	// m h * * * — daily.
	if fields[2] == "*" && fields[4] == "*" && fields[3] == "*" &&
		len(spec.minute) == 1 && len(spec.hour) == 1 {
		return []string{"/SC", "DAILY", "/ST", hhmm(spec)}, nil
	}
	// m h * * <dow> — weekly (dow may be names, numbers, lists, ranges).
	if fields[2] == "*" && fields[3] == "*" && len(spec.minute) == 1 && len(spec.hour) == 1 {
		var days []string
		for d := 0; d <= 6; d++ {
			if spec.dow[d] {
				days = append(days, dowName(d))
			}
		}
		if len(days) == 0 {
			return nil, fmt.Errorf("timer %s: cron %q has no valid weekday", t.ID, t.Cron)
		}
		return []string{"/SC", "WEEKLY", "/D", strings.Join(days, ","), "/ST", hhmm(spec)}, nil
	}
	// m h d * * — monthly on day d.
	if fields[3] == "*" && fields[4] == "*" && len(spec.minute) == 1 && len(spec.hour) == 1 && len(spec.dom) == 1 {
		for d := range spec.dom {
			return []string{"/SC", "MONTHLY", "/D", strconv.Itoa(d), "/ST", hhmm(spec)}, nil
		}
	}
	return nil, fmt.Errorf("timer %s: cron %q not expressible as a scheduled task (use daily/weekly/monthly/step forms); keep it in-process", t.ID, t.Cron)
}

// hhmm renders the single minute+hour of a spec as "HH:MM".
func hhmm(spec *cronSpec) string {
	var m, h int
	for v := range spec.minute {
		m = v
	}
	for v := range spec.hour {
		h = v
	}
	return fmt.Sprintf("%02d:%02d", h, m)
}

func dowName(d int) string {
	for name, v := range dowNames {
		if v == d {
			return name
		}
	}
	return "?"
}

// winQuoteArg quotes s for one Windows argv slot (CommandLineToArgvW
// rules): bare when it has no space/tab/quote; otherwise wrapped in
// double quotes with embedded quotes backslash-escaped and backslash
// runs before quotes doubled.
func winQuoteArg(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	nBack := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			nBack++
		case '"':
			b.WriteString(strings.Repeat("\\", nBack*2+1))
			b.WriteByte('"')
			nBack = 0
		default:
			b.WriteString(strings.Repeat("\\", nBack))
			nBack = 0
			b.WriteByte(s[i])
		}
	}
	b.WriteString(strings.Repeat("\\", nBack*2))
	b.WriteByte('"')
	return b.String()
}

// SchtasksDeleteArgs builds the /Delete argv for a task id.
func SchtasksDeleteArgs(id string) []string {
	return []string{"/Delete", "/TN", OSTaskName(id), "/F"}
}

// ParseSchtasksQuery extracts lyon- task ids from `schtasks /Query /FO
// CSV /NH` output. Task names arrive as "\lyon-<id>".
func ParseSchtasksQuery(out string) []string {
	var ids []string
	r := csv.NewReader(strings.NewReader(out))
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil {
		return nil
	}
	for _, row := range rows {
		if len(row) == 0 {
			continue
		}
		name := strings.TrimPrefix(row[0], "\\")
		if strings.HasPrefix(name, OSPrefix) {
			ids = append(ids, strings.TrimPrefix(name, OSPrefix))
		}
	}
	return ids
}

// --- POSIX: crontab --------------------------------------------------------

// CrontabLine builds the crontab entry for t, exe being the lyon
// binary to invoke. The line ends with a "# lyon:<id>" anchor comment
// (stripped by sh at execution, used by us to identify our own
// entries) and redirects output into the dated OS log next to the
// timer log. once-kind and sub-divisor intervals have no honest
// crontab form and are rejected with a clear error.
func CrontabLine(t *Task, exe, logsDir string) (string, error) {
	if err := t.Validate(); err != nil {
		return "", err
	}
	sched, err := crontabSchedule(t)
	if err != nil {
		return "", err
	}
	var cmd string
	switch {
	case t.Command != "":
		cmd = t.Command
	default:
		parts := []string{shQuote(exe), "run"}
		if t.Workspace != "" {
			parts = append(parts, "-workspace", shQuote(t.Workspace))
		}
		parts = append(parts, "--", shQuote(t.Prompt))
		cmd = strings.Join(parts, " ")
	}
	// shQuote splits the log path so $(date) still expands: quoted
	// prefix + bare substitution + quoted suffix.
	line := sched + " " + cmd +
		" >> " + shQuote(filepath.Join(logsDir, "timer-os-")) + "$(date +%F)" + shQuote(".log") + " 2>&1" +
		" # " + crontabAnchor(t.ID)
	return line, nil
}

// crontabAnchor is the comment marker identifying our entries.
func crontabAnchor(id string) string { return "lyon:" + id }

func crontabSchedule(t *Task) (string, error) {
	switch t.Kind {
	case KindOnce:
		return "", fmt.Errorf("timer %s: crontab has no one-shot; keep once-tasks in-process", t.ID)
	case KindInterval:
		if t.EveryMS < 60_000 || t.EveryMS%60_000 != 0 {
			return "", fmt.Errorf("timer %s: OS-level interval must be whole minutes >= 60000ms (got %dms)", t.ID, t.EveryMS)
		}
		mins := t.EveryMS / 60_000
		if mins < 60 && 60%mins != 0 {
			return "", fmt.Errorf("timer %s: interval %d minutes does not divide 60; cron */n would drift — pick a divisor (1,2,3,4,5,6,10,12,15,20,30) or stay in-process", t.ID, mins)
		}
		if mins >= 60 && mins%60 == 0 {
			hours := mins / 60
			if hours < 24 && 24%hours == 0 {
				return fmt.Sprintf("0 */%d * * *", hours), nil
			}
			if hours == 24 {
				return "0 0 * * *", nil
			}
			return "", fmt.Errorf("timer %s: interval %d minutes not expressible in cron; keep it in-process", t.ID, mins)
		}
		return fmt.Sprintf("*/%d * * * *", mins), nil
	case KindCron:
		// Validate then pass the user's expression through verbatim.
		if _, err := parseCron(t.Cron); err != nil {
			return "", fmt.Errorf("timer %s: %w", t.ID, err)
		}
		return strings.Join(strings.Fields(t.Cron), " "), nil
	}
	return "", fmt.Errorf("timer %s: unknown kind %q", t.ID, t.Kind)
}

// shQuote wraps s in single quotes for sh, escaping embedded quotes
// the POSIX way: ' -> '\”.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// CrontabUpsert merges line into an existing crontab: any prior line
// carrying the same "# lyon:<id>" anchor is replaced, everything else
// (including other agents' lyon lines and the user's own crontab) is
// preserved byte-for-byte.
func CrontabUpsert(existing, line, id string) string {
	anchor := "# " + crontabAnchor(id)
	var keep []string
	for _, l := range splitLines(existing) {
		if strings.Contains(l, anchor) {
			continue
		}
		keep = append(keep, l)
	}
	keep = append(keep, line)
	return joinLines(keep)
}

// CrontabRemove strips the anchored line for id, preserving the rest.
func CrontabRemove(existing, id string) string {
	anchor := "# " + crontabAnchor(id)
	var keep []string
	for _, l := range splitLines(existing) {
		if strings.Contains(l, anchor) {
			continue
		}
		keep = append(keep, l)
	}
	return joinLines(keep)
}

// ParseCrontabLyonIDs lists every lyon-anchored task id in a crontab.
func ParseCrontabLyonIDs(content string) []string {
	var ids []string
	for _, l := range splitLines(content) {
		if i := strings.LastIndex(l, "# "+crontabAnchor("")); i >= 0 {
			id := strings.TrimSpace(l[i+len("# lyon:"):])
			if id != "" {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

func joinLines(lines []string) string {
	if len(lines) == 0 || (len(lines) == 1 && lines[0] == "") {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// --- Backend dispatch ------------------------------------------------------

// Backend registers, removes and lists OS-level timers.
type Backend interface {
	Register(t *Task) error
	Unregister(id string) error
	List() ([]string, error)
}

// NewBackend picks the platform backend at runtime.
func NewBackend() Backend {
	if runtime.GOOS == "windows" {
		return &schtasksBackend{}
	}
	return &crontabBackend{}
}

type schtasksBackend struct{}

func (b *schtasksBackend) Register(t *Task) error {
	exe, err := LyonExe()
	if err != nil {
		return err
	}
	args, err := SchtasksCreateArgs(t, exe, time.Now())
	if err != nil {
		return err
	}
	out, err := exec.Command("schtasks", args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if isAccessDenied(msg) {
			return fmt.Errorf("timer %s: schtasks /Create needs elevation or task-folder permission: %s", t.ID, msg)
		}
		return fmt.Errorf("timer %s: schtasks /Create failed: %v: %s", t.ID, err, msg)
	}
	LogInfo("timer %s: registered OS task %s", t.ID, OSTaskName(t.ID))
	return nil
}

func (b *schtasksBackend) Unregister(id string) error {
	out, err := exec.Command("schtasks", SchtasksDeleteArgs(id)...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		// Deleting a task that was never registered (or is already
		// gone) counts as success — idempotent by design.
		if strings.Contains(msg, "cannot find") || strings.Contains(msg, "ERROR: The system cannot find") ||
			strings.Contains(msg, "找不到") {
			return nil
		}
		return fmt.Errorf("timer %s: schtasks /Delete failed: %v: %s", id, err, msg)
	}
	LogInfo("timer %s: unregistered OS task %s", id, OSTaskName(id))
	return nil
}

func (b *schtasksBackend) List() ([]string, error) {
	out, err := exec.Command("schtasks", "/Query", "/FO", "CSV", "/NH").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("timer: schtasks /Query failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return ParseSchtasksQuery(string(out)), nil
}

func isAccessDenied(msg string) bool {
	l := strings.ToLower(msg)
	return strings.Contains(l, "access is denied") || strings.Contains(msg, "拒绝访问")
}

type crontabBackend struct{}

func (b *crontabBackend) Register(t *Task) error {
	exe, err := LyonExe()
	if err != nil {
		return err
	}
	dir, err := logsDir()
	if err != nil {
		return err
	}
	line, err := CrontabLine(t, exe, dir)
	if err != nil {
		return err
	}
	cur, err := readCrontab()
	if err != nil {
		return err
	}
	if err := writeCrontab(CrontabUpsert(cur, line, t.ID)); err != nil {
		return err
	}
	LogInfo("timer %s: registered crontab entry %s", t.ID, crontabAnchor(t.ID))
	return nil
}

func (b *crontabBackend) Unregister(id string) error {
	cur, err := readCrontab()
	if err != nil {
		return err
	}
	return writeCrontab(CrontabRemove(cur, id))
}

func (b *crontabBackend) List() ([]string, error) {
	cur, err := readCrontab()
	if err != nil {
		return nil, err
	}
	return ParseCrontabLyonIDs(cur), nil
}

// readCrontab fetches the current user's crontab. "no crontab" is an
// empty crontab, not an error.
func readCrontab() (string, error) {
	out, err := exec.Command("crontab", "-l").CombinedOutput()
	if err != nil {
		msg := strings.ToLower(string(out))
		if strings.Contains(msg, "no crontab") || bytes.Contains(out, []byte("no crontab")) {
			return "", nil
		}
		return "", fmt.Errorf("timer: crontab -l failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func writeCrontab(content string) error {
	cmd := exec.Command("crontab", "-")
	cmd.Stdin = strings.NewReader(content)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("timer: crontab write failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func logsDir() (string, error) {
	root, err := homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "logs"), nil
}
