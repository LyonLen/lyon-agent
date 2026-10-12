package main

// figfont: a minimal FIGFont renderer for the header wordmark.
//
// The logo used to be a hand-typed block-letter const — easy to misalign and
// impossible to render anything beyond "LYON". It is now rendered from an
// embedded FIGFont with a full alphabet (A–Z, a–z, 0–9), so every letterform
// comes from one vetted typeface and any display text can share it.
//
// Font: "ANSI Shadow" (flf2a, height 7) from the figlet fonts collection —
// created with patorjk.com's figfont editor after TheDrawFonts. Only the
// format subset FIGFont provides is implemented: header, comment lines,
// fixed-width glyph rows with endmarks, hardblank substitution. No smushing
// — ANSI Shadow is a full-width font, plain concatenation is its intent.

import (
	_ "embed"
	"strconv"
	"strings"
	"unicode/utf8"
)

//go:embed fonts/ansi-shadow.flf
var figFontRaw string

// figFont is a parsed FIGFont: glyph rows keyed by rune, plus the hardblank
// that must be blanked out at render time.
type figFont struct {
	hardblank string
	height    int
	glyphs    map[rune][]string
}

// logoFont is the parsed embedded font, built once at init (parsing 12KB of
// glyph data per frame would be silly).
var logoFont = parseFIGFont(figFontRaw)

// parseFIGFont parses flf2a data. Malformed input degrades to an empty font
// (render then falls back to blanks) rather than panicking — the header must
// never take the TUI down.
func parseFIGFont(raw string) *figFont {
	f := &figFont{glyphs: make(map[rune][]string), height: 1}
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "flf2a") {
		return f
	}
	head := strings.Fields(lines[0])
	// Signature line: flf2a<hardblank> height baseline max_len old_layout
	// comment_lines … — the hardblank rides on the end of the signature.
	if len(head) > 0 && len(head[0]) > len("flf2a") {
		f.hardblank = head[0][len("flf2a"):]
	}
	comments := 0
	if len(head) > 1 {
		if h, err := strconv.Atoi(head[1]); err == nil && h > 0 {
			f.height = h
		}
	}
	if len(head) > 5 {
		comments, _ = strconv.Atoi(head[5])
	}

	// Glyphs follow the header comment, one block of `height` lines per
	// character starting at codepoint 32 (space).
	idx := 1 + comments
	var mark byte // endmark: last char of the first glyph line
	for code := rune(32); code < 256 && idx < len(lines); code++ {
		rows := make([]string, 0, f.height)
		for r := 0; r < f.height && idx+r < len(lines); r++ {
			line := lines[idx+r]
			if r == 0 && mark == 0 && line != "" {
				mark = line[len(line)-1]
			}
			line = trimEndmark(line, mark)
			if f.hardblank != "" {
				line = strings.ReplaceAll(line, f.hardblank, " ")
			}
			rows = append(rows, line)
		}
		idx += f.height
		if len(rows) == f.height {
			f.glyphs[code] = rows
		}
	}
	return f
}

// trimEndmark drops the FLF line endmark: the last character, doubled when
// the line's final two characters match (e.g. a blank bottom row ends "@@").
func trimEndmark(line string, mark byte) string {
	if mark == 0 || line == "" || line[len(line)-1] != mark {
		return line
	}
	if len(line) >= 2 && line[len(line)-2] == mark {
		return line[:len(line)-2]
	}
	return line[:len(line)-1]
}

// figRender lays text out in the font: one string per glyph row, trailing
// blank rows dropped, every row padded to the same width so the header's
// frame math sees uniform rows (an unknown rune renders as a blank).
func (f *figFont) render(text string) []string {
	if f.height < 1 {
		return nil
	}
	rows := make([]string, f.height)
	for _, ch := range text {
		glyph, ok := f.glyphs[ch]
		if !ok {
			glyph = f.glyphs[' ']
		}
		for i := range rows {
			if i < len(glyph) {
				rows[i] += glyph[i]
			}
		}
	}
	// Drop rows the text doesn't use (LYON has no descenders: the 7th row
	// of the font is empty), then pad all rows to the widest one. Width is
	// counted in runes — font glyphs are single-width box characters.
	n := len(rows)
	for n > 0 && strings.TrimRight(rows[n-1], " ") == "" {
		n--
	}
	rows = rows[:n]
	width := 0
	for _, r := range rows {
		if w := utf8.RuneCountInString(r); w > width {
			width = w
		}
	}
	for i, r := range rows {
		if pad := width - utf8.RuneCountInString(r); pad > 0 {
			rows[i] = r + strings.Repeat(" ", pad)
		}
	}
	return rows
}
