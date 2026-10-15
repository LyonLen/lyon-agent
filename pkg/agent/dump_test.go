package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"lyon-agent/internal/mockapi"
	"lyon-agent/pkg/logx"
	"lyon-agent/pkg/tools"
)

// dumpGlob lists every dump file written under home.
func dumpGlob(t *testing.T, home string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(home, "dumps", "*", "*.json"))
	if err != nil {
		t.Fatalf("glob dumps: %v", err)
	}
	return matches
}

// readDump loads and parses one dump file, failing on I/O or JSON errors.
func readDump(t *testing.T, path string) runDumpFile {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read dump %s: %v", path, err)
	}
	var doc runDumpFile
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse dump %s: %v", path, err)
	}
	return doc
}

// TestRunDumpWritesTimeline: a normal run with a tool leaves exactly one
// dump under $LYON_HOME/dumps/<day>/<time>-<rand>.json carrying the full
// schema — prompt header, per-run turns, non-zero usage totals, and a
// seq-contiguous event timeline that contains the tool executions and
// ends on prompt_end.
func TestRunDumpWritesTimeline(t *testing.T) {
	home := tempDir(t)
	t.Setenv("LYON_HOME", home)
	mock := newCompactMock(t, []mockapi.Turn{
		{Tool: &mockapi.ToolCall{Name: "noop", Input: `{"reason":"dump-test"}`}},
		{Text: []string{"run-dump-final-answer"}},
	})
	reg := tools.NewRegistry()
	if err := tools.RegisterFunc(reg, "noop", "does nothing", false,
		func(ctx context.Context, _ struct{}) (string, error) { return "ok", nil }); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Model: "dump-test-model", CompactThreshold: -1, Workspace: tempDir(t)}
	eng := NewEngine(cfg, reg)
	sess := eng.NewSession(nil)
	defer sess.Close()

	const prompt = "dump-run-prompt: exercise the run dump"
	// SubmitOn proves the dump records the model actually requested, not
	// just the session config model.
	if err := sess.SubmitOn(prompt, "dump-override-model"); err != nil {
		t.Fatal(err)
	}
	pe := waitEvent(t, sess, EventPromptEnd)
	if pe.Err != "" {
		t.Fatalf("prompt failed: %s", pe.Err)
	}

	// The write runs on its own goroutine; Close waits it out, so the
	// dump is on disk once Close returns — no polling.
	sess.Close()
	files := dumpGlob(t, home)
	if len(files) != 1 {
		t.Fatalf("dump files = %v, want exactly 1", files)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}

	// Schema keys present, even when their value is the zero value.
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("dump is not a JSON object: %v", err)
	}
	for _, k := range []string{
		"schema_version", "run_id", "workspace", "model", "prompt",
		"first_prompt", "started_at", "ended_at", "duration_ms",
		"iterations", "stop_reason", "prompt_err", "usage", "turns",
		"events", "history",
	} {
		if _, ok := keys[k]; !ok {
			t.Fatalf("schema field %q missing from dump:\n%s", k, raw)
		}
	}

	var doc runDumpFile
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse dump: %v", err)
	}
	if doc.SchemaVersion != dumpSchemaVersion {
		t.Errorf("schema_version = %d, want %d", doc.SchemaVersion, dumpSchemaVersion)
	}
	if doc.Prompt != prompt || doc.FirstPrompt != prompt {
		t.Errorf("prompt = %q / first = %q, want %q", doc.Prompt, doc.FirstPrompt, prompt)
	}
	if doc.Model != "dump-override-model" {
		t.Errorf("model = %q, want the submitted override", doc.Model)
	}
	if doc.Workspace != cfg.Workspace {
		t.Errorf("workspace = %q, want %q", doc.Workspace, cfg.Workspace)
	}
	if doc.StopReason != pe.StopReason || doc.PromptErr != pe.Err {
		t.Errorf("stop/err = %q/%q, want %q/%q", doc.StopReason, doc.PromptErr, pe.StopReason, pe.Err)
	}
	if doc.Iterations != pe.Iterations || doc.Iterations <= 0 {
		t.Errorf("iterations = %d, prompt_end said %d", doc.Iterations, pe.Iterations)
	}
	if doc.DurationMS <= 0 {
		t.Errorf("duration_ms = %d, want > 0", doc.DurationMS)
	}
	if doc.EndedAt.Before(doc.StartedAt) {
		t.Errorf("ended_at %v before started_at %v", doc.EndedAt, doc.StartedAt)
	}

	// File name ↔ run_id ↔ started_at agree.
	base := strings.TrimSuffix(filepath.Base(files[0]), ".json")
	if !strings.HasSuffix(doc.RunID, base) {
		t.Errorf("run_id %q does not lead the file name %q", doc.RunID, base)
	}
	if dir := filepath.Base(filepath.Dir(files[0])); dir != doc.StartedAt.Format("2006-01-02") {
		t.Errorf("day dir %q, want %q", dir, doc.StartedAt.Format("2006-01-02"))
	}
	if !strings.HasPrefix(base, doc.StartedAt.Format("150405")) {
		t.Errorf("file name %q does not start with the run's HHMMSS", base)
	}

	// Usage: non-zero totals, exactly the sum of this run's turns.
	total := doc.Usage.Input + doc.Usage.Output + doc.Usage.CacheRead + doc.Usage.CacheCreation
	if total <= 0 {
		t.Errorf("usage is all zero: %+v", doc.Usage)
	}
	var sum Usage
	for _, tr := range doc.Turns {
		sum.Input += tr.Usage.Input
		sum.Output += tr.Usage.Output
		sum.CacheRead += tr.Usage.CacheRead
		sum.CacheCreation += tr.Usage.CacheCreation
	}
	if sum != doc.Usage {
		t.Errorf("usage %+v != sum of turns %+v", doc.Usage, sum)
	}
	if len(doc.Turns) != 2 {
		t.Fatalf("turns = %+v, want exactly 2 (tool turn + final turn)", doc.Turns)
	}
	if n := mock.Calls(); n != 2 {
		t.Fatalf("mock served %d calls, want 2 — the run is not what the dump claims", n)
	}
	if doc.Turns[0].Turn != 1 || doc.Turns[0].StopReason != "tool_use" {
		t.Errorf("turn 1 = %+v, want {1 tool_use}", doc.Turns[0])
	}
	if doc.Turns[1].Turn != 2 || doc.Turns[1].StopReason != "end_turn" {
		t.Errorf("turn 2 = %+v, want {2 end_turn}", doc.Turns[1])
	}

	// Timeline: seq contiguous from 1, tool executions present, two
	// turn_ends, prompt_end terminal and mirroring prompt_end's numbers.
	if len(doc.Events) < 6 {
		t.Fatalf("timeline too short: %d entries", len(doc.Events))
	}
	var sawUse, sawExecStart, sawExecEnd bool
	turnEnds := 0
	for i, ev := range doc.Events {
		if ev.Seq != i+1 {
			t.Fatalf("seq at index %d = %d, want %d", i, ev.Seq, i+1)
		}
		switch {
		case ev.Type == EventToolUseStart && ev.ToolName == "noop":
			sawUse = true
		case ev.Type == EventToolExecStart && ev.ToolName == "noop":
			sawExecStart = true
		case ev.Type == EventToolExecEnd && ev.ToolName == "noop":
			sawExecEnd = true
		case ev.Type == EventTurnEnd:
			turnEnds++
		}
	}
	if !sawUse || !sawExecStart || !sawExecEnd {
		t.Fatalf("tool trail incomplete: use=%v start=%v end=%v", sawUse, sawExecStart, sawExecEnd)
	}
	if turnEnds != 2 {
		t.Errorf("turn_end entries = %d, want 2", turnEnds)
	}
	if last := doc.Events[len(doc.Events)-1]; last.Type != EventPromptEnd ||
		last.Iterations != pe.Iterations || last.DurationMS != pe.DurationMS {
		t.Errorf("terminal entry = %+v, want prompt_end mirroring %+v", last, pe)
	}

	// The prompt text is readable both parsed and raw (grep-able on disk).
	if !strings.Contains(doc.History, prompt) || !strings.Contains(doc.History, "run-dump-final-answer") {
		t.Errorf("history missing prompt or answer:\n%.500s", doc.History)
	}
	if !strings.Contains(string(raw), prompt) {
		t.Error("prompt text not present verbatim in the dump file")
	}
}

// TestRunDumpOnInterrupt: an Esc-style interrupt ends the run with
// prompt_err set — the dump exists all the same, naming the interrupt as
// the terminal entry's error and keeping the tool that was running on the
// timeline.
func TestRunDumpOnInterrupt(t *testing.T) {
	home := tempDir(t)
	t.Setenv("LYON_HOME", home)
	// The blocker's cancellation logs "tool execution failed", which opens
	// the run log under this tempdir and holds it for the process — release
	// it before TempDir cleanup (Windows cannot unlink open files).
	t.Cleanup(logx.Close)
	mock := mockapi.New(mockapi.Config{
		Script: []mockapi.Turn{
			{Tool: &mockapi.ToolCall{Name: "blocker", Input: "{}"}},
			{Text: []string{"never reached"}},
		},
		Latency: 5 * time.Millisecond,
	})
	t.Cleanup(mock.Close)
	t.Setenv("ANTHROPIC_BASE_URL", mock.URL())
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "dump-interrupt-token")

	reg := tools.NewRegistry()
	if err := tools.RegisterFunc(reg, "blocker", "blocks until its context is canceled", false,
		func(ctx context.Context, _ struct{}) (string, error) {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(20 * time.Second):
				return "unblocked without interrupt", nil
			}
		}); err != nil {
		t.Fatal(err)
	}
	eng := NewEngine(Config{CompactThreshold: -1, Workspace: tempDir(t)}, reg)
	sess := eng.NewSession(nil)
	defer sess.Close()

	const prompt = "interrupt-this-run-for-the-dump"
	if err := sess.Submit(prompt); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, sess, EventToolExecStart)
	if !sess.Interrupt() {
		t.Fatal("Interrupt returned false while the prompt was running")
	}
	pe := waitEvent(t, sess, EventPromptEnd)
	if pe.Err != ErrInterrupted.Error() {
		t.Fatalf("prompt_end err = %q, want %q", pe.Err, ErrInterrupted)
	}

	sess.Close() // waits out the async dump write
	files := dumpGlob(t, home)
	if len(files) != 1 {
		t.Fatalf("dump files = %v, want exactly 1", files)
	}
	doc := readDump(t, files[0])
	if doc.Prompt != prompt {
		t.Errorf("prompt = %q, want %q", doc.Prompt, prompt)
	}
	if doc.PromptErr != ErrInterrupted.Error() {
		t.Errorf("prompt_err = %q, want the interrupt", doc.PromptErr)
	}
	if len(doc.Events) == 0 {
		t.Fatal("timeline empty")
	}
	last := doc.Events[len(doc.Events)-1]
	if last.Type != EventPromptEnd || last.Err != ErrInterrupted.Error() {
		t.Errorf("terminal entry = %+v, want prompt_end carrying the interrupt", last)
	}
	sawExecStart := false
	for _, ev := range doc.Events {
		if ev.Type == EventToolExecStart && ev.ToolName == "blocker" {
			sawExecStart = true
		}
	}
	if !sawExecStart {
		t.Error("the interrupted tool execution is missing from the timeline")
	}
}

// TestRunDumpOnFailedRun: a run killed by an API error (here a scripted
// 400 the guard does not repair) still dumps — prompt_err mirrors
// prompt_end, the error event rides the timeline, and zero-turn runs
// report zero usage instead of a bogus summary.
func TestRunDumpOnFailedRun(t *testing.T) {
	home := tempDir(t)
	t.Setenv("LYON_HOME", home)
	// Un-repairable 400s can log through apiguard — same Windows
	// open-handle cleanup concern as the interrupt test above.
	t.Cleanup(logx.Close)
	// A gateway that always answers 400 invalid_request_error citing no
	// repairable field: the SDK does not retry 4xx, so the very first
	// request fails the run fast.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w,
			`{"type":"error","error":{"type":"invalid_request_error","message":"scripted rejection for the dump test"}}`)
	}))
	t.Cleanup(ts.Close)
	t.Setenv("ANTHROPIC_BASE_URL", ts.URL)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "dump-failure-token")

	eng := NewEngine(Config{CompactThreshold: -1, Workspace: tempDir(t)}, nil)
	sess := eng.NewSession(nil)
	defer sess.Close()

	const prompt = "fail-this-run-on-purpose"
	if err := sess.Submit(prompt); err != nil {
		t.Fatal(err)
	}
	pe := waitEvent(t, sess, EventPromptEnd)
	if pe.Err == "" {
		t.Fatal("expected the run to fail")
	}

	sess.Close() // waits out the async dump write
	files := dumpGlob(t, home)
	if len(files) != 1 {
		t.Fatalf("dump files = %v, want exactly 1", files)
	}
	doc := readDump(t, files[0])
	if doc.Prompt != prompt {
		t.Errorf("prompt = %q, want %q", doc.Prompt, prompt)
	}
	if doc.PromptErr == "" || doc.PromptErr != pe.Err {
		t.Errorf("prompt_err = %q, prompt_end said %q", doc.PromptErr, pe.Err)
	}
	sawErr := false
	for _, ev := range doc.Events {
		if ev.Type == EventError && ev.Err != "" {
			sawErr = true
		}
	}
	if !sawErr {
		t.Error("the failing request left no error event on the timeline")
	}
	if last := doc.Events[len(doc.Events)-1]; last.Type != EventPromptEnd || last.Err != pe.Err {
		t.Errorf("terminal entry = %+v, want prompt_end with %q", last, pe.Err)
	}
	if len(doc.Turns) != 0 {
		t.Errorf("zero-turn run reported turns: %+v", doc.Turns)
	}
	if doc.Usage != (Usage{}) {
		t.Errorf("zero-turn run reported usage: %+v", doc.Usage)
	}
}

// TestRunDumpWriteFailureKeepsRunHealthy: with the dumps path
// un-writable (occupied by a plain file) every run still ends with a
// clean prompt_end — the failure surfaces only as "dump_error" lines in
// the existing audit log, never as a run error.
func TestRunDumpWriteFailureKeepsRunHealthy(t *testing.T) {
	home := tempDir(t)
	t.Setenv("LYON_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "dumps"), []byte("occupied"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = newCompactMock(t, []mockapi.Turn{
		{Text: []string{"answer-despite-dump-failure"}},
	})
	eng := NewEngine(Config{CompactThreshold: -1, Workspace: tempDir(t)}, nil)
	sess := eng.NewSession(nil)
	defer sess.Close()

	if err := sess.Submit("run-1 despite dump failure"); err != nil {
		t.Fatal(err)
	}
	if pe := waitEvent(t, sess, EventPromptEnd); pe.Err != "" {
		t.Fatalf("run 1 failed under dump pressure: %s", pe.Err)
	}
	// Second run too: the write failure is per-dump, never sticky.
	if err := sess.Submit("run-2 despite dump failure"); err != nil {
		t.Fatal(err)
	}
	if pe := waitEvent(t, sess, EventPromptEnd); pe.Err != "" {
		t.Fatalf("run 2 failed under dump pressure: %s", pe.Err)
	}
	// Close first: it waits for the failed writes' goroutines (whose
	// Fire calls enqueue dump_error), then Flush drains the queue.
	sess.Close()
	if files := dumpGlob(t, home); len(files) != 0 {
		t.Fatalf("dumps appeared despite the blocked path: %v", files)
	}

	eng.Hooks().Flush() // audit lines land asynchronously
	auditPath := filepath.Join(home, "audit", time.Now().Format("2006-01-02")+".jsonl")
	b, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("audit log missing: %v", err)
	}
	if !strings.Contains(string(b), `"event":"dump_error"`) {
		t.Fatalf("dump failure not reported through audit:\n%s", b)
	}
}

// TestRunDumpConcurrentWrites: N goroutines dumping into one LYON_HOME at
// the same instant must produce N distinct, individually parseable files
// and no temp litter — independent names plus atomic rename, no shared
// file and no lock anywhere.
func TestRunDumpConcurrentWrites(t *testing.T) {
	home := tempDir(t)
	t.Setenv("LYON_HOME", home)
	now := time.Now()
	const n = 64
	paths := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			doc := runDumpFile{
				SchemaVersion: dumpSchemaVersion,
				StartedAt:     now,
				EndedAt:       now,
				Prompt:        fmt.Sprintf("concurrent-%d", i),
				Turns:         []dumpTurn{},
				Events: []dumpEvent{
					{Seq: 1, Event: Event{Type: EventPromptEnd, TS: now}},
				},
			}
			p, err := writeRunDump(&doc)
			paths[i] = p
			errs[i] = err
		}(i)
	}
	wg.Wait()

	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("write %d: %v", i, errs[i])
		}
		if seen[paths[i]] {
			t.Fatalf("two dumps share a path: %s", paths[i])
		}
		seen[paths[i]] = true
		if doc := readDump(t, paths[i]); doc.Prompt != fmt.Sprintf("concurrent-%d", i) {
			t.Fatalf("dump %d holds another run's content: %q", i, doc.Prompt)
		}
	}
	if files := dumpGlob(t, home); len(files) != n {
		t.Fatalf("files = %d, want %d", len(files), n)
	}
	tmp, err := filepath.Glob(filepath.Join(home, "dumps", "*", "*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tmp) != 0 {
		t.Fatalf("temp files left behind: %v", tmp)
	}
}

// TestRunDumpTwoSessionsConcurrent: two sessions of one engine finish a
// run at the same moment against one LYON_HOME — both dumps land, each
// holding its own prompt, neither overwrite nor lock touching the other.
func TestRunDumpTwoSessionsConcurrent(t *testing.T) {
	home := tempDir(t)
	t.Setenv("LYON_HOME", home)
	// Slow turns keep both runs in flight together so their prompt_end
	// paths — and therefore their dump writes — overlap.
	mock := mockapi.New(mockapi.Config{
		Script:  []mockapi.Turn{{Text: []string{"shared-mock-answer"}}},
		Latency: 50 * time.Millisecond,
	})
	t.Cleanup(mock.Close)
	t.Setenv("ANTHROPIC_BASE_URL", mock.URL())
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "dump-two-sessions-token")

	eng := NewEngine(Config{CompactThreshold: -1, Workspace: tempDir(t)}, nil)
	sessA := eng.NewSession(nil)
	sessB := eng.NewSession(nil)
	defer sessA.Close()
	defer sessB.Close()

	if err := sessA.Submit("session-A dump prompt"); err != nil {
		t.Fatal(err)
	}
	if err := sessB.Submit("session-B dump prompt"); err != nil {
		t.Fatal(err)
	}
	// Each run's events fit the default buffer, so draining in order is
	// safe: B keeps producing while A is drained.
	if pe := waitEvent(t, sessA, EventPromptEnd); pe.Err != "" {
		t.Fatalf("session A: %s", pe.Err)
	}
	if pe := waitEvent(t, sessB, EventPromptEnd); pe.Err != "" {
		t.Fatalf("session B: %s", pe.Err)
	}
	sessA.Close() // both writes may still be in flight — Close waits them out
	sessB.Close()

	files := dumpGlob(t, home)
	if len(files) != 2 {
		t.Fatalf("dump files = %v, want exactly 2", files)
	}
	prompts := map[string]bool{}
	runIDs := map[string]bool{}
	for _, f := range files {
		doc := readDump(t, f)
		prompts[doc.Prompt] = true
		if runIDs[doc.RunID] {
			t.Fatalf("duplicate run_id %q", doc.RunID)
		}
		runIDs[doc.RunID] = true
	}
	if !prompts["session-A dump prompt"] || !prompts["session-B dump prompt"] {
		t.Fatalf("dumps do not cover both sessions: %v", prompts)
	}
}

// TestRunDumpClipsTimelineFields: oversized event fields are truncated to
// their caps (with a valid-JSON tool input), a closed recorder drops
// stragglers instead of writing them after prompt_end, and the history
// clip keeps the recent tail.
func TestRunDumpClipsTimelineFields(t *testing.T) {
	rec := &runDump{}
	bigText := strings.Repeat("y", dumpEventCap*3)
	bigInput := json.RawMessage(`{"blob":"` + strings.Repeat("z", dumpEventCap*3) + `"}`)
	rec.add(Event{Type: EventToolExecOutput, ToolName: "noisy", Text: bigText, TS: time.Now()})
	rec.add(Event{Type: EventToolUseStart, ToolName: "noisy", Input: bigInput, TS: time.Now()})

	events := rec.close(Event{Type: EventPromptEnd, TS: time.Now()})
	// A straggler after prompt_end (canceled tool's final report) must not
	// land in the closed timeline.
	rec.add(Event{Type: EventToolExecEnd, ToolName: "noisy", TS: time.Now()})

	if len(events) != 3 {
		t.Fatalf("timeline = %d entries, want 3 (straggler dropped)", len(events))
	}
	for i, ev := range events {
		if ev.Seq != i+1 {
			t.Fatalf("seq at %d = %d, want %d", i, ev.Seq, i+1)
		}
	}
	if last := events[len(events)-1]; last.Type != EventPromptEnd {
		t.Fatalf("terminal entry = %s, want prompt_end", last.Type)
	}
	suffix := len("... [truncated]")
	if n := len([]rune(events[0].Text)); n > dumpEventCap+suffix {
		t.Errorf("text field clipped to %d runes, cap %d", n, dumpEventCap+suffix)
	}
	if !json.Valid(events[1].Input) {
		t.Errorf("clipped tool input is not valid JSON: %.120s", events[1].Input)
	}
	if n := len(events[1].Input); n > 2*(dumpEventCap+suffix)+8 {
		t.Errorf("clipped tool input is %d bytes, unexpectedly large", n)
	}

	marker := "... [earlier history trimmed]\n"
	tail := clipHistoryTail(strings.Repeat("a", dumpHistoryCap*2), dumpHistoryCap)
	if !strings.HasPrefix(tail, marker) {
		t.Errorf("trimmed history lacks the marker: %.80s", tail)
	}
	if n := len([]rune(tail)); n != len([]rune(marker))+dumpHistoryCap {
		t.Errorf("trimmed history = %d runes, want marker+%d", n, dumpHistoryCap)
	}
}
