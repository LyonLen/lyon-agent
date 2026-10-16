// Package timer implements lyon's dual timer system (user request #8):
// an in-process scheduler that ticks while lyon runs, and OS-level
// registration (Windows Task Scheduler / POSIX crontab) that keeps
// firing after lyon exits. Both share one Task definition and one JSON
// store — everything is a file under $LYON_HOME/timers/*.json.
//
// In-process scheduler: a single goroutine drives a min-heap of next
// fire times; callbacks (Runners) execute in their own goroutines with
// a timeout, so a slow or wedged run can never stall the schedule.
// Overlap policy (default skip, optional coalesced queue) and restart
// behavior (default no catch-up, optional single catch-up run) are per
// task. See DESIGN.md for the full rationale.
//
// OS level: the same Task registers through timer.Backend — Windows
// via `schtasks /Create` (task names prefixed lyon-), POSIX via a
// crontab line tagged with a `# lyon:<id>` comment anchor. Command
// line construction is pure and testable on any platform; only the
// exec step is OS-specific.
//
// Logging: every fire, skip, completion, failure, timeout and OS
// registration lands in $LYON_HOME/logs/timer-YYYY-MM-DD.log. All
// writes funnel through logLine, the single seam where pkg/logx (once
// it exists) will be spliced in.
package timer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Kind selects how a task schedules its fires.
type Kind string

const (
	// KindOnce fires once at At, then disables itself.
	KindOnce Kind = "once"
	// KindInterval fires every EveryMS, measured from the tick.
	KindInterval Kind = "interval"
	// KindCron fires on a simplified 5-field cron expression (Cron).
	KindCron Kind = "cron"
)

// Overlap policies for a fire arriving while the previous run is live.
const (
	// OverlapSkip drops the tick and logs a WARN (default). Safe for
	// state-refresh style tasks: a run already in flight means the
	// world already has a fresh answer.
	OverlapSkip = "skip"
	// OverlapQueue arms exactly one catch-up run (ticks coalesce; they
	// never queue unboundedly). The catch-up fires the moment the live
	// run finishes. Use when every tick must produce a result.
	OverlapQueue = "queue"
)

// Miss policies for ticks that elapsed while lyon was not running.
const (
	// MissSkip schedules from now, never replaying the past (default):
	// a catch-up storm after an overnight stop is worse than one lost
	// tick. Missed fires are logged.
	MissSkip = "skip"
	// MissOnce schedules from now, but if at least one tick was truly
	// missed (Next(lastRun) already in the past), fires exactly one
	// catch-up run at startup — never one per missed tick.
	MissOnce = "once"
)

// Task is one timer: schedule + payload + last-run bookkeeping. It is
// the same definition whether it will be driven in-process or by the
// OS; registration style is a choice made at Register time, not a
// property of the definition.
type Task struct {
	ID   string `json:"id"`
	Kind Kind   `json:"kind"`

	// Schedule payload, one per Kind.
	EveryMS int64      `json:"every_ms,omitempty"` // KindInterval
	Cron    string     `json:"cron,omitempty"`     // KindCron
	At      *time.Time `json:"at,omitempty"`       // KindOnce (RFC3339)

	// Execution payload, exactly one of the two.
	Command   string `json:"command,omitempty"` // shell command
	Prompt    string `json:"prompt,omitempty"`  // fed to `lyon run`
	Workspace string `json:"workspace,omitempty"`

	Enabled   bool   `json:"enabled"`
	Overlap   string `json:"overlap,omitempty"` // OverlapSkip default
	Miss      string `json:"miss,omitempty"`    // MissSkip default
	TimeoutMS int64  `json:"timeout_ms,omitempty"`

	// Bookkeeping, rewritten on every run.
	RunCount   int64      `json:"run_count,omitempty"`
	LastRun    *time.Time `json:"last_run,omitempty"`
	LastStatus string     `json:"last_status,omitempty"` // ok | error | timeout
	LastError  string     `json:"last_error,omitempty"`

	// pendingCatchup marks one coalesced re-fire under overlap=queue.
	// Runtime-only state, never persisted.
	pendingCatchup bool
	// spent marks a one-shot whose schedule ran out at fire time: the
	// final run's bookkeeping must still be saved (with enabled=false)
	// even though the id left the heap before the run finished.
	spent bool
}

var idRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// minPayloadEveryMS is the floor for interval tasks that carry a
// payload (prompt/command): every fire of such a task forks a
// `lyon run` / shell child, so a sub-second interval is a fork
// storm — a real 15ms fixture leaked into a live store once and the
// TUI forked sixty children a second until the machine cried uncle.
// Tasks below the floor never enter the heap, and an on-disk copy is
// disarmed (enabled=false) so the storm cannot restart on every
// launch. Interval tasks without a payload (a pure tick) are not
// constrained; once/cron are minute-grained or one-shot anyway.
// Tests lower this variable via scratchHome to drive fast ticks with
// injected non-forking runners; production code must not touch it.
var minPayloadEveryMS int64 = 1000

// tooFast reports whether the task is a payload-carrying interval
// task below minPayloadEveryMS.
func (t *Task) tooFast() bool {
	return t.Kind == KindInterval && t.EveryMS > 0 && t.EveryMS < minPayloadEveryMS &&
		(t.Command != "" || t.Prompt != "")
}

// Validate rejects anything that could not be scheduled or persisted:
// IDs must be filename/taskname/anchor safe, the kind must be known,
// the schedule must be present, and exactly one payload must be set.
func (t *Task) Validate() error {
	if !idRe.MatchString(t.ID) {
		return fmt.Errorf("timer %q: id must match %s (filename + taskname safe)", t.ID, idRe)
	}
	switch t.Kind {
	case KindOnce:
		if t.At == nil {
			return fmt.Errorf("timer %s: kind=once requires at", t.ID)
		}
	case KindInterval:
		if t.EveryMS <= 0 {
			return fmt.Errorf("timer %s: kind=interval requires every_ms > 0", t.ID)
		}
	case KindCron:
		if strings.TrimSpace(t.Cron) == "" {
			return fmt.Errorf("timer %s: kind=cron requires a cron expression", t.ID)
		}
		if _, err := parseCron(t.Cron); err != nil {
			return fmt.Errorf("timer %s: %w", t.ID, err)
		}
	default:
		return fmt.Errorf("timer %s: unknown kind %q", t.ID, t.Kind)
	}
	if (t.Command == "") == (t.Prompt == "") {
		return fmt.Errorf("timer %s: need exactly one of command or prompt", t.ID)
	}
	if t.Prompt != "" && strings.ContainsAny(t.Prompt, "\r\n") {
		return fmt.Errorf("timer %s: prompt must be a single line", t.ID)
	}
	switch t.Overlap {
	case "", OverlapSkip, OverlapQueue:
	default:
		return fmt.Errorf("timer %s: overlap must be %q or %q", t.ID, OverlapSkip, OverlapQueue)
	}
	switch t.Miss {
	case "", MissSkip, MissOnce:
	default:
		return fmt.Errorf("timer %s: miss must be %q or %q", t.ID, MissSkip, MissOnce)
	}
	return nil
}

// OverlapPolicy resolves the effective overlap policy (default skip).
func (t *Task) OverlapPolicy() string {
	if t.Overlap == OverlapQueue {
		return OverlapQueue
	}
	return OverlapSkip
}

// MissPolicy resolves the effective miss policy (default skip).
func (t *Task) MissPolicy() string {
	if t.Miss == MissOnce {
		return MissOnce
	}
	return MissSkip
}

// ErrNoNext is returned by Next when the task has no future fire —
// a spent one-shot, or a cron so exotic it never matches.
var ErrNoNext = errors.New("timer: no next fire time")

// Next returns the earliest fire strictly after from. Interval tasks
// count from from itself (the scheduler passes the tick instant, so
// runs do not stack); cron is evaluated at minute granularity in local
// time; a one-shot yields its At when that still lies ahead.
func (t *Task) Next(from time.Time) (time.Time, error) {
	switch t.Kind {
	case KindOnce:
		if t.At == nil {
			return time.Time{}, fmt.Errorf("timer %s: once without at", t.ID)
		}
		if t.At.After(from) {
			return *t.At, nil
		}
		return time.Time{}, ErrNoNext
	case KindInterval:
		if t.EveryMS <= 0 {
			return time.Time{}, fmt.Errorf("timer %s: non-positive every_ms", t.ID)
		}
		return from.Add(time.Duration(t.EveryMS) * time.Millisecond), nil
	case KindCron:
		spec, err := parseCron(t.Cron)
		if err != nil {
			return time.Time{}, err
		}
		next, ok := spec.Next(from)
		if !ok {
			return time.Time{}, ErrNoNext
		}
		return next, nil
	default:
		return time.Time{}, fmt.Errorf("timer %s: unknown kind %q", t.ID, t.Kind)
	}
}

// TimersDir is the store root: $LYON_HOME/timers when set, else
// ~/.lyon/timers — the same store-root convention as sessions/mcp.
func TimersDir() (string, error) {
	root, err := homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "timers"), nil
}

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

// saveMu serializes every task-file operation (Save/Load/Delete)
// across the process. Two writers sharing one .tmp path trip Windows'
// rename locks, and a reader racing a Save's rename-over can fail
// with a sharing violation — all file access goes through this mutex.
// Lock order: s.mu (scheduler) may be held while taking saveMu, never
// the reverse.
var saveMu sync.Mutex

// Save writes t to $LYON_HOME/timers/<id>.json atomically (unique
// temp + rename), so a crash mid-write can never corrupt the store
// and two writers never share one staged path.
func Save(t *Task) error {
	if err := t.Validate(); err != nil {
		return err
	}
	dir, err := TimersDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	dst := filepath.Join(dir, t.ID+".json")
	saveMu.Lock()
	defer saveMu.Unlock()
	tmp, err := os.CreateTemp(dir, filepath.Base(dst)+"-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	_ = tmp.Chmod(0o644) // CreateTemp makes 0600; keep the file's original mode
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, dst); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// Load reads one task by id. A missing file is os.ErrNotExist; a
// corrupt or invalid file is a load error (LoadAll turns those into
// skips).
func Load(id string) (*Task, error) {
	dir, err := TimersDir()
	if err != nil {
		return nil, err
	}
	// The read shares saveMu with Save: on Windows a ReadFile racing
	// Save's rename-over can fail with a sharing violation.
	saveMu.Lock()
	data, err := os.ReadFile(filepath.Join(dir, id+".json"))
	saveMu.Unlock()
	if err != nil {
		return nil, err
	}
	t := &Task{}
	if err := json.Unmarshal(data, t); err != nil {
		return nil, fmt.Errorf("timer %s: corrupt file: %w", id, err)
	}
	// The stored id must match the filename — a mismatch means the
	// file was hand-edited or copied from another task.
	if t.ID == "" {
		t.ID = id
	}
	if t.ID != id {
		return nil, fmt.Errorf("timer %s: file holds id %q", id, t.ID)
	}
	if err := t.Validate(); err != nil {
		return nil, err
	}
	return t, nil
}

// LoadAll reads every task in the store, sorted by id. One corrupt
// file is skipped with a WARN — a single bad JSON must never take the
// whole timer system down.
func LoadAll() ([]*Task, error) {
	dir, err := TimersDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Task
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		t, err := Load(id)
		if err != nil {
			LogWarn("timers: skipping corrupt file %s: %v", e.Name(), err)
			continue
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Delete removes a task file. Missing file is not an error.
func Delete(id string) error {
	dir, err := TimersDir()
	if err != nil {
		return err
	}
	// Same serialization as Load: don't delete under a concurrent
	// Save's rename on Windows.
	saveMu.Lock()
	err = os.Remove(filepath.Join(dir, id+".json"))
	saveMu.Unlock()
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
