package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"lyon-agent/pkg/agent"
)

// TestKeyHintsSpellModifiers: every key-hint surface spells the modifier
// chords out — shift+alt+↑↓, alt+enter, alt+←→ — because the old
// "enter open · ←→ width" footers read as the BARE send/cursor keys and
// misled (#6). Both panel footers must also fit the default panel width
// whole (no ellipsis); that is why panelWidth is 52 and not 40. The
// composed column parks whichever footer at its very bottom row (#1).
func TestKeyHintsSpellModifiers(t *testing.T) {
	// The steps footer, checked at the default width.
	footer := stepsFooter(panelWidth)
	if n := ansi.StringWidth(footer); n > panelWidth {
		t.Fatalf("steps footer is %d cells, panel is %d: %q", n, panelWidth, footer)
	}
	plain := stripColor(footer)
	if strings.Contains(plain, "…") {
		t.Fatalf("steps footer truncated at panelWidth=%d: %q", panelWidth, plain)
	}
	for _, want := range []string{"alt+↑↓", "alt+enter", "alt+←→"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("steps footer missing %q: %q", want, plain)
		}
	}
	// The regression itself: a bare key after the separator means the
	// modifier dropped out again.
	for _, bare := range []string{" · enter open", " · ←→"} {
		if strings.Contains(plain, bare) {
			t.Fatalf("steps footer shows bare %q: %q", bare, plain)
		}
	}

	// The evolution footer: same width contract, resize chord spelled.
	efooter := evoFooter(panelWidth)
	if strings.Contains(stripColor(efooter), "…") {
		t.Fatalf("evolve footer truncated at panelWidth=%d: %q", panelWidth, efooter)
	}
	if !strings.Contains(stripColor(efooter), "alt+←→") {
		t.Fatalf("evolve footer missing alt+←→: %q", efooter)
	}
	if strings.Contains(stripColor(efooter), " · ←→") {
		t.Fatalf("evolve footer shows bare ←→: %q", efooter)
	}

	// Resident idle legend: the same chords in both languages, plus the
	// live-output band's toggle (#13).
	for _, legend := range []string{idleLegendEN, idleLegendZH} {
		for _, want := range []string{"shift+alt+↑↓", "alt+enter", "alt+o", "alt+←→"} {
			if !strings.Contains(legend, want) {
				t.Fatalf("idle legend missing %q: %q", want, legend)
			}
		}
	}

	// /help's steps line: the same chords in both languages.
	orig := uiLang
	t.Cleanup(func() { // other tests in this package assume English
		uiLang = orig
		refreshBakedTexts()
	})
	for _, lang := range []string{"zh", "en"} {
		setLang(lang) // false only when already active — the assert below holds either way
		if uiLang != lang {
			t.Fatalf("uiLang = %q after setLang(%q)", uiLang, lang)
		}
		help := strings.Join(helpLines(), "\n")
		for _, want := range []string{"shift+alt+↑↓", "alt+enter", "alt+o", "alt+←→"} {
			if !strings.Contains(help, want) {
				t.Fatalf("help (%s) missing %q:\n%s", lang, want, help)
			}
		}
	}

	// Composed column: whichever panel is open, its footer parks on the
	// very bottom row — the plan block sits directly above it (#1).
	m := newTestModel(t)
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	col := rightColumnRows(m, 20)
	if len(col) != 20 {
		t.Fatalf("composed column rows = %d, want 20", len(col))
	}
	if last := col[len(col)-1]; !strings.Contains(last, "alt+enter") {
		t.Fatalf("steps footer not at the column bottom: %q", last)
	}
	m.evoOn = true
	if last := rightColumnRows(m, 20)[19]; !strings.Contains(last, "/exam") {
		t.Fatalf("evo footer not at the column bottom: %q", last)
	}

	// The live-output band (#13) hints its own chord on its header row —
	// the shared footer stays exactly as #6 pins it, so "alt+o" rides the
	// band: spelled whole, no ellipsis at the default panel width, in
	// both states.
	band := newTestModel(t)
	band.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	band.applyEvent(agent.Event{Type: agent.EventToolUseStart, ToolID: "t1", ToolName: "run_shell"})
	band.applyEvent(agent.Event{Type: agent.EventToolExecStart, ExecID: 1, ToolName: "run_shell"})
	band.applyEvent(agent.Event{Type: agent.EventToolExecOutput, ExecID: 1, Text: "ok pkg/a\n"})
	bandRow := func(open bool) string {
		band.stepOutOpen = open
		for _, r := range rightColumnRows(band, 20) {
			if strings.Contains(r, "live output") {
				return r
			}
		}
		return ""
	}
	for _, st := range []struct {
		open bool
		want string
	}{{false, "alt+o expand"}, {true, "alt+o fold"}} {
		row := bandRow(st.open)
		if row == "" {
			t.Fatalf("live-output band missing (open=%v)", st.open)
		}
		if !strings.Contains(row, st.want) {
			t.Fatalf("band row %q missing hint %q", row, st.want)
		}
		if strings.Contains(row, "…") {
			t.Fatalf("band hint truncated at panelWidth=%d: %q", panelWidth, row)
		}
		if n := ansi.StringWidth(row); n > panelWidth {
			t.Fatalf("band row is %d cells, panel is %d: %q", n, panelWidth, row)
		}
	}
}
