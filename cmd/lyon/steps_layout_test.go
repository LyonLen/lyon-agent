package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"lyon-agent/pkg/agent"
)

// seedBandPanel builds a wide-terminal model whose right column stacks
// the three #13 segments: a finished step that streamed a two-line tail,
// a still-running step with a fresher line, and a two-item plan.
func seedBandPanel(t *testing.T) *tuiModel {
	t.Helper()
	m := newTestModel(t)
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	m.applyEvent(agent.Event{
		Type: agent.EventToolUseStart, ToolID: "t1", ToolName: "run_shell",
		Input: json.RawMessage(`{"command":"go test ./..."}`),
	})
	m.applyEvent(agent.Event{Type: agent.EventToolExecStart, ExecID: 1, ToolName: "run_shell"})
	m.applyEvent(agent.Event{Type: agent.EventToolExecOutput, ExecID: 1, Text: "ok pkg/a\nFAIL pkg/b\n"})
	m.applyEvent(agent.Event{Type: agent.EventToolExecEnd, ExecID: 1, ToolName: "run_shell", DurationMS: 12})
	m.applyEvent(agent.Event{
		Type: agent.EventToolUseStart, ToolID: "t2", ToolName: "run_shell",
		Input: json.RawMessage(`{"command":"go vet ./..."}`),
	})
	m.applyEvent(agent.Event{Type: agent.EventToolExecStart, ExecID: 2, ToolName: "run_shell"})
	m.applyEvent(agent.Event{Type: agent.EventToolExecOutput, ExecID: 2, Text: "vet pass\n"})
	m.setTodos(parseTodos(json.RawMessage(todoPlan1)))
	return m
}

// bandSpan locates the live-output band in a composed right column: the
// header row index and the plan block's first row (-1 when absent).
func bandSpan(col []string) (band, plan int) {
	band, plan = -1, -1
	for i, r := range col {
		if band < 0 && strings.Contains(r, "live output") {
			band = i
		}
		if plan < 0 && strings.Contains(r, "📋") {
			plan = i
		}
	}
	return band, plan
}

// TestRightColumnThreeSegments: the #13 stack — steps panel on top, the
// live-output band in the middle, the resident plan block above the
// shared footer — composes to exactly vh rows in order, and alt+o grows
// the band from its one-line summary to the tail window without moving
// the other segments' seats.
func TestRightColumnThreeSegments(t *testing.T) {
	m := seedBandPanel(t)
	const vh = 24
	col := rightColumnRows(m, vh)
	if len(col) != vh {
		t.Fatalf("composed column rows = %d, want %d", len(col), vh)
	}
	if !strings.Contains(col[0], "steps") {
		t.Fatalf("steps header lost the top seat: %q", col[0])
	}
	band, plan := bandSpan(col)
	if band < 1 || plan <= band || plan >= vh-1 {
		t.Fatalf("segment order: steps@0 band@%d plan@%d footer@%d", band, plan, vh-1)
	}
	if last := col[vh-1]; !strings.Contains(last, "alt+enter") {
		t.Fatalf("footer not at the column bottom: %q", last)
	}
	// Collapsed: the band is exactly the summary row — live target is the
	// running step (#2), with its line count and the spelled-out chord.
	if plan != band+1 {
		t.Fatalf("collapsed band = %d rows, want 1", plan-band)
	}
	head := col[band]
	for _, want := range []string{"#2", "1 line", "alt+o expand"} {
		if !strings.Contains(head, want) {
			t.Fatalf("collapsed header missing %q: %q", want, head)
		}
	}

	// alt+o expands: header (fold hint) + the step's streamed line; the
	// steps panel yields one row, plan and footer hold their seats.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o"), Alt: true})
	if !m.stepOutOpen {
		t.Fatal("alt+o did not expand the band")
	}
	col = rightColumnRows(m, vh)
	if len(col) != vh {
		t.Fatalf("expanded column rows = %d, want %d", len(col), vh)
	}
	band, plan = bandSpan(col)
	if plan != band+2 {
		t.Fatalf("expanded band = %d rows, want 2 (header + line)", plan-band)
	}
	if !strings.Contains(col[band], "alt+o fold") {
		t.Fatalf("expanded header missing fold hint: %q", col[band])
	}
	if !strings.Contains(col[band+1], "vet pass") {
		t.Fatalf("streamed line missing from the band: %q", col[band+1])
	}
	if !strings.Contains(col[0], "steps") || !strings.Contains(col[vh-1], "alt+enter") {
		t.Fatalf("expansion evicted a seat: top=%q bottom=%q", col[0], col[vh-1])
	}

	// alt+o again collapses back to the one-line summary.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o"), Alt: true})
	if m.stepOutOpen {
		t.Fatal("second alt+o did not collapse the band")
	}
	if b, p := bandSpan(rightColumnRows(m, vh)); p != b+1 {
		t.Fatalf("recollapsed band = %d rows, want 1", p-b)
	}
}

// TestStepOutBandFollowsSelection: the band reads the selected step when
// one is selected (@#n marker, that step's tail), and the live pick —
// newest running step first — while following the tail.
func TestStepOutBandFollowsSelection(t *testing.T) {
	m := seedBandPanel(t)
	col := rightColumnRows(m, 24)
	band, _ := bandSpan(col)
	if !strings.Contains(col[band], "#2") || strings.Contains(col[band], "@") {
		t.Fatalf("live target header = %q, want #2 without @", col[band])
	}

	// First alt+up selects the newest (still #2, now marked); the next
	// walk lands on the finished step and the band follows with ITS tail.
	m.Update(tea.KeyMsg{Type: tea.KeyUp, Alt: true})
	col = rightColumnRows(m, 24)
	band, _ = bandSpan(col)
	if !strings.Contains(col[band], "@#2") {
		t.Fatalf("selected header = %q, want @#2", col[band])
	}
	m.Update(tea.KeyMsg{Type: tea.KeyUp, Alt: true})
	m.stepOutOpen = true
	col = rightColumnRows(m, 24)
	band, _ = bandSpan(col)
	if !strings.Contains(col[band], "@#1") {
		t.Fatalf("selected header = %q, want @#1", col[band])
	}
	if !strings.Contains(col[band], "2 lines") {
		t.Fatalf("selected step's line count missing: %q", col[band])
	}
	joined := strings.Join(col[band:], "\n")
	if !strings.Contains(joined, "FAIL pkg/b") {
		t.Fatalf("selected step's tail missing from the band:\n%s", joined)
	}
}

// TestStepOutBandDegradation: a short column never panics and never loses
// the footer; the band and the steps header yield rows before it does,
// and an expanded band still leaves the steps panel its header row.
func TestStepOutBandDegradation(t *testing.T) {
	m := seedBandPanel(t)
	m.stepOutOpen = true
	for vh := 1; vh <= 8; vh++ {
		col := rightColumnRows(m, vh)
		if len(col) != vh {
			t.Fatalf("vh=%d: composed rows = %d", vh, len(col))
		}
		if last := col[vh-1]; !strings.Contains(last, "alt+enter") {
			t.Fatalf("vh=%d: footer lost: %q", vh, last)
		}
	}
	// vh=8 (expanded): all three segments still up — steps header first.
	col := rightColumnRows(m, 8)
	if !strings.Contains(col[0], "steps") {
		t.Fatalf("vh=8: steps header gone: %q", col[0])
	}
	band, plan := bandSpan(col)
	if band < 1 || plan <= band || plan >= 7 {
		t.Fatalf("vh=8: segments missing: band@%d plan@%d", band, plan)
	}

	// A fat tail: the band windows the newest lines with the cut marker
	// on top, capped by the section budget — steps and plan keep seats.
	for i := 1; i <= 40; i++ {
		m.stepAppendOutput(2, fmt.Sprintf("out-%02d\n", i))
	}
	col = rightColumnRows(m, 8)
	band, plan = bandSpan(col)
	rows := col[band:plan]
	if len(rows) < 3 || !strings.Contains(rows[1], "…(-") {
		t.Fatalf("cut marker missing from the band:\n%s", strings.Join(rows, "\n"))
	}
	if !strings.Contains(rows[len(rows)-1], "out-40") {
		t.Fatalf("newest line must keep the band bottom: %q", rows[len(rows)-1])
	}
	joined := strings.Join(rows, "\n")
	if strings.Contains(joined, "out-01") || strings.Contains(joined, "vet pass") {
		t.Fatalf("older lines should be cut:\n%s", joined)
	}
	if !strings.Contains(col[0], "steps") {
		t.Fatalf("fat tail evicted the steps header: %q", col[0])
	}
	if last := col[7]; !strings.Contains(last, "alt+enter") {
		t.Fatalf("fat tail evicted the footer: %q", last)
	}
}

// TestRenderStepOutContract: the band honors the panel render contract —
// exactly h rows of at most w cells at every size and state, with the
// meta header pinned and blanks as filler, like renderSteps/renderPlan.
func TestRenderStepOutContract(t *testing.T) {
	m := seedBandPanel(t)
	for _, tc := range []struct {
		w, h int
		open bool
	}{{52, 6, false}, {52, 6, true}, {28, 12, true}, {52, 1, true}, {28, 2, false}} {
		m.stepOutOpen = tc.open
		rows := m.renderStepOut(tc.w, tc.h)
		if len(rows) != tc.h {
			t.Fatalf("renderStepOut(%d,%d,%v): %d rows, want %d",
				tc.w, tc.h, tc.open, len(rows), tc.h)
		}
		for i, r := range rows {
			if r == "" {
				continue // filler per the renderSteps contract
			}
			if n := ansi.StringWidth(r); n > tc.w {
				t.Fatalf("renderStepOut(%d,%d): row %d is %d cells: %q",
					tc.w, tc.h, i, n, stripColor(r))
			}
		}
		if !strings.Contains(stripColor(rows[0]), "live output") {
			t.Fatalf("header row: %q", rows[0])
		}
	}
	// No steps at all: the band has no subject and composeSplit gives it
	// zero rows — the seat only exists once there is an audit to read.
	fresh := newTestModel(t)
	fresh.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	if h := fresh.stepOutSectionH(52, 20); h != 0 {
		t.Fatalf("band on a fresh panel = %d rows, want 0", h)
	}
}

// TestStepOutBandKeyContract: alt+o drives the band only while the steps
// panel owns the column — panel off or evolution board up, the chord is
// inert, and an empty panel never flips state it cannot render.
func TestStepOutBandKeyContract(t *testing.T) {
	m := seedBandPanel(t)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o"), Alt: true})
	if !m.stepOutOpen {
		t.Fatal("alt+o dead while the panel is on")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o"), Alt: true})
	if m.stepOutOpen {
		t.Fatal("second alt+o did not collapse")
	}

	m.slash("/steps") // panel off
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o"), Alt: true})
	if m.stepOutOpen {
		t.Fatal("alt+o moved state with the panel off")
	}
	m.slash("/steps")

	m.evoOn = true // evolution board owns the column
	m.relayout()
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o"), Alt: true})
	if m.stepOutOpen {
		t.Fatal("alt+o moved state under the evolution panel")
	}
	m.evoOn = false
	m.relayout()

	// Empty panel: the chord stays a no-op — it never lies about state.
	fresh := newTestModel(t)
	fresh.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	fresh.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o"), Alt: true})
	if fresh.stepOutOpen {
		t.Fatal("alt+o expanded a band with nothing to show")
	}
}

// TestStepOutBandFullFrame: with the band present (expanded, plan in
// tow) the frame keeps its exact-geometry invariants down to the heights
// where the column collapses segment by segment.
func TestStepOutBandFullFrame(t *testing.T) {
	m := seedBandPanel(t)
	m.stepOutOpen = true
	for _, size := range []struct{ w, h int }{{140, 12}, {140, 14}, {140, 20}, {140, 40}} {
		m.Update(tea.WindowSizeMsg{Width: size.w, Height: size.h})
		checkView(t, m, fmt.Sprintf("live-output band %dx%d", size.w, size.h))
	}
}
