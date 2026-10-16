package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// This file is the READ side of the run-dump store (dump.go owns writing
// and the schema). The web DashboardService lists and replays dumps, but
// runDumpFile is unexported — keeping the decode here means the JSON
// layout lives in exactly one file, and callers get validated ids rather
// than hand-built paths.

// ErrBadDay rejects a day selector that is not a calendar date in
// "2006-01-02" shape — the only thing that may reach a filesystem path.
var ErrBadDay = errors.New("day must be YYYY-MM-DD")

// ErrBadRunID rejects a run id that does not match the writer's name
// ("YYYYMMDD-HHMMSS" + "-" + 12 hex). Validation doubles as path
// containment: only digits, dashes and hex ever join a dumps path.
var ErrBadRunID = errors.New("run id must be YYYYMMDD-HHMMSS-<12 hex>")

// runDumpDay is the dumps/ directory day format — local time, matching
// doc.StartedAt.Format in writeRunDump.
const runDumpDay = "2006-01-02"

// RunDumpRef is one persisted run's list row: header fields only, so a
// day of timelines never has to be decoded to render the table.
type RunDumpRef struct {
	RunID      string
	Workspace  string
	Model      string
	Prompt     string
	StartedAt  time.Time
	DurationMS int64
	Failed     bool // prompt_err != ""
}

// RunDumpEvent is one replayable timeline entry: the run-local sequence
// number plus the (already clipped at write time) event itself.
type RunDumpEvent struct {
	Seq int
	Event
}

// RunDumpDoc is a full dump document — the GetRun payload. Turns are
// deliberately absent: they are /stats-shaped data derivable from the
// timeline, and no current reader asks for them.
type RunDumpDoc struct {
	RunID       string
	Workspace   string
	Model       string
	Prompt      string
	FirstPrompt string
	StartedAt   time.Time
	EndedAt     time.Time
	DurationMS  int64
	Iterations  int
	StopReason  string
	PromptErr   string
	Usage       Usage
	SubUsage    map[string]Usage
	Events      []RunDumpEvent
	History     string
}

// runDumpHeader is the subset ListRunDumps decodes — unknown fields
// (events, history) are skipped by encoding/json without being retained.
type runDumpHeader struct {
	RunID      string    `json:"run_id"`
	Workspace  string    `json:"workspace"`
	Model      string    `json:"model"`
	Prompt     string    `json:"prompt"`
	StartedAt  time.Time `json:"started_at"`
	DurationMS int64     `json:"duration_ms"`
	PromptErr  string    `json:"prompt_err"`
}

// validDay reports whether day is exactly "YYYY-MM-DD" (charset check
// first, calendar check second) — day strings are path segments, so the
// shape is enforced before anything touches the filesystem.
func validDay(day string) bool {
	if len(day) != 10 || day[4] != '-' || day[7] != '-' {
		return false
	}
	for i, c := range day {
		if i == 4 || i == 7 {
			continue
		}
		if c < '0' || c > '9' {
			return false
		}
	}
	if _, err := time.Parse(runDumpDay, day); err != nil {
		return false
	}
	return true
}

// validRunID reports whether id matches the writer's name and derives the
// day directory + file base from it. Only after this passes may id be
// joined into a path.
func validRunID(id string) (day, base string, ok bool) {
	// YYYYMMDD (8) + '-' + HHMMSS (6) + '-' + 12 hex = 28.
	if len(id) != 28 || id[8] != '-' || id[15] != '-' {
		return "", "", false
	}
	hex := func(s string) bool {
		for _, c := range s {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return false
			}
		}
		return true
	}
	if !hex(id[:8]) || !hex(id[9:15]) || !hex(id[16:]) {
		return "", "", false
	}
	if _, err := time.Parse("20060102", id[:8]); err != nil {
		return "", "", false
	}
	if _, err := time.Parse("150405", id[9:15]); err != nil {
		return "", "", false
	}
	return id[:4] + "-" + id[4:6] + "-" + id[6:8], id[9:], true
}

// ListRunDumps returns the day's dump headers newest first, capped at
// limit (<=0 = no cap). A missing day directory is an empty list, not an
// error — most days nobody has run anything yet. One unreadable or
// corrupt file is skipped: a single bad dump must not hide the day.
func ListRunDumps(day string, limit int) ([]RunDumpRef, error) {
	if !validDay(day) {
		return nil, ErrBadDay
	}
	root, err := lyonHome()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(root, "dumps", day)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	// ReadDir sorts by filename; names start HHMMSS, so ascending is
	// chronological — walk it backwards for newest first.
	refs := make([]RunDumpRef, 0, len(entries))
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue // .tmp in-flight writes are not dumps
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var h runDumpHeader
		if err := json.Unmarshal(data, &h); err != nil {
			continue
		}
		if h.RunID == "" {
			h.RunID = strings.TrimSuffix(e.Name(), ".json") // pre-run-id fallback
		}
		refs = append(refs, RunDumpRef{
			RunID:      h.RunID,
			Workspace:  h.Workspace,
			Model:      h.Model,
			Prompt:     h.Prompt,
			StartedAt:  h.StartedAt,
			DurationMS: h.DurationMS,
			Failed:     h.PromptErr != "",
		})
		if limit > 0 && len(refs) >= limit {
			break
		}
	}
	return refs, nil
}

// LoadRunDump reads one full dump document by its run id. Bad id →
// ErrBadRunID (never a path); missing file → os.ErrNotExist.
func LoadRunDump(runID string) (*RunDumpDoc, error) {
	day, base, ok := validRunID(runID)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrBadRunID, runID)
	}
	root, err := lyonHome()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(root, "dumps", day, base+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc runDumpFile
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("dump %s: %w", runID, err)
	}
	out := &RunDumpDoc{
		RunID:       doc.RunID,
		Workspace:   doc.Workspace,
		Model:       doc.Model,
		Prompt:      doc.Prompt,
		FirstPrompt: doc.FirstPrompt,
		StartedAt:   doc.StartedAt,
		EndedAt:     doc.EndedAt,
		DurationMS:  doc.DurationMS,
		Iterations:  doc.Iterations,
		StopReason:  doc.StopReason,
		PromptErr:   doc.PromptErr,
		Usage:       doc.Usage,
		SubUsage:    doc.SubUsage,
		History:     doc.History,
		Events:      make([]RunDumpEvent, len(doc.Events)),
	}
	for i, e := range doc.Events {
		out.Events[i] = RunDumpEvent{Seq: e.Seq, Event: e.Event}
	}
	return out, nil
}
