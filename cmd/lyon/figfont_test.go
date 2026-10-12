package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// TestFigRenderLogo: the wordmark renders from the embedded FIGFont as
// uniform-width block rows (the header's frame math depends on it), and the
// font really carries a full alphabet — the reason the logo no longer needs
// hand-typed letterforms.
func TestFigRenderLogo(t *testing.T) {
	rows := lyonLogo
	if len(rows) != 6 {
		t.Fatalf("LYON rows: %d, want 6 (font height minus the empty descender row)", len(rows))
	}
	for i, r := range rows {
		// The feet row of the font is pure ╚═╝ — accept any box-drawing rune.
		if !strings.ContainsAny(r, "█═╔╗╚╝") {
			t.Fatalf("row %d missing box-drawing glyphs: %q", i, r)
		}
		if n := ansi.StringWidth(r); n != 36 {
			t.Fatalf("row %d is %d cells, want 36: %q", i, n, r)
		}
	}

	// Full coverage: every letter and digit must exist as a non-blank
	// glyph — a missing rune would fall back to a blank and pass a naive
	// "render didn't error" check.
	const charset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	for _, ch := range charset {
		glyph, ok := logoFont.glyphs[ch]
		if !ok {
			t.Fatalf("font missing glyph %q", ch)
		}
		if strings.TrimSpace(strings.Join(glyph, "")) == "" {
			t.Fatalf("glyph %q is blank", ch)
		}
	}

	// Rendering pads to one uniform width so header rows stay rectangular.
	rendered := logoFont.render("The quick brown fox jumps over 42 lazy dogs")
	width := ansi.StringWidth(rendered[0])
	for i, r := range rendered {
		if n := ansi.StringWidth(r); n != width {
			t.Fatalf("row %d is %d cells, want uniform %d: %q", i, n, width, r)
		}
	}

	// Unknown runes render as blanks rather than crashing the header.
	if got := logoFont.render("¶"); strings.TrimSpace(strings.Join(got, "")) != "" {
		t.Fatalf("unknown rune must render as blanks, got %q", got)
	}
}
