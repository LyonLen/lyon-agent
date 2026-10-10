package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/tools"
)

// checkView asserts the two geometry invariants the terminal depends on:
// View() is exactly m.height rows (one row too many scrolls the altscreen
// frame and desyncs the renderer — stale rows below, top rows clipped),
// and every row is at most m.width cells (a wider row soft-wraps into a
// second terminal row with the same effect).
func checkView(t *testing.T, m *tuiModel, when string) {
	t.Helper()
	lines := strings.Split(m.View(), "\n")
	if len(lines) != m.height {
		t.Errorf("%s: View is %d rows, want exactly %d", when, len(lines), m.height)
	}
	for i, l := range lines {
		if n := ansi.StringWidth(l); n > m.width {
			t.Errorf("%s: row %d is %d cells, want ≤ %d: %q", when, i, n, m.width, stripColor(l))
		}
	}
}

// TestViewFitsTerminalExactly walks every state the bottom chrome can be
// in — idle, streaming, slash popup, session picker, approval overlay —
// and pages the viewport in each, requiring the frame to stay exactly
// terminal-sized throughout.
func TestViewFitsTerminalExactly(t *testing.T) {
	cases := []struct {
		name  string
		setup func(m *tuiModel)
	}{
		{"fresh", func(m *tuiModel) {}},
		{"long-content", func(m *tuiModel) {
			for i := 0; i < 80; i++ {
				m.pushText(lineText, fmt.Sprintf("第%d行 %s", i, strings.Repeat("很长的中文内容", 30)))
				m.pushStyled(fmt.Sprintf("⚒ tool_use run_shell {\"cmd\":\"%s\"}", strings.Repeat("x", 300)))
			}
			m.refresh()
		}},
		{"suggestions-open", func(m *tuiModel) {
			m.ti.SetValue("/res")
			m.updateSuggestions()
		}},
		{"picker-open", func(m *tuiModel) {
			m.pick = make([]agent.SnapshotRef, 12)
			m.updateSuggestions()
		}},
		{"approval-with-suggestions", func(m *tuiModel) {
			m.ti.SetValue("/")
			m.updateSuggestions()
			m.appr = &approveReq{ev: tools.ExecEvent{Name: "run_shell",
				Input: []byte(`{"command":"` + strings.Repeat("y", 300) + `"}`)}, ch: make(chan bool, 1)}
		}},
		{"approval-alone", func(m *tuiModel) {
			m.appr = &approveReq{ev: tools.ExecEvent{Name: "run_shell", Input: []byte(`{"command":"ls"}`)},
				ch: make(chan bool, 1)}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newTestModel(t)
			c.setup(m)
			checkView(t, m, c.name+" before paging")
			m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
			checkView(t, m, c.name+" after pgup")
			m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
			checkView(t, m, c.name+" after pgdown")
		})
	}
}

// checkEscapes scans rendered output for dangling escape sequences: an ESC
// that never reaches a valid final byte would make the terminal swallow the
// bytes that follow it — rows vanish and every row below lands wrong.
func checkEscapes(t *testing.T, s, when string) {
	t.Helper()
	for i := 0; i < len(s); i++ {
		if s[i] != 0x1b {
			continue
		}
		if i+1 >= len(s) {
			t.Errorf("%s: dangling ESC at end", when)
			return
		}
		switch s[i+1] {
		case '[': // CSI: parameter/intermediate bytes then a final byte @-~
			j := i + 2
			for j < len(s) && (s[j] >= '0' && s[j] <= '9' || s[j] == ';' || s[j] == '?' || s[j] == ' ' || s[j] == ':') {
				j++
			}
			if j < len(s) && s[j] >= 0x40 && s[j] <= 0x7e {
				i = j
				continue
			}
			t.Errorf("%s: truncated CSI at byte %d: %q", when, i, s[i:min(i+12, len(s))])
		case ']': // OSC: terminated by BEL or ST
			j := i + 2
			for j < len(s) && s[j] != 0x07 && !(s[j] == 0x1b && j+1 < len(s) && s[j+1] == 0x5c) {
				j++
			}
			if j < len(s) {
				i = j
				continue
			}
			t.Errorf("%s: unterminated OSC at byte %d", when, i)
		default:
			if s[i+1] >= 0x20 && s[i+1] <= 0x7e { // Fe/Fs escapes are 2 bytes
				i++
				continue
			}
			t.Errorf("%s: malformed escape at byte %d: %q", when, i, s[i:min(i+8, len(s))])
		}
	}
}

// TestRealSnapshotReplayGeometry replays the developer's actual saved
// session (skipped when absent) through the full render path at a couple
// of terminal sizes, paging in between: invariants must hold on real
// content, not just synthetic strings.
func TestRealSnapshotReplayGeometry(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	path := filepath.Join(home, ".lyon", "sessions", "72f2fa928a6b1980a1957bb38a5701dd.json")
	snap, err := agent.LoadSnapshotFrom(path)
	if err != nil || snap == nil {
		t.Skipf("snapshot not available: %v", err)
	}
	for _, size := range []struct{ w, h int }{{100, 30}, {237, 56}, {40, 12}} {
		m := newTestModel(t)
		m.Update(tea.WindowSizeMsg{Width: size.w, Height: size.h})
		m.replay(snap)
		checkView(t, m, fmt.Sprintf("replay %dx%d", size.w, size.h))
		checkEscapes(t, m.renderTranscript(), fmt.Sprintf("replay %dx%d", size.w, size.h))
		m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
		checkView(t, m, fmt.Sprintf("replay %dx%d after pgup", size.w, size.h))
		checkEscapes(t, m.renderTranscript(), fmt.Sprintf("replay %dx%d after pgup", size.w, size.h))
		m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
		checkView(t, m, fmt.Sprintf("replay %dx%d after pgdown", size.w, size.h))
	}
}
