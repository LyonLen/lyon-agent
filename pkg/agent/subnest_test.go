package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"lyon-agent/pkg/hooks"
	"lyon-agent/pkg/tools"
)

// TestSubNestMarksEvents: exec events emitted while runSubagent is in
// flight carry Sub=true; outside the window (including run_subagent's own
// start/end brackets) they stay at depth 0. Renderers rely on this to
// nest subagent work under its task instead of the main flow.
func TestSubNestMarksEvents(t *testing.T) {
	t.Setenv("LYON_HOME", t.TempDir())
	s := &Session{
		eng:    &Engine{cfg: Config{Workspace: t.TempDir()}, hooks: hooks.New(t.TempDir())},
		ctx:    context.Background(),
		events: make(chan Event, 16),
	}
	ev := tools.ExecEvent{ExecID: 1, Name: "read_file", Start: time.Now()}

	s.OnToolStart(ev)
	if got := <-s.events; got.Sub {
		t.Fatal("main-flow exec start marked Sub")
	}

	atomic.AddInt32(&s.subNest, 1)
	s.OnToolStart(ev)
	s.OnToolEnd(ev)
	a, b := <-s.events, <-s.events
	if !a.Sub || !b.Sub {
		t.Fatalf("nested execs must carry Sub: start=%v end=%v", a.Sub, b.Sub)
	}

	atomic.AddInt32(&s.subNest, -1)
	s.OnToolEnd(ev)
	if got := <-s.events; got.Sub {
		t.Fatal("exec end after the window must not carry Sub")
	}
}
