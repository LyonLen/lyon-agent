package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"lyon-agent/pkg/agent"
)

// TestKeyHintsSpellModifiers: every key-hint surface spells the modifier
// chords out — shift+alt+↑↓, alt+enter, alt+←→ — because the old
// "enter open · ←→ width" footers read as the BARE send/cursor keys and
// misled (#6). Both panel footers must also fit the default panel width
// whole (no ellipsis); that is why panelWidth is 52 and not 40.
func TestKeyHintsSpellModifiers(t *testing.T) {
	m := newTestModel(t)
	m.applyEvent(agent.Event{
		Type: agent.EventToolUseStart, ToolID: "t1", ToolName: "read_file",
		Input: json.RawMessage(`{"path":"a.txt"}`),
	})

	// Steps footer: full text at the default width, every chord spelled.
	rows := m.renderSteps(panelWidth, 10)
	raw := rows[len(rows)-1]
	if n := ansi.StringWidth(raw); n > panelWidth {
		t.Fatalf("footer is %d cells, panel is %d: %q", n, panelWidth, raw)
	}
	footer := stripColor(raw)
	if strings.Contains(footer, "…") {
		t.Fatalf("steps footer truncated at panelWidth=%d: %q", panelWidth, footer)
	}
	for _, want := range []string{"alt+↑↓", "alt+enter", "alt+←→"} {
		if !strings.Contains(footer, want) {
			t.Fatalf("steps footer missing %q: %q", want, footer)
		}
	}
	// The regression itself: a bare key after the separator means the
	// modifier dropped out again.
	for _, bare := range []string{" · enter open", " · ←→"} {
		if strings.Contains(footer, bare) {
			t.Fatalf("steps footer shows bare %q: %q", bare, footer)
		}
	}

	// Resident idle legend: the same chords in both languages.
	for _, legend := range []string{idleLegendEN, idleLegendZH} {
		for _, want := range []string{"shift+alt+↑↓", "alt+enter", "alt+←→"} {
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
		for _, want := range []string{"shift+alt+↑↓", "alt+enter", "alt+←→"} {
			if !strings.Contains(help, want) {
				t.Fatalf("help (%s) missing %q:\n%s", lang, want, help)
			}
		}
	}

	// Evolve panel footer: the resize chord spelled, fits the width whole.
	erows := m.renderEvolve(panelWidth, 12)
	eraw := erows[len(erows)-1]
	efooter := stripColor(eraw)
	if strings.Contains(efooter, "…") {
		t.Fatalf("evolve footer truncated at panelWidth=%d: %q", panelWidth, efooter)
	}
	if !strings.Contains(efooter, "alt+←→") {
		t.Fatalf("evolve footer missing alt+←→: %q", efooter)
	}
	if strings.Contains(efooter, " · ←→") {
		t.Fatalf("evolve footer shows bare ←→: %q", efooter)
	}
}
