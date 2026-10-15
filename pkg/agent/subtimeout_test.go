package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"lyon-agent/internal/mockapi"
	"lyon-agent/pkg/tools"
)

// subTimeoutConfig builds a config with an injected-short ToolTimeout: the
// nested run's no-progress budget is 2×ToolTimeout, so a small value lets
// the watchdog race a slow mock in milliseconds instead of minutes.
func subTimeoutConfig(t *testing.T, toolTimeout time.Duration) Config {
	t.Helper()
	return Config{
		Model:         "main-x",
		SubagentModel: "sub-x",
		ToolTimeout:   toolTimeout,
		EventBuffer:   64,
		Workspace:     tempDir(t),
		WebToolMode:   "off",
	}
}

// subTimeoutSession starts the engine/session pair for the tests below.
// LYON_HOME is parked in a temp dir first and eng.Close registered after,
// so the LIFO cleanups stop the hooks writer before the dir is deleted.
// reg may be nil (engine built-ins only).
func subTimeoutSession(t *testing.T, cfg Config, reg *tools.Registry) *Session {
	t.Helper()
	t.Setenv("LYON_HOME", tempDir(t))
	eng := NewEngine(cfg, reg)
	t.Cleanup(eng.Close)
	sess := eng.NewSession(nil)
	t.Cleanup(sess.Close)
	return sess
}

// TestSubagentStallDiagnosis: a nested run that gets no stream events for
// the whole idle budget (mock first byte far beyond it) is cancelled by
// the watchdog — not by a wall clock — and the error carries the task
// name, the budget and the stall point, so a timeout is diagnosable
// instead of a bare context error. This is the plan-mode timeout report's
// "no progress" half: stuck runs die fast AND say where they stuck.
func TestSubagentStallDiagnosis(t *testing.T) {
	mock := mockapi.New(mockapi.Config{
		Script:  []mockapi.Turn{{Text: []string{"never arrives in time"}}},
		Latency: 3 * time.Second, // first byte far beyond the 300ms idle budget
	})
	t.Cleanup(mock.Close)
	t.Setenv("ANTHROPIC_BASE_URL", mock.URL())
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "sub-timeout-test-token")

	// ToolTimeout 150ms → idle budget 300ms.
	sess := subTimeoutSession(t, subTimeoutConfig(t, 150*time.Millisecond), nil)

	start := time.Now()
	_, err := sess.runSubagent(context.Background(), json.RawMessage(
		`{"task":"survey the tea archives for the plan"}`))
	if err == nil {
		t.Fatal("a run silent past the idle budget must be cancelled")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("watchdog waited %s — it must fire around the 300ms budget, not the mock's 3s latency", elapsed)
	}
	for _, want := range []string{
		"no progress",  // the watchdog was the executioner
		"300ms",        // its budget (2×ToolTimeout)
		"tea archives", // the task name
		"api round 1",  // where it stalled
		"elapsed",      // how long it ran
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("diagnosis missing %q:\n%v", want, err)
		}
	}
}

// TestSubagentProgressOutlivesWallBudget: a run that keeps producing
// events must survive past any single fixed budget — the fix for the
// reported "48.9s success vs the 120s ceiling" shape. Same injected 150ms
// ToolTimeout as the stall test, but the mock answers every round inside
// the idle window: five API rounds take longer than the budget in total
// while never going silent, so the run completes untouched.
func TestSubagentProgressOutlivesWallBudget(t *testing.T) {
	script := []mockapi.Turn{
		{Tool: &mockapi.ToolCall{Name: "noop", Input: "{}"}},
		{Tool: &mockapi.ToolCall{Name: "noop", Input: "{}"}},
		{Tool: &mockapi.ToolCall{Name: "noop", Input: "{}"}},
		{Tool: &mockapi.ToolCall{Name: "noop", Input: "{}"}},
		{Text: []string{"finished after several rounds"}},
	}
	mock := mockapi.New(mockapi.Config{Script: script, Latency: 80 * time.Millisecond})
	t.Cleanup(mock.Close)
	t.Setenv("ANTHROPIC_BASE_URL", mock.URL())
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "sub-timeout-test-token")

	reg := tools.NewRegistry()
	if err := tools.RegisterFunc(reg, "noop", "does nothing", true,
		func(ctx context.Context, _ struct{}) (string, error) { return "ok", nil }); err != nil {
		t.Fatal(err)
	}
	// Idle budget 300ms; each round's 80ms latency stays inside it.
	sess := subTimeoutSession(t, subTimeoutConfig(t, 150*time.Millisecond), reg)

	start := time.Now()
	answer, _, iters, err := sess.runNested(context.Background(), "sub-x",
		"multi round progress test", "", "")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("a run that keeps making progress must not be cut: %v", err)
	}
	if !strings.Contains(answer, "finished after several rounds") {
		t.Fatalf("answer = %q", answer)
	}
	if iters != 5 {
		t.Fatalf("API rounds = %d, want 5", iters)
	}
	// Sanity: wall time really did pass the idle budget — the budget reset
	// on progress rather than firing at a fixed second.
	if elapsed < 300*time.Millisecond {
		t.Fatalf("run finished in %s — the test needs wall > 300ms budget to prove progress resets it", elapsed)
	}
}

// TestSubagentWallBudgetDiagnosis: when the CALLER's wall clock fires
// mid-run (the adapter's ToolTimeout, until it stops wrapping the
// subagent tools — see the session.go note in the fix report), the error
// still diagnoses itself: which budget, which task, which round, how
// fresh the last activity was. It must NOT claim "no progress" — the run
// was alive when the clock killed it.
func TestSubagentWallBudgetDiagnosis(t *testing.T) {
	mock := mockapi.New(mockapi.Config{
		Script:  []mockapi.Turn{{Text: []string{"way too slow"}}},
		Latency: 5 * time.Second,
	})
	t.Cleanup(mock.Close)
	t.Setenv("ANTHROPIC_BASE_URL", mock.URL())
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "sub-timeout-test-token")

	// ToolTimeout 500ms → idle budget 1000ms, well beyond the 400ms
	// caller deadline: the wall must win the race so the case selection
	// is deterministic even on a slow CI machine.
	sess := subTimeoutSession(t, subTimeoutConfig(t, 500*time.Millisecond), nil)

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := sess.runSubagent(ctx, json.RawMessage(
		`{"task":"slow gateway research for the plan"}`))
	if err == nil {
		t.Fatal("the caller's deadline must still cut the run")
	}
	elapsed := time.Since(start)
	if elapsed < 300*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("died after %s — want ~400ms (neither early nor the mock's full 5s latency)", elapsed)
	}
	msg := err.Error()
	for _, want := range []string{
		"time budget",   // the wall clock was the executioner
		"slow gateway",  // the task name
		"api round 1",   // which round it died in
		"last activity", // how fresh the run was when killed
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("diagnosis missing %q:\n%v", want, msg)
		}
	}
	if strings.Contains(msg, "no progress") {
		t.Errorf("wall-clock kill must not be misattributed to the watchdog:\n%v", msg)
	}
}

// TestSubagentAdapterWallDiagnosis: through the REAL adapter path (a full
// prompt whose model calls run_subagent), the wall-clock kill arrives as a
// diagnosed tool result the model can act on — task name included — and
// the prompt itself keeps going (the tool error is a result, not a prompt
// failure). Both budgets are acceptable: the wall clock names itself
// before the session.go exemption lands, the idle watchdog ("no
// progress") after it — the task name is the invariant.
func TestSubagentAdapterWallDiagnosis(t *testing.T) {
	mock := mockapi.New(mockapi.Config{
		Script: []mockapi.Turn{
			{Tool: &mockapi.ToolCall{Name: "run_subagent",
				Input: `{"task":"deep research on maple syrup"}`}},
			{Text: []string{"wrapping up"}},
		},
		Latency: time.Second, // nested first byte far beyond the 150ms wall
	})
	t.Cleanup(mock.Close)
	t.Setenv("ANTHROPIC_BASE_URL", mock.URL())
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "sub-timeout-test-token")

	cfg := subTimeoutConfig(t, 150*time.Millisecond)
	cfg.CompactThreshold = -1 // compaction noise would mask the step under test
	sess := subTimeoutSession(t, cfg, nil)

	if err := sess.Submit("research syrup for the plan"); err != nil {
		t.Fatal(err)
	}
	var sawDiagnosis bool
	deadline := time.After(15 * time.Second)
	for !sawDiagnosis {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				t.Fatal("event stream closed before run_subagent reported")
			}
			switch ev.Type {
			case EventToolExecEnd:
				if ev.ToolName == "run_subagent" && ev.IsError {
					if !strings.Contains(ev.Err, "maple syrup") {
						t.Errorf("diagnosis must carry the task name:\n%s", ev.Err)
					}
					if !strings.Contains(ev.Err, "time budget") && !strings.Contains(ev.Err, "no progress") {
						t.Errorf("diagnosis must name the budget that killed it:\n%s", ev.Err)
					}
					sawDiagnosis = true
				}
			case EventPromptEnd:
				if ev.Err != "" {
					t.Fatalf("prompt must survive the tool failure: %s", ev.Err)
				}
				t.Fatal("prompt ended before run_subagent's diagnosed exec end arrived")
			}
		case <-deadline:
			t.Fatal("timeout waiting for the diagnosed run_subagent failure")
		}
	}
}

// TestSubagentToolSurvivesPastToolTimeout: lands WITH the session.go patch
// that exempts the subagent tools from the wall-clock ToolTimeout - red
// before it, green after. The mock's 200ms gap sits beyond the old 150ms
// wall but inside the 300ms idle budget, so only the wall can kill this run.
func TestSubagentToolSurvivesPastToolTimeout(t *testing.T) {
	mock := mockapi.New(mockapi.Config{
		Script:  []mockapi.Turn{{Text: []string{"slow but healthy answer"}}},
		Latency: 200 * time.Millisecond,
	})
	t.Cleanup(mock.Close)
	t.Setenv("ANTHROPIC_BASE_URL", mock.URL())
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "sub-timeout-test-token")

	sess := subTimeoutSession(t, subTimeoutConfig(t, 150*time.Millisecond), nil)
	var tool anthropic.BetaTool
	for _, bt := range sess.betaTools {
		if bt.Name() == "run_subagent" {
			tool = bt
		}
	}
	if tool == nil {
		t.Fatal("run_subagent missing from the session's adapted tool set")
	}
	if _, err := tool.Execute(sess.streamCtx, // carries sessionCtxKey: run_subagent needs its driver

		json.RawMessage(`{"task":"slow but healthy research"}`)); err != nil {
		t.Fatalf("subagent tool must survive past ToolTimeout: %v", err)
	}
}
