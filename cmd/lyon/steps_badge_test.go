package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/eval"
)

// stripANSI reduces a rendered row to plain text for assertions.
func stripANSI(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		switch {
		case inEsc:
			if r == 'm' {
				inEsc = false
			}
		case r == '\x1b':
			inEsc = true
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// TestAgentBadges: prompt groups carry the main tier's badge, task nodes
// the subagent tier's — "who did this" for a multi-agent session.
func TestAgentBadges(t *testing.T) {
	m := newTestModel(t)
	m.cfg.Model = "claude-opus-5-5"
	m.cfg.SubagentModel = "claude-haiku-5-5"

	// Prompt group: badge right-aligned on the titled row.
	m.startPromptStep("refactor the session loop")
	if a := m.steps[0].agent; a != "claude-opus-5-5" {
		t.Fatalf("prompt step agent = %q", a)
	}
	row := stripANSI(m.stepLine(m.steps[0], 40))
	if !strings.HasSuffix(strings.TrimRight(row, " "), "[opus-5-5]") {
		t.Fatalf("prompt row lacks right-aligned badge: %q", row)
	}

	// Task node: born with the subagent tier, confirmed by the end event.
	m.applyEvent(agent.Event{
		Type: agent.EventToolUseStart, ToolID: "s1", ToolName: "run_subagent",
		Input: json.RawMessage(`{"task":"survey the router"}`),
	})
	if a := m.steps[1].agent; a != "claude-haiku-5-5" {
		t.Fatalf("task node agent = %q", a)
	}
	m.applyEvent(agent.Event{
		Type: agent.EventSubagentEnd, ToolName: "claude-haiku-5-5",
		Detail: "2 API calls", DurationMS: 400,
	})
	row = stripANSI(m.stepLine(m.steps[1], 40))
	if !strings.Contains(row, "[haiku-5-5]") {
		t.Fatalf("task node row lacks badge: %q", row)
	}

	// shortModel: family prefix carries no information; long ids clip.
	if got := shortModel("claude-opus-5-5"); got != "opus-5-5" {
		t.Fatalf("shortModel = %q", got)
	}
	if got := shortModel("mimo-v2.6-pro-ultraspeed"); len([]rune(got)) > 19 {
		t.Fatalf("shortModel did not clip: %q", got)
	}
}

// TestMemoryPanelRows: injection lands as a born-finished row, and the
// async distill/sync pairs open running and close on their messages.
func TestMemoryPanelRows(t *testing.T) {
	m := newTestModel(t)

	m.applyEvent(agent.Event{
		Type: agent.EventMemoryInjected, Detail: "1024 bytes of hot memory",
	})
	s := m.steps[len(m.steps)-1]
	if s.kind != stepMemory || s.name != "memory.inject" ||
		s.status != stepDone || !strings.Contains(s.detail, "1024 bytes") {
		t.Fatalf("inject row: %+v", s)
	}

	// Distill: opens running, rememberDone closes it.
	n := m.pushAsyncStep(stepMemory, "memory.distill", "extracting durable facts…")
	if st := m.steps[len(m.steps)-1].status; st != stepRunning {
		t.Fatalf("distill row not running: %v", st)
	}
	m.Update(rememberDone{n: 3, step: n})
	if got := m.stepBy(n); got == nil || got.status != stepDone ||
		!strings.Contains(got.detail, "saved 3 fact") {
		t.Fatalf("distill row after done: %+v", got)
	}

	// Sync: same lifecycle, error path records the failure text.
	n = m.pushAsyncStep(stepMemory, "memory.sync", "pushing/pulling cloud…")
	m.Update(memSyncDone{pushed: 1, pulled: 2, step: n})
	if got := m.stepBy(n); got == nil || got.status != stepDone ||
		!strings.Contains(got.detail, "pushed 1, pulled 2") {
		t.Fatalf("sync row after done: %+v", got)
	}
	n = m.pushAsyncStep(stepMemory, "memory.sync", "pushing/pulling cloud…")
	m.Update(memSyncDone{err: errTest("cloud down"), step: n})
	if got := m.stepBy(n); got == nil || got.status != stepError ||
		!strings.Contains(got.err, "cloud down") {
		t.Fatalf("sync row after error: %+v", got)
	}
}

// errTest is a one-line error for assertions.
type errTest string

func (e errTest) Error() string { return string(e) }

// TestDiagnoseLifecycle: a failed prompt's post-mortem opens a running
// stepDiagnose row, diagDone closes it with the conditions, and the
// answer area shows the full post-mortem. The kill switch suppresses
// the kickoff entirely (no goroutine, no row).
func TestDiagnoseLifecycle(t *testing.T) {
	m := newTestModel(t)

	// Kill switch: nothing opens.
	t.Setenv("LYON_DIAGNOSE", "off")
	m.kickDiagnose("tool loop failed")
	if got := m.stepByKind(stepDiagnose); got != nil {
		t.Fatalf("LYON_DIAGNOSE=off must not open a row: %+v", got)
	}

	// Real flow (without the network leg): open a row, close via message.
	t.Setenv("LYON_DIAGNOSE", "")
	step := m.pushAsyncStep(stepDiagnose, "diagnose", "post-mortem of the failed run…")
	m.Update(diagDone{
		step: step,
		diag: eval.Diagnosis{
			FailedConditions: []string{"test file path never updated", "output not verified"},
			Patch:            "update the fixture before claiming done",
		},
	})
	got := m.stepBy(step)
	if got == nil || got.status != stepDone ||
		!strings.Contains(got.detail, "test file path never updated") {
		t.Fatalf("diagnose row after done: %+v", got)
	}
	out := m.cmdOutText()
	if !strings.Contains(out, "错因归纳") || !strings.Contains(out, "update the fixture") {
		t.Fatalf("answer area lacks post-mortem:\n%s", out)
	}

	// Writeback: the post-mortem lands in the audit log (§1.3 record).
	m.sess.Hooks().Flush()
	files, _ := filepath.Glob(filepath.Join(os.Getenv("LYON_HOME"), "audit", "*.jsonl"))
	wroteBack := false
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if strings.Contains(string(data), `"event":"diagnosis"`) {
			wroteBack = true
			break
		}
	}
	if !wroteBack {
		t.Fatalf("diagnosis not written to audit (files: %v)", files)
	}

	// Error leg: the row closes as failed, never dangling.
	step = m.pushAsyncStep(stepDiagnose, "diagnose", "post-mortem of the failed run…")
	m.Update(diagDone{step: step, err: errTest("rate limited")})
	if got = m.stepBy(step); got == nil || got.status != stepError ||
		!strings.Contains(got.err, "rate limited") {
		t.Fatalf("diagnose row after error: %+v", got)
	}
}

// TestDiagnoseTrajectory: the post-mortem input carries the prompt, the
// error and the recent step tail — the panel audit as evidence.
func TestDiagnoseTrajectory(t *testing.T) {
	m := newTestModel(t)
	m.startPromptStep("fix the rollback bug")
	m.applyEvent(agent.Event{
		Type: agent.EventToolUseStart, ToolID: "t1", ToolName: "run_shell",
		Input: json.RawMessage(`{"command":"go test ./..."}`),
	})
	traj := m.diagTrajectory("fix the rollback bug", "3 tests failed")
	for _, want := range []string{
		"fix the rollback bug", "3 tests failed", "run_shell", "go test ./...",
	} {
		if !strings.Contains(traj, want) {
			t.Errorf("trajectory missing %q:\n%s", want, traj)
		}
	}
}
