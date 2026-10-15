package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lyon-agent/pkg/hooks"
	"lyon-agent/pkg/tools"
)

// newOutputSession builds a bare session wired to a temp home, mirroring
// subnest_test.go: events land on a buffered channel the test drains.
func newOutputSession(t *testing.T) *Session {
	t.Helper()
	t.Setenv("LYON_HOME", tempDir(t))
	eng := &Engine{cfg: Config{Workspace: tempDir(t)}, hooks: hooks.New(tempDir(t))}
	t.Cleanup(eng.Close)
	return &Session{eng: eng, ctx: context.Background(), events: make(chan Event, 16)}
}

// TestSessionOnToolOutputEmits: OnToolOutput carries the chunk as one
// EventToolExecOutput, stamped Sub while a nested run is active.
func TestSessionOnToolOutputEmits(t *testing.T) {
	s := newOutputSession(t)

	s.OnToolOutput(tools.ExecEvent{ExecID: 7, Name: "run_shell", Output: "chunk-a\n"})
	ev := <-s.events
	if ev.Type != EventToolExecOutput {
		t.Fatalf("type = %s", ev.Type)
	}
	if ev.ExecID != 7 || ev.Text != "chunk-a\n" || ev.ToolName != "run_shell" {
		t.Fatalf("event = %+v", ev)
	}
	if ev.Sub || ev.SubID != "" {
		t.Fatalf("outside a nested run: sub=%v subID=%q", ev.Sub, ev.SubID)
	}

	atomic.AddInt32(&s.subNest, 1)
	s.OnToolOutput(tools.ExecEvent{ExecID: 8, Name: "run_shell", Output: "chunk-b\n"})
	ev = <-s.events
	if !ev.Sub {
		t.Fatal("inside a nested run the chunk must be stamped Sub")
	}
	if ev.ExecID != 8 || ev.Text != "chunk-b\n" {
		t.Fatalf("event = %+v", ev)
	}
	atomic.AddInt32(&s.subNest, -1)
}

// TestOutputSkipsHooksAudit: live chunks are noise for the audit trail —
// the end event still lands, the output events never do.
func TestOutputSkipsHooksAudit(t *testing.T) {
	s := newOutputSession(t)

	s.OnToolOutput(tools.ExecEvent{ExecID: 1, Name: "run_shell", Output: "noisy\n"})
	<-s.events
	s.OnToolEnd(tools.ExecEvent{ExecID: 1, Name: "run_shell", Duration: time.Second})
	<-s.events

	s.eng.Hooks().Flush()
	auditPath := filepath.Join(os.Getenv("LYON_HOME"), "audit", time.Now().Format("2006-01-02")+".jsonl")
	raw, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("audit file: %v", err)
	}
	audit := string(raw)
	if !strings.Contains(audit, `"event":"tool_exec_end"`) {
		t.Fatalf("end event missing from audit:\n%s", audit)
	}
	if strings.Contains(audit, "tool_exec_output") {
		t.Fatalf("output events must not be audited:\n%s", audit)
	}
}

// TestSubObsOnToolOutputStampsSubID: a fan-out chunk carries its task's
// SubID and forwards to an opted-in external observer; a plain observer
// (no OnToolOutput) is skipped without panicking.
func TestSubObsOnToolOutputStampsSubID(t *testing.T) {
	s := newOutputSession(t)

	var ext []string
	s.extObs = tools.ObserverFuncs{Output: func(e tools.ExecEvent) { ext = append(ext, e.Output) }}
	o := subObs{s: s, id: "task-1"}
	o.OnToolOutput(tools.ExecEvent{ExecID: 3, Name: "run_shell", Output: "fan\n"})

	ev := <-s.events
	if ev.Type != EventToolExecOutput || !ev.Sub || ev.SubID != "task-1" {
		t.Fatalf("event = %+v", ev)
	}
	if len(ext) != 1 || ext[0] != "fan\n" {
		t.Fatalf("extObs got %q", ext)
	}

	// A *Metrics observer implements Observer but not OutputObserver:
	// forwarding must skip, not panic.
	s.extObs = tools.NewMetrics()
	o.OnToolOutput(tools.ExecEvent{ExecID: 4, Name: "run_shell", Output: "skip\n"})
	if ev := <-s.events; ev.ExecID != 4 {
		t.Fatalf("event = %+v", ev)
	}
}
