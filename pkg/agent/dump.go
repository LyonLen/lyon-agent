package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"lyon-agent/pkg/hooks"
)

// Run dumps (#3): one self-contained, post-hoc reviewable JSON file per
// finished run — a run being one user prompt's full tool loop, from Submit
// to EventPromptEnd (success, failure or interrupt alike). The audit JSONL
// in pkg/hooks stays the append-only, cross-run decision log (approvals,
// policy blocks, repairs); the dump is its debugging companion: it binds a
// run's event timeline, final history, usage and error into a single file
// you can open after the fact, when an execution error left no evidence
// anywhere ("执行报错查无对证").
//
// # Location
//
//	$LYON_HOME/dumps/<YYYY-MM-DD>/<HHMMSS>-<rand12>.json
//
// lyonHome() (snapshot.go) resolves the root. The day directory keys off
// the run's start; the file name is the start time-of-day plus 12 hex
// chars of crypto/rand. Names are unique per run by construction, so
// concurrent sessions never share a path: dumps take no lock, append to
// nothing shared, and cannot overwrite each other — the "并发抢锁" failure
// mode is designed out rather than locked around. Bytes land via an
// O_EXCL temp file next to the target and os.Rename (atomic within a
// directory), so a reader — or a crash mid-write — never sees a partial
// document. A rare name collision regenerates the suffix instead of
// clobbering the earlier file. Orphaned *.tmp files (crash between write
// and rename) are inert; readers glob *.json only.
//
// # Schema (schema_version 1)
//
//	{
//	  "schema_version": 1,                    // bump on incompatible layout change
//	  "run_id":   "20261015-143022-a1b2c3d4", // date dir + file base: locator AND correlation key
//	  "workspace": "...",                     // engine workspace (cfg.Workspace)
//	  "model":    "...",                      // model this run requested (per-prompt override resolved)
//	  "prompt":   "...",                      // the Submit line, clipped at dumpPromptCap
//	  "first_prompt": "...",                  // session's first prompt (topic anchor for review)
//	  "started_at":  <time.Time>,             // wall clock at drive entry
//	  "ended_at":    <time.Time>,             // wall clock at prompt_end
//	  "duration_ms": <int>,                   // end-to-end run time (prompt_end)
//	  "iterations":  <int>,                   // API calls this run made (prompt_end)
//	  "stop_reason": "max_iterations" | "",   // budget stop; "" = natural end or error
//	  "prompt_err":  "" | "interrupted" | …,  // non-empty exactly for failed/interrupted runs
//	  "usage":      {input, output, cache_read, cache_creation}, // totals over this run's turns
//	  "sub_usage":  {"<model>": {usage…}…},   // run_subagent billing lines, kept separate (#53)
//	  "turns":  [{turn, stop_reason, usage, duration_ms}…],       // one per turn_end; empty on zero-turn failures
//	  "events": [{seq, ts, type, …Event fields…}…],               // the run's timeline (see below)
//	  "history": "...role-tagged final messages (tail-bounded)..." // renderHistory at prompt_end
//	}
//
// events[] carries every Event the run emitted — one entry per event, seq
// from 1, strictly chronological, prompt_end always last: tool_use/tool_exec
// pairs, turn_end usage, error events, subagent nodes, queue injections,
// stream deltas, live tool output. Each entry embeds the Event's own JSON
// shape (tool_id, exec_id, input, error, usage, iterations, detail, sub,
// sub_id, …), so anything a renderer could show is on disk here too.
// String-ish fields (text, thinking, detail, error) are clipped per entry
// at dumpEventCap and tool inputs are clipped-or-re-encoded as valid JSON,
// so one giant tool output cannot bloat the file. history uses
// renderHistory (thinking dropped, 4000-rune blocks) then keeps the TAIL
// of dumpHistoryCap runes — in a post-mortem the recent end of the
// conversation matters more than the opening turns.
//
// # Design notes
//
//   - Best effort, always: writeRunDump failures are reported once
//     through the existing audit channel (event "dump_error") and
//     swallowed. finishRunDump sits on the prompt_end path but can never
//     change it — a dump problem must not surface as a run problem, and
//     every run (success / failure / interrupt / close-mid-run) dumps
//     identically.
//   - The disk write never blocks the run: the document is assembled on
//     the drive goroutine (session state must be snapshotted under
//     histMu, and that part is pure in-memory work), then handed to a
//     short-lived writer goroutine — prompt_end announces without waiting
//     for the disk, the same never-block rule hooks.Fire follows for the
//     audit queue. Writers serialize process-wide on dumpWriteGate:
//     uniqueness already makes the file layer lock-free, but parallel
//     create+rename bursts contend on the directory and the AV scanner,
//     and that contention is measurable as prompt latency — one disk
//     stream keeps dumps polite without ever putting the wait on a run
//     (it parks on the writer goroutine instead). Session.Close waits
//     out pending writes (dumpPending), so a process exiting right
//     after a run still lands its dumps, audit flush style.
//   - One recorder per run: drive stores it at prompt start, emit appends
//     every event while it is open, the prompt_end path swaps it out and
//     writes. Run-scoped by construction — no cross-run bleed, no shared
//     state between sessions.
//   - Duplicates nothing from audit: approvals, policy decisions and
//     history-repair lines stay in audit; the dump's value is the
//     integrated per-run view (timeline + history + usage + error) in one
//     file.
//
// # Future extensions (candidates for schema_version 2)
//
//   - Run correlation: session_id plus prev_run_id / run chain so one
//     conversation's dumps link into a browsable timeline; queue_injected
//     events already name the mid-run joins.
//   - Full event stream: untruncated deltas/text as a sidecar .jsonl (or
//     an "events_full" field), keeping this file bounded.
//   - Retention: prune dumps/<day> past N days (mirror of archiveKeep)
//     and an opt-out (LYON_DUMP=off, mirroring LYON_AUDIT).
//   - Optional full message list (the runner's exact BetaMessageParam
//     JSON) behind a config flag; history now renders a bounded summary.

const (
	// dumpSchemaVersion identifies this layout; bump on incompatible change.
	dumpSchemaVersion = 1
	// dumpEventCap clips one string-ish field (text/thinking/detail/error,
	// raw tool input) inside a timeline entry.
	dumpEventCap = 2000
	// dumpPromptCap clips the prompt / first_prompt header fields.
	dumpPromptCap = 16384
	// dumpHistoryCap bounds the rendered final history (tail kept).
	dumpHistoryCap = 16000
	// dumpNameRandBytes is the entropy in the file-name suffix: 6 bytes =
	// 12 hex chars, so two sessions dumping in the same second cannot land
	// on the same path.
	dumpNameRandBytes = 6
	// dumpWriteAttempts bounds unique-name retries (stat/O_EXCL/rename
	// races) before giving up — the give-up is audited, never fatal.
	dumpWriteAttempts = 4
)

// dumpWriteGate serializes the DISK PHASE of dump writes process-wide.
// Correctness never needs it — every run owns a unique path, so the file
// layer itself stays lock-free (writeRunDump is directly exercised by 64
// concurrent writers in tests) — but concurrent small-file create+rename
// bursts contend on the directory and the AV scanner: measured on this
// machine, one write is ~3-5ms alone while a 16-way burst spikes a
// single write to ~47ms, which showed up as engine prompt p95 in the
// load gate (24ms → 47ms overhead). One writer at a time keeps the file
// layer as polite as the audit layer's single writer; the parked writers
// wait OFF the critical path, so prompt_end still never blocks — and a
// full serial drain measures back under the gate (8ms overhead).
var dumpWriteGate sync.Mutex

// runDumpFile is the on-disk dump document — the schema above. Every core
// field serializes unconditionally (no omitempty) so a reader can rely on
// the key set; only sub_usage (meaningful only when subagents billed) is
// conditional.
type runDumpFile struct {
	SchemaVersion int              `json:"schema_version"`
	RunID         string           `json:"run_id"`
	Workspace     string           `json:"workspace"`
	Model         string           `json:"model"`
	Prompt        string           `json:"prompt"`
	FirstPrompt   string           `json:"first_prompt"`
	StartedAt     time.Time        `json:"started_at"`
	EndedAt       time.Time        `json:"ended_at"`
	DurationMS    int64            `json:"duration_ms"`
	Iterations    int              `json:"iterations"`
	StopReason    string           `json:"stop_reason"`
	PromptErr     string           `json:"prompt_err"`
	Usage         Usage            `json:"usage"`
	SubUsage      map[string]Usage `json:"sub_usage,omitempty"`
	Turns         []dumpTurn       `json:"turns"`
	Events        []dumpEvent      `json:"events"`
	History       string           `json:"history"`
}

// dumpTurn is one completed API turn of the run — the per-run slice of
// what /stats shows session-wide (turn numbers keep counting across runs
// and restores).
type dumpTurn struct {
	Turn       int    `json:"turn"`
	StopReason string `json:"stop_reason"`
	Usage      Usage  `json:"usage"`
	DurationMS int64  `json:"duration_ms"`
}

// dumpEvent is one timeline entry: the run-local sequence number plus the
// Event itself (embedded, so Event's JSON tags serialize inline). The
// Event stored here is a clipped copy — the live stream is untouched.
type dumpEvent struct {
	Seq int `json:"seq"`
	Event
}

// runDump is one run's in-memory recorder. Drive goroutine creates it at
// prompt start and closes it at prompt_end; emit appends from tool and
// subagent goroutines while it is open — hence mu. After close, add is a
// no-op: events straggling in past prompt_end (a canceled tool's final
// report) are dropped rather than written after the terminal entry.
type runDump struct {
	mu      sync.Mutex
	started time.Time // drive entry — the run's wall-clock start
	prompt  string    // the Submit line this run answers
	model   string    // model actually requested (override resolved)
	seq     int
	events  []dumpEvent
	done    bool
}

// beginRunDump opens the recorder for the run drive is about to run.
// prompt_start timing: called once per drive, before any event of the run
// can fire, so the timeline cannot miss its own opening.
func (s *Session) beginRunDump(prompt, runModel string, started time.Time) {
	model := runModel
	if model == "" {
		model = s.eng.model()
	}
	s.dumpRec.Store(&runDump{started: started, prompt: prompt, model: model})
}

// add appends one event to the run's timeline (clipped copy, seq assigned
// under mu). Called from emit on any goroutine; a closed or absent
// recorder drops the event — collection must never block or fail a run.
func (r *runDump) add(e Event) {
	if e.TS.IsZero() {
		e.TS = time.Now()
	}
	clipDumpEvent(&e)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done {
		return
	}
	r.seq++
	r.events = append(r.events, dumpEvent{Seq: r.seq, Event: e})
}

// close marks the recorder finished (later adds drop), appends the
// prompt_end entry as the terminal event, and hands the timeline back.
// After it returns the slice is immutable, so the caller may marshal it
// without further locking.
func (r *runDump) close(pe Event) []dumpEvent {
	if pe.TS.IsZero() {
		pe.TS = time.Now()
	}
	clipDumpEvent(&pe)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.done = true
	r.seq++
	r.events = append(r.events, dumpEvent{Seq: r.seq, Event: pe})
	return r.events
}

// clipDumpEvent bounds the string-ish fields of one timeline copy so no
// single event can bloat the file. Tool input is valid JSON when it came
// from the stream; an oversized or malformed payload is re-encoded as a
// JSON string (always valid, visibly truncated) — an invalid raw message
// anywhere would fail the whole document's marshal.
func clipDumpEvent(e *Event) {
	e.Text = clipRunes(e.Text, dumpEventCap)
	e.Thinking = clipRunes(e.Thinking, dumpEventCap)
	e.Detail = clipRunes(e.Detail, dumpEventCap)
	e.Err = clipRunes(e.Err, dumpEventCap)
	if len(e.Input) > 0 && (len(e.Input) > dumpEventCap || !json.Valid(e.Input)) {
		if b, err := json.Marshal(clipRunes(string(e.Input), dumpEventCap)); err == nil {
			e.Input = b
		} else {
			e.Input = nil
		}
	}
}

// finishRunDump closes the run's recorder, assembles the document and
// ships the file write to its own goroutine. Drive calls it on the
// prompt_end path (histMu held) before announcing the end: the snapshot
// side — recorder close, history render, session fields — is pure
// in-memory work done here while the state is stable; only the disk write
// runs off-thread (queued behind dumpWriteGate when other sessions are
// writing), so prompt_end never waits for the file — the engine overhead
// gate measures this path, and hooks.Fire has always followed the same
// never-block rule. dumpPending is raised BEFORE prompt_end is visible,
// so Close waiting on it also waits for this run's write. Best effort:
// write failures become one audit line ("dump_error") and nothing else.
// A nil recorder (no run opened — defensive) is a no-op.
func (s *Session) finishRunDump(pe Event) {
	rec := s.dumpRec.Swap(nil)
	if rec == nil {
		return
	}
	doc := s.makeRunDump(rec, pe)
	s.dumpPending.Add(1)
	go func() {
		defer s.dumpPending.Add(-1)
		dumpWriteGate.Lock()
		defer dumpWriteGate.Unlock()
		if _, err := writeRunDump(&doc); err != nil {
			s.eng.hooks.Fire(hooks.Record{
				Event:  "dump_error",
				Err:    err.Error(),
				Detail: "run dump not written (#3) — run unaffected",
			})
		}
	}()
}

// makeRunDump assembles the document from the closed recorder plus session
// state. Drive goroutine at prompt_end: histMu is held, so the history
// read is stable; s.mu is taken only for the firstPrompt snapshot (lock
// order histMu before mu — satisfied).
func (s *Session) makeRunDump(rec *runDump, pe Event) runDumpFile {
	events := rec.close(pe)

	// Per-run turns ride the turn_end entries this run emitted (every
	// recorded turn emits exactly one, compaction turns included); the
	// usage total is their sum. Subagent billing lines are folded
	// separately — they ride inside a turn but bill under their own
	// model (#53).
	turns := []dumpTurn{}
	var total Usage
	var sub map[string]Usage
	for _, de := range events {
		switch de.Type {
		case EventTurnEnd:
			t := dumpTurn{Turn: de.Turn, StopReason: de.StopReason, DurationMS: de.DurationMS}
			if de.Usage != nil {
				t.Usage = *de.Usage
				total.Input += de.Usage.Input
				total.Output += de.Usage.Output
				total.CacheRead += de.Usage.CacheRead
				total.CacheCreation += de.Usage.CacheCreation
			}
			turns = append(turns, t)
		case EventSubagentEnd:
			if de.Usage != nil && de.ToolName != "" {
				if sub == nil {
					sub = make(map[string]Usage, 1)
				}
				u := sub[de.ToolName]
				u.Input += de.Usage.Input
				u.Output += de.Usage.Output
				u.CacheRead += de.Usage.CacheRead
				u.CacheCreation += de.Usage.CacheCreation
				sub[de.ToolName] = u
			}
		}
	}

	s.mu.Lock()
	first := s.firstPrompt
	s.mu.Unlock()

	return runDumpFile{
		SchemaVersion: dumpSchemaVersion,
		Workspace:     s.eng.cfg.Workspace,
		Model:         rec.model,
		Prompt:        clipRunes(rec.prompt, dumpPromptCap),
		FirstPrompt:   clipRunes(first, dumpPromptCap),
		StartedAt:     rec.started,
		EndedAt:       pe.TS,
		DurationMS:    pe.DurationMS,
		Iterations:    pe.Iterations,
		StopReason:    pe.StopReason,
		PromptErr:     clipRunes(pe.Err, dumpEventCap),
		Usage:         total,
		SubUsage:      sub,
		Turns:         turns,
		Events:        events,
		History:       clipHistoryTail(renderHistory(s.currentHistoryLocked()), dumpHistoryCap),
	}
}

// writeRunDump persists doc under $LYON_HOME/dumps/<day>/ and returns the
// path it landed at. The name is time-of-day plus crypto/rand; the write
// is O_EXCL temp + os.Rename. No shared file and no lock means concurrent
// sessions never contend — each dump owns its path outright.
func writeRunDump(doc *runDumpFile) (string, error) {
	root, err := lyonHome()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, "dumps", doc.StartedAt.Format("2006-01-02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	var lastErr error
	for attempt := 0; attempt < dumpWriteAttempts; attempt++ {
		suffix, err := randHex(dumpNameRandBytes)
		if err != nil {
			return "", err
		}
		base := doc.StartedAt.Format("150405") + "-" + suffix
		doc.RunID = doc.StartedAt.Format("20060102-150405") + "-" + suffix
		final := filepath.Join(dir, base+".json")
		if _, err := os.Stat(final); err == nil {
			lastErr = fmt.Errorf("name already taken: %s", final)
			continue // absurdly rare random collision — fresh suffix next round
		}
		data, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return "", err
		}
		tmp := final + ".tmp"
		f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			if os.IsExist(err) {
				lastErr = err // another writer holds this exact name — regenerate
				continue
			}
			return "", err
		}
		if _, err := f.Write(data); err != nil {
			f.Close()
			os.Remove(tmp)
			return "", err
		}
		if err := f.Close(); err != nil { // close before rename: Windows refuses to move open files
			os.Remove(tmp)
			return "", err
		}
		if err := os.Rename(tmp, final); err != nil {
			os.Remove(tmp)
			if _, statErr := os.Stat(final); statErr == nil {
				lastErr = err // the final name landed meanwhile — try a new one
				continue
			}
			return "", err
		}
		return final, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no unique name in %s", dir)
	}
	return "", fmt.Errorf("run dump: %w", lastErr)
}

// randHex returns n random bytes as lowercase hex — the suffix that keeps
// concurrent runs from ever sharing a dump path.
func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// clipHistoryTail bounds the rendered history while keeping its recent
// end: a post-mortem cares about where the run was when it ended, not
// about re-reading the opening turns (those live in older dumps anyway).
func clipHistoryTail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "... [earlier history trimmed]\n" + string(r[len(r)-n:])
}
