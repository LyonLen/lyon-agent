// Package market is the task labor-hour ledger (#54): every finished
// prompt files its real wall time, and a task arriving fresh gets a
// price — the mean wall time of recent peers in its class. The estimate
// is deliberately dumb (class + recency, no model guessing): the point
// is that the number comes from THIS machine's own history, not from a
// guess about the model.
package market

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"lyon-agent/pkg/routing"
)

// Record is one finished task on the ledger. Failed and interrupted runs
// keep their row (history is history) but carry Err, which excludes them
// from estimates — a cancelled run's truncated time is not a price.
type Record struct {
	Class  string    `json:"class"`
	Model  string    `json:"model,omitempty"`
	Prompt string    `json:"prompt"` // clipped preview, enough to recognize
	DurMS  int64     `json:"dur_ms"`
	Iters  int       `json:"iterations,omitempty"`
	Err    string    `json:"err,omitempty"`
	TS     time.Time `json:"ts"`
}

// Table is the file-backed ledger at $LYON_HOME/market.json.
type Table struct {
	mu      sync.Mutex
	path    string
	Records []Record `json:"records"`
}

// historyCap is the ledger's size limit — months of daily use without
// the file growing unbounded; the oldest rows trim off the front.
const historyCap = 500

// estWindow is how many recent successful records feed an estimate —
// recent enough to track how this machine currently works, wide enough
// that one slow run doesn't set the price.
const estWindow = 10

// tables is the process-wide cache behind Open: one shared *Table per
// ledger path. Production calls Open() per finished prompt (TUI
// prompt_end, CLI fileMarket) without retaining the handle — a fresh
// Table per call would let two concurrent finishers load the whole file,
// each append their own row, and last-writer-wins silently drop one.
var (
	tablesMu sync.Mutex
	tables   = map[string]*Table{}
)

// Open returns the shared ledger table for this LYON_HOME; missing or
// corrupt state starts empty (never fatal — a market with no memory
// simply has no prices yet). The file is read once per path per process;
// later Open calls hand back the same in-memory table.
func Open() *Table {
	path := filepath.Join(homeDir(), "market.json")
	tablesMu.Lock()
	defer tablesMu.Unlock()
	if t, ok := tables[path]; ok {
		return t
	}
	t := &Table{path: path}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, t)
	}
	tables[path] = t
	return t
}

// Record files one finished task (best-effort persist). The write stages
// a temp file and renames — same contract as memory.Store and
// agent.SaveSnapshot: a crash mid-write must never truncate months of
// history, and Open reports a torn file as "no memory" (silent loss).
func (t *Table) Record(r Record) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r.Prompt = clip(r.Prompt, 120)
	t.Records = append(t.Records, r)
	if len(t.Records) > historyCap {
		t.Records = t.Records[len(t.Records)-historyCap:]
	}
	// Whole-table marshal (like routing): Open decodes into *Table.
	data, err := json.MarshalIndent(t, "", " ")
	if err != nil {
		return
	}
	// 0600: the ledger holds prompt previews — snapshot.go keeps the
	// same class of payload private for the same reason.
	tmp := t.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, t.path); err != nil {
		// Windows refuses rename-over-open (sharing violation). Never
		// fall back to an in-place write — the whole-table marshal
		// means the next successful Record flushes this row too.
		os.Remove(tmp)
	}
}

// Recent returns the last n records, oldest first.
func (t *Table) Recent(n int) []Record {
	t.mu.Lock()
	defer t.mu.Unlock()
	if n > len(t.Records) {
		n = len(t.Records)
	}
	out := make([]Record, n)
	copy(out, t.Records[len(t.Records)-n:])
	return out
}

// Est is one class's current price.
type Est struct {
	Class    string
	DurMS    int64
	Samples  int
	Fallback bool // priced from the whole ledger (class had no history)
}

// Estimate prices a task from its class's recent successful runs; a class
// with no history borrows the global pool (flagged), and an empty ledger
// returns false — no history, no number, no line.
func (t *Table) Estimate(text string) (Est, bool) {
	class := string(routing.Classify(text))
	t.mu.Lock()
	defer t.mu.Unlock()
	if est, ok := meanOf(t.recentOKLocked(class, estWindow)); ok {
		return Est{Class: class, DurMS: est.dur, Samples: est.n}, true
	}
	if est, ok := meanOf(t.recentOKLocked("", estWindow)); ok {
		return Est{Class: class, DurMS: est.dur, Samples: est.n, Fallback: true}, true
	}
	return Est{}, false
}

// Stat is one class's aggregate for the /market table.
type Stat struct {
	Class   string
	Samples int
	AvgMS   int64
	LastMS  int64
}

// Stats aggregates successful runs per class, most-recent class first.
func (t *Table) Stats() []Stat {
	t.mu.Lock()
	defer t.mu.Unlock()
	byClass := map[string]*Stat{}
	var order []string
	for i := len(t.Records) - 1; i >= 0; i-- {
		r := t.Records[i]
		if r.Err != "" {
			continue
		}
		s, ok := byClass[r.Class]
		if !ok {
			s = &Stat{Class: r.Class}
			byClass[r.Class] = s
			order = append(order, r.Class)
		}
		s.Samples++
		s.AvgMS += r.DurMS // divided once at the end
		if s.Samples == 1 {
			s.LastMS = r.DurMS
		}
	}
	out := make([]Stat, 0, len(order))
	for _, c := range order {
		s := byClass[c]
		s.AvgMS /= int64(s.Samples)
		out = append(out, *s)
	}
	return out
}

// recentOKLocked collects up to n successful records of a class ("" =
// every class), walking newest-first so the window tracks current pace.
func (t *Table) recentOKLocked(class string, n int) []Record {
	var out []Record
	for i := len(t.Records) - 1; i >= 0 && len(out) < n; i-- {
		r := t.Records[i]
		if r.Err != "" || (class != "" && r.Class != class) {
			continue
		}
		out = append(out, r)
	}
	return out
}

type mean struct {
	dur int64
	n   int
}

func meanOf(rs []Record) (mean, bool) {
	if len(rs) == 0 {
		return mean{}, false
	}
	var sum int64
	for _, r := range rs {
		sum += r.DurMS
	}
	return mean{dur: sum / int64(len(rs)), n: len(rs)}, true
}

func clip(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

func homeDir() string {
	if h := os.Getenv("LYON_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".lyon"
	}
	return filepath.Join(home, ".lyon")
}
