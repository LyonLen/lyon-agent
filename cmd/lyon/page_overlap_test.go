package main

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestPgUpKeepsOverlap: a page hop moves the viewport by height minus
// pageOverlap lines — the reader keeps pageOverlap lines of context
// instead of a hard cut (#65). Both directions, and the bottom pin
// still releases on the way up.
func TestPgUpKeepsOverlap(t *testing.T) {
	m := newTestModel(t)
	var b strings.Builder
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&b, "line-%03d\n", i)
	}
	m.vp.SetContent(b.String())
	h := m.vp.Height
	if h <= pageOverlap+2 {
		t.Fatalf("viewport height %d too small to page", h)
	}

	m.vp.GotoBottom()
	m.follow = true
	bottom := m.vp.YOffset

	m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	if want := bottom - (h - pageOverlap); m.vp.YOffset != want {
		t.Fatalf("pgup offset = %d, want %d (page minus %d-line overlap)",
			m.vp.YOffset, want, pageOverlap)
	}
	if m.follow {
		t.Fatal("pgup must release the follow pin")
	}

	m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	if want := bottom - (h - pageOverlap) + (h - pageOverlap); m.vp.YOffset != want {
		t.Fatalf("pgdown offset = %d, want %d", m.vp.YOffset, want)
	}

	// Storming pgdown from the bottom clamps — #63's regression guard.
	for i := 0; i < 20; i++ {
		m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	}
	if m.vp.YOffset > bottom {
		t.Fatalf("pgdown storm overshot the bottom: %d > %d", m.vp.YOffset, bottom)
	}
	// And pgup storming from the top clamps at zero.
	for i := 0; i < 40; i++ {
		m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	}
	if m.vp.YOffset < 0 {
		t.Fatalf("pgup storm undershot the top: %d", m.vp.YOffset)
	}
}
