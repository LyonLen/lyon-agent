package main

import (
	"bytes"
	"io"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/tools"
)

type lockBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockBuf) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }
func (l *lockBuf) String() string              { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }
func (l *lockBuf) Reset()                      { l.mu.Lock(); defer l.mu.Unlock(); l.b.Reset() }

// waitFor polls cond up to d, failing the test on timeout.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestE2ECtrlTExpandsFoldedThinking drives a real bubbletea program through
// its input reader: stream thinking, let it fold behind the text answer,
// then write the ctrl+t byte and require the folded body to be re-rendered.
func TestE2ECtrlTExpandsFoldedThinking(t *testing.T) {
	cfg := agent.DefaultConfig()
	eng := agent.NewEngine(cfg, nil)
	sess := eng.NewSession(nil)
	defer sess.Close()

	m := newTUIModel(cfg, sess, tools.NewMetrics())
	inR, inW := io.Pipe()
	out := &lockBuf{}
	p := tea.NewProgram(m, tea.WithInput(inR), tea.WithOutput(out))
	go func() { _, _ = p.Run() }()
	defer p.Kill()

	p.Send(tuiEv{ev: agent.Event{Type: agent.EventThinkingDelta,
		Thinking: "E2E_THINK_FIRST\nE2E_THINK_SECOND"}})
	p.Send(tuiEv{ev: agent.Event{Type: agent.EventTextDelta, Text: "E2E_ANSWER"}})
	waitFor(t, 3*time.Second, "answer rendered", func() bool {
		return bytes.Contains([]byte(out.String()), []byte("E2E_ANSWER"))
	})

	// The body must be folded away now (only the preview line shows).
	waitFor(t, 2*time.Second, "thinking folded", func() bool {
		return bytes.Contains([]byte(out.String()), []byte("thinking"))
	})
	out.Reset()

	if _, err := inW.Write([]byte{0x14}); err != nil { // ctrl+t
		t.Fatalf("write ctrl+t: %v", err)
	}
	waitFor(t, 3*time.Second, "folded body re-rendered after ctrl+t", func() bool {
		return bytes.Contains([]byte(out.String()), []byte("E2E_THINK_SECOND"))
	})
}
