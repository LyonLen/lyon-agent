package main

import (
	"encoding/json"
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

// TestPgupStormKeepsFrameSane: the frame-tear bug (#46) — glyphs that
// ansi measures as 1 cell but Windows Terminal's Segoe emoji fallback
// renders as 2 made full-width rows physically wrap, scrolling the
// altscreen one row per flush until the UI shredded. Feed the renderer
// content carrying every wide-risk glyph, hammer pgup/pgdown like the
// user did, and require the geometry invariants (and a glyph-free
// frame) throughout — including the steps panel's own rows.
func TestPgupStormKeepsFrameSane(t *testing.T) {
	m := newTestModel(t)
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	for i := 0; i < 60; i++ {
		m.pushText(lineText, fmt.Sprintf("第%d行 %s ⏸⏹⏺⚒⚠✎⌘★🖼⟳", i, strings.Repeat("很长的中文内容ab ", 12)))
	}
	// A running step puts the panel's status glyph in the frame too.
	m.applyEvent(agent.Event{
		Type: agent.EventToolUseStart, ToolID: "t", ToolName: "run_shell",
		Input: json.RawMessage(`{"command":"sleep 1"}`),
	})
	m.refresh()
	for i := 0; i < 30; i++ {
		m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
		checkView(t, m, fmt.Sprintf("pgup storm %d", i))
	}
	for i := 0; i < 40; i++ { // overshoot the bottom on purpose
		m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
		checkView(t, m, fmt.Sprintf("pgdown storm %d", i))
	}
	frame := m.View()
	for _, r := range "⏸⏹⏺⚒⚠✎⌘★🖼⟳" {
		if strings.ContainsRune(frame, r) {
			t.Errorf("wide-risk glyph %q rendered in the frame — it wraps a full row and scrolls the altscreen", r)
		}
	}
}

// TestIdleTypingKeepsViewportStable: idle typing must not touch the
// transcript viewport — neither its height nor its scroll offset. The
// popup shrinks/grows via View's frame math alone; a second height
// owner inside updateSuggestions made the tail jump on every keystroke
// (#39).
func TestIdleTypingKeepsViewportStable(t *testing.T) {
	m := newTestModel(t)
	for i := 0; i < 50; i++ {
		m.pushText(lineText, fmt.Sprintf("line %d", i))
	}
	m.refresh()
	m.View() // settle: View pins to the bottom and sets the height
	h0, y0 := m.vp.Height, m.vp.YOffset

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	if m.vp.Height != h0 || m.vp.YOffset != y0 {
		t.Fatalf("typing / moved the viewport: h %d→%d y %d→%d", h0, m.vp.Height, y0, m.vp.YOffset)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("st")}) // "/st"
	if m.vp.Height != h0 || m.vp.YOffset != y0 {
		t.Fatalf("typing /st moved the viewport: h %d→%d y %d→%d", h0, m.vp.Height, y0, m.vp.YOffset)
	}
	// Scrolled away from the tail, typing must not yank the view back.
	m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	y1 := m.vp.YOffset
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")}) // "/sta"
	if m.vp.Height != h0 || m.vp.YOffset != y1 {
		t.Fatalf("typing while scrolled jumped: h %d y %d→%d", m.vp.Height, y1, m.vp.YOffset)
	}
}

// TestHeaderBlockGeometry: the pinned header block = identity line +
// wordmark (roomy terminals only) + full-width divider; narrow windows
// drop the logo but keep the rule. The block's row count feeds View's
// frame math, and the logo rows must stay exactly wordmark-width.
func TestHeaderBlockGeometry(t *testing.T) {
	m := newTestModel(t)

	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	head := m.headerRows()
	if len(head) != 8 { // identity + 6 logo rows + rule
		t.Fatalf("roomy header rows: %d, want 8", len(head))
	}
	for i, l := range head[1:7] {
		s := stripColor(l)
		if !strings.ContainsAny(s, "█═╔╗╚╝") {
			t.Fatalf("logo row %d missing box-drawing glyphs: %q", i, s)
		}
		if n := ansi.StringWidth(s); n != 36 {
			t.Fatalf("logo row %d is %d cells, want the wordmark's 36: %q", i, n, s)
		}
	}
	if rule := stripColor(head[7]); rule != strings.Repeat("─", 100) {
		t.Fatalf("rule row: %q", rule)
	}
	checkView(t, m, "roomy header block")

	m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	head = m.headerRows()
	if len(head) != 2 { // identity + rule, logo dropped
		t.Fatalf("narrow header rows: %d, want 2", len(head))
	}
	if rule := stripColor(head[1]); rule != strings.Repeat("─", 40) {
		t.Fatalf("narrow rule row: %q", rule)
	}
	checkView(t, m, "narrow header block")
}
