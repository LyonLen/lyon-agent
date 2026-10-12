package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestCmdOutArea: command answers render in the temporary area under the
// transcript (with a rule marking the boundary), long answers clamp with
// a "+N more" hint, and the frame invariants hold with the area present —
// including alongside the popup.
func TestCmdOutArea(t *testing.T) {
	m := newTestModel(t)

	m.slash("/help")
	if len(m.cmdOut) == 0 {
		t.Fatal("/help produced no output rows")
	}
	view := m.View()
	if !strings.Contains(view, "── output ──") {
		t.Fatalf("output rule missing:\n%s", view)
	}
	if !strings.Contains(view, "/rollback") {
		t.Fatalf("help body missing from the area:\n%s", view)
	}
	checkView(t, m, "output area frame")

	// The popup shares the frame: its rows shrink the budget, and the
	// frame stays exact.
	m.ti.SetValue("/")
	m.updateSuggestions()
	if len(m.sugs) == 0 {
		t.Fatal("popup did not re-arm")
	}
	checkView(t, m, "output area + popup frame")
	m.ti.SetValue("")
	m.updateSuggestions()

	// Head clamp: 30 rows show 10 plus the overflow count, never more.
	m.cmdOut = nil
	for i := 0; i < 30; i++ {
		m.cmdOut = append(m.cmdOut, "row")
	}
	rows := m.cmdOutView(30)
	if len(rows) > 12 { // rule + cap + hint
		t.Fatalf("clamped rows = %d", len(rows))
	}
	if !strings.Contains(strings.Join(rows, "\n"), "+20 more") {
		t.Fatalf("overflow hint missing:\n%s", strings.Join(rows, "\n"))
	}

	// A terminal too short for area + transcript drops the area — the
	// conversation always wins.
	if got := m.cmdOutView(4); got != nil {
		t.Fatalf("tiny budget must drop the area, got %d rows", len(got))
	}
}

// TestCmdOutClearPoints: the area shows the newest answer alone — slash
// dispatch replaces it, shift+tab feedback replaces rather than stacks,
// and prompt submit / esc dismiss it.
func TestCmdOutClearPoints(t *testing.T) {
	m := newTestModel(t)

	// Slash dispatch clears before answering.
	m.cmdOut = []string{"stale answer"}
	m.slash("/mode")
	if strings.Contains(m.cmdOutText(), "stale answer") {
		t.Fatalf("stale answer survived slash dispatch:\n%s", m.cmdOutText())
	}
	if !strings.Contains(m.cmdOutText(), "mode") {
		t.Fatalf("/mode feedback missing:\n%s", m.cmdOutText())
	}

	// shift+tab replaces, never stacks under, the old answer.
	m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	if strings.Contains(m.cmdOutText(), "mode: default") &&
		!strings.Contains(m.cmdOutText(), "mode →") {
		t.Fatalf("shift+tab stacked under the old answer:\n%s", m.cmdOutText())
	}
	if !strings.Contains(m.cmdOutText(), "mode → plan") {
		t.Fatalf("shift+tab feedback missing:\n%s", m.cmdOutText())
	}

	// esc dismisses the area outright.
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.cmdOut != nil {
		t.Fatalf("esc left %d output rows", len(m.cmdOut))
	}
}
