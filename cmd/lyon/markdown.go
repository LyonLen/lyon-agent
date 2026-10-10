package main

import (
	"regexp"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// markdown-lite rendering for assistant transcript text (lineText only —
// prompts stay literal; tool events and banners are pre-painted lineStyled
// and bypass all of this on purpose).
//
// Block cues are dispatched from renderTranscript's line loop, the one
// place that knows the viewport width and the fence state: pipe tables,
// wrapped list items and ─ rules are emitted pre-aligned through emitRaw
// (ansi.Wrap would shred the grid), while headings and blockquotes fit the
// existing per-line path through styleTextLine. Inline spans are a single
// regex pass in inlineStyle so a construct matched earlier wins inside
// later ones: a backticked [x](y) renders as code, not a link.

// inlineRe matches `code`, [text](url) and **bold**. Alternation order is
// the priority at a given position; left-to-right scanning is what keeps
// code spans literal — their match consumes the text before links see it.
var inlineRe = regexp.MustCompile("`([^`\n]+)`" + `|\[([^\]]+)\]\(([^)\n]+)\)|\*\*([^*\n]+)\*\*`)

// tint wraps s in color/style c, re-asserting c after every nested reset:
// SGR 0 clears everything, so paint() inside paint() would otherwise drop
// the outer style at each inner close.
func tint(c, s string) string {
	if !color || s == "" {
		return s
	}
	return c + strings.ReplaceAll(s, cReset, cReset+c) + cReset
}

// inlineStyle dresses the spans of one raw fragment — a heading's text, a
// blockquote body, a table cell, or a whole plain line.
func inlineStyle(s string) string {
	return inlineRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := inlineRe.FindStringSubmatch(m)
		switch {
		case strings.HasPrefix(m, "`"):
			return paint(cCyan, sub[1])
		case strings.HasPrefix(m, "["):
			// Collapse [text](url): keep both when they differ so the
			// target stays copyable; a self-link needs only the text.
			if sub[2] == sub[3] {
				return paint(cDim, sub[2])
			}
			return inlineStyle(sub[2]) + paint(cDim, " ("+sub[3]+")")
		default:
			return paint(cBold, sub[4])
		}
	})
}

// stripHeading reports an ATX heading and returns its text without the
// marker run. GitHub needs a space after the hashes, so "#include" stays
// prose; a lone "###" stays literal too.
func stripHeading(raw string) (string, bool) {
	t := strings.TrimLeft(raw, " \t")
	i := 0
	for i < len(t) && i < 6 && t[i] == '#' {
		i++
	}
	if i == 0 || i == len(t) || (t[i] != ' ' && t[i] != '\t') {
		return "", false
	}
	return strings.TrimLeft(t[i:], " \t"), true
}

var (
	// hruleRe is a line of --- *** or ___. A setext underline under prose
	// ("Title\n---") matches too — the lite renderer draws the rule
	// instead of promoting the previous line.
	hruleRe = regexp.MustCompile(`^(?:-{3,}|\*{3,}|_{3,})$`)
	// listRe captures indent, marker, spacer and text of a bullet or
	// ordered item. \d{1,3} keeps "2026. foo" prose, and the spacer
	// requirement keeps "--- x" from reading as a bullet.
	listRe    = regexp.MustCompile(`^(\s*)([-*+]|\d{1,3}[.)])(\s+)(.*)$`)
	sepCellRe = regexp.MustCompile(`^:?-+:?$`)
)

func isHRule(raw string) bool { return hruleRe.MatchString(strings.TrimSpace(raw)) }

// isTableLine filters the pipe-run rows worth grouping; renderTable then
// applies the real GFM gate (a separator row in position 1).
func isTableLine(raw string) bool {
	t := strings.TrimSpace(raw)
	return strings.HasPrefix(t, "|") && strings.Count(t, "|") >= 2
}

// splitTableRow trims the outer pipes and whitespace off one row. Cells
// split naively — an escaped \| inside a cell is not handled (the lite
// renderer never promised full CommonMark).
func splitTableRow(raw string) []string {
	t := strings.TrimSpace(raw)
	t = strings.TrimPrefix(t, "|")
	t = strings.TrimSuffix(t, "|")
	cells := strings.Split(t, "|")
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return cells
}

func isSepRow(cells []string) bool {
	if len(cells) == 0 {
		return false
	}
	for _, c := range cells {
		if !sepCellRe.MatchString(c) {
			return false
		}
	}
	return true
}

// renderTable aligns a GFM table (header row, separator row, body rows)
// to column widths measured in display cells, so CJK counts double. A grid
// wider than the viewport shrinks its widest columns first — floor 2 cells
// — and wraps those cells onto extra physical lines, so every reasonable
// table renders as a grid. ok is false only without a separator row or
// when the viewport is too narrow to give every column two cells; the
// caller then falls back to prose with the pipes intact.
func renderTable(rows []string, w int) ([]string, bool) {
	if len(rows) < 2 {
		return nil, false
	}
	cells := make([][]string, len(rows))
	for i, r := range rows {
		cells[i] = splitTableRow(r)
	}
	if !isSepRow(cells[1]) {
		return nil, false
	}
	cols := 0
	for _, r := range cells {
		if len(r) > cols {
			cols = len(r)
		}
	}
	if cols == 0 {
		return nil, false
	}

	// Style first, measure second: widths must reflect the rendered ANSI
	// (backticks and asterisks take no space on screen).
	styled := make([][]string, len(cells))
	for i, r := range cells {
		styled[i] = make([]string, cols)
		for j := 0; j < cols; j++ {
			var c string
			if j < len(r) {
				c = inlineStyle(r[j])
			}
			if i == 0 {
				c = tint(cBold, c)
			}
			styled[i][j] = c
		}
	}
	widths := make([]int, cols)
	for _, r := range styled {
		for j, c := range r {
			if n := ansi.StringWidth(c); n > widths[j] {
				widths[j] = n
			}
		}
	}
	for j := range widths {
		if widths[j] == 0 {
			widths[j] = 1 // keep empty columns from collapsing the gutter
		}
	}
	avail := w - 1 - 3*(cols-1) // leading space + " │ " between columns
	if avail < 2*cols {
		return nil, false // not even two cells per column: give up
	}
	// Shrink the widest column until the grid fits; the widest absorbs the
	// whole deficit per round (floor 2) — cells that got too narrow wrap.
	for widthsSum(widths) > avail {
		j := 0
		for k := range widths {
			if widths[k] > widths[j] {
				j = k
			}
		}
		deficit := widthsSum(widths) - avail
		if widths[j]-deficit >= 2 {
			widths[j] -= deficit
			break
		}
		widths[j] = 2
	}
	gutter := paint(cDim, "│")

	// renderRow emits one logical row as one or more physical lines: each
	// cell wraps to its column width, shorter cells pad with blank lines so
	// the gutters stay aligned row by row.
	renderRow := func(cs []string) []string {
		cellLines := make([][]string, cols)
		lines := 1
		for j := 0; j < cols; j++ {
			var c string
			if j < len(cs) {
				c = cs[j]
			}
			if ansi.StringWidth(c) > widths[j] {
				cellLines[j] = strings.Split(ansi.Wrap(c, widths[j], " "), "\n")
			} else {
				cellLines[j] = []string{c}
			}
			if len(cellLines[j]) > lines {
				lines = len(cellLines[j])
			}
		}
		out := make([]string, lines)
		for row := 0; row < lines; row++ {
			var b strings.Builder
			b.WriteByte(' ')
			for j := 0; j < cols; j++ {
				if j > 0 {
					b.WriteString(" " + gutter + " ")
				}
				var cell string
				if row < len(cellLines[j]) {
					cell = cellLines[j][row]
				}
				b.WriteString(cell)
				if pad := widths[j] - ansi.StringWidth(cell); pad > 0 {
					b.WriteString(strings.Repeat(" ", pad))
				}
			}
			out[row] = b.String()
		}
		return out
	}

	out := make([]string, 0, len(rows)+1)
	out = append(out, renderRow(styled[0])...)
	var u strings.Builder
	u.WriteByte(' ')
	for j, n := range widths {
		if j > 0 {
			u.WriteString(paint(cDim, "─┼─"))
		}
		u.WriteString(strings.Repeat("─", n))
	}
	out = append(out, tint(cDim, u.String()))
	for _, r := range styled[2:] {
		out = append(out, renderRow(r)...)
	}
	return out, true
}

func widthsSum(ws []int) int {
	n := 0
	for _, v := range ws {
		n += v
	}
	return n
}

// listSplit separates a list item into indent, marker, spacer and text.
func listSplit(raw string) (indent, marker, spacer, rest string, ok bool) {
	m := listRe.FindStringSubmatch(raw)
	if m == nil {
		return "", "", "", "", false
	}
	return m[1], m[2], m[3], m[4], true
}

// wrapListItem styles one item and wraps its text so continuation lines
// hang under the text instead of under the bullet.
func wrapListItem(indent, marker, spacer, rest string, w int) string {
	pw := ansi.StringWidth(indent + marker + spacer)
	avail := w - pw
	if avail < 8 {
		avail = 8
	}
	body := ansi.Wrap(inlineStyle(rest), avail, " ")
	lines := strings.Split(body, "\n")
	head := indent + paint(cCyan, marker) + spacer
	for i := range lines {
		if i == 0 {
			lines[i] = head + lines[i]
		} else {
			lines[i] = strings.Repeat(" ", pw) + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}
