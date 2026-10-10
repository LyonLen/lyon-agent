package main

import (
	"regexp"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// colorRe strips SGR sequences so assertions can look at layout alone.
var colorRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripColor(s string) string { return colorRe.ReplaceAllString(s, "") }

// TestMarkdownInlineAndBlockCues: headings lose their # markers, quotes
// gain the │ gutter, and inline spans resolve with code beating links
// beating bold inside a single line.
func TestMarkdownInlineAndBlockCues(t *testing.T) {
	cases := []struct{ in, want string }{
		{"## 形态", "形态"},
		{"#include <x>", "#include <x>"}, // needs a space: not a heading
		{"###", "###"},                   // no text after hashes: stays literal
		{"> quoted text", "│ quoted text"},
		{"**核心** 点", "核心 点"},
		{"use `os.Exit` now", "use os.Exit now"},
		{"see [docs](https://x.io)", "see docs (https://x.io)"},
		{"self [here](here)", "self here"},
		{"`[a](b)` stays", "[a](b) stays"}, // code span wins over the link
	}
	for _, c := range cases {
		if got := stripColor(styleTextLine(c.in)); got != c.want {
			t.Errorf("styleTextLine(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestMarkdownTableAlignsCJK: a valid GFM table renders as an aligned grid
// with no raw pipes, a ─┼─ underline under the header, and both columns
// starting at the same display offset across CJK and ASCII cells.
func TestMarkdownTableAlignsCJK(t *testing.T) {
	rows := []string{
		"| 包 | 职责 |",
		"| --- | --- |",
		"| pkg/agent | 引擎核心 |",
		"| pkg/tools | shell/files/image |",
	}
	out, ok := renderTable(rows, 100)
	if !ok {
		t.Fatal("renderTable rejected a valid table")
	}
	if len(out) != 4 { // header, underline, two body rows
		t.Fatalf("rendered %d lines, want 4", len(out))
	}
	if !strings.Contains(out[1], "─┼─") {
		t.Fatalf("header underline missing: %q", out[1])
	}
	plain := make([]string, len(out))
	for i, l := range out {
		plain[i] = stripColor(l)
		if strings.Contains(plain[i], "|") {
			t.Fatalf("raw pipe survived rendering: %q", plain[i])
		}
	}
	// Every row's second column must start at the same display offset.
	var want int
	for n, cell := range []string{"职责", "引擎核心", "shell/files/image"} {
		var row string
		for _, l := range plain {
			if strings.Contains(l, cell) {
				row = l
				break
			}
		}
		if row == "" {
			t.Fatalf("no row contains %q:\n%q", cell, plain)
		}
		got := ansi.StringWidth(row[:strings.Index(row, cell)])
		if n == 0 {
			want = got
		} else if got != want {
			t.Errorf("column 2 offset for %q = %d, want %d (row %q)", cell, got, want, row)
		}
	}
}

// TestMarkdownTableFallbacks: without a separator row, with a single row,
// or narrower than two cells per column, renderTable declines — the caller
// then renders raw pipes instead of a broken grid. Width alone is not a
// reason: overwide tables shrink and wrap (TestMarkdownTableWrapsOverwide).
func TestMarkdownTableFallbacks(t *testing.T) {
	valid := []string{"| a | b |", "|---|---|", "| 1 | 2 |"}
	if _, ok := renderTable(valid, 100); !ok {
		t.Error("valid table rejected")
	}
	if _, ok := renderTable(valid, 5); ok {
		t.Error("a table that cannot give every column two cells should fall back")
	}
	if _, ok := renderTable([]string{"| a | b |", "| 1 | 2 |"}, 100); ok {
		t.Error("table without separator row should fall back")
	}
	if _, ok := renderTable([]string{"| only |"}, 100); ok {
		t.Error("a single row is not a table")
	}
}

// TestMarkdownTableWrapsOverwide: a table whose cells far exceed the
// viewport still renders as a grid — the widest columns shrink and their
// cells wrap onto extra physical lines, every output row within the width,
// pipes gone, content intact.
func TestMarkdownTableWrapsOverwide(t *testing.T) {
	big := strings.Repeat("很长的单元格内容", 40) // 600+ cells, no spaces
	rows := []string{
		"| 字段 | " + big + " |",
		"| --- | --- |",
		"| a | " + big + " |",
	}
	out, ok := renderTable(rows, 100)
	if !ok {
		t.Fatal("overwide table fell back instead of wrapping")
	}
	if len(out) <= 4 { // header wraps to several lines: must exceed one-per-row
		t.Fatalf("expected wrapped multi-line rows, got %d lines", len(out))
	}
	for i, l := range out {
		if n := ansi.StringWidth(l); n > 100 {
			t.Errorf("row %d is %d cells, want ≤ 100: %q", i, n, stripColor(l))
		}
		if strings.Contains(stripColor(l), "|") {
			t.Errorf("raw pipe in rendered row %d: %q", i, stripColor(l))
		}
	}
	joined := stripColor(strings.Join(out, ""))
	for _, want := range []string{"字段", "a", "很长的单元格内容"} {
		if !strings.Contains(joined, want) {
			t.Errorf("content %q lost while wrapping", want)
		}
	}
	if !strings.Contains(stripColor(strings.Join(out, "\n")), "─┼─") {
		t.Error("header underline missing")
	}
}

// TestSanitizeText: escape sequences, carriage returns and tabs are
// stripped from untrusted content while newlines survive — a dangling CSI
// inside a rendered frame would otherwise swallow the renderer's line
// separators and tear the layout.
func TestSanitizeText(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain 文本", "plain 文本"},
		{"a\x1b[36mb\x1b[0mc", "abc"},  // SGR stripped whole
		{"cut\x1b[…", "cut"},           // dangling CSI eats its tail
		{"x\x1b]0;title\x07y", "xy"},   // OSC swallowed
		{"h\x1b", "h"},                 // lone ESC
		{"a\r\nb", "a\nb"},             // CRLF → LF
		{"spin\r", "spin"},             // progress bar CR
		{"a\tb", "a    b"},             // tab → spaces
		{"keep\nlines", "keep\nlines"}, // newlines stay
		{"\x00\x01ctl", "ctl"},         // stray C0
	}
	for _, c := range cases {
		if got := sanitizeText(c.in); got != c.want {
			t.Errorf("sanitizeText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := sanitizeText(sanitizeText("a\x1b[2mb")); got != "ab" {
		t.Errorf("sanitizeText not idempotent: %q", got)
	}
}

// TestTranscriptBlockRendering: the full path through renderTranscript —
// heading markers stripped, tables gridded, rules drawn, quotes gated,
// while a fenced block keeps its content literal even when it contains
// what would otherwise be a rule.
func TestTranscriptBlockRendering(t *testing.T) {
	m := newTestModel(t)
	m.pushText(lineText, "## 标题\n| a | b |\n|---|---|\n| 1 | 2 |\n---\n> note")
	got := stripColor(m.renderTranscript())
	if strings.Contains(got, "##") {
		t.Fatalf("heading markers not stripped:\n%s", got)
	}
	if strings.Contains(got, "|") {
		t.Fatalf("raw pipes in transcript:\n%s", got)
	}
	for _, want := range []string{"标题", "│ note"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "─") {
		t.Fatalf("rule not drawn:\n%s", got)
	}

	// A fenced --- is code, not a rule, and the ``` delimiters themselves
	// never render — first line and last line of the block are content.
	m.pushText(lineText, "```\n---\n```")
	got = stripColor(m.renderTranscript())
	if !strings.Contains(got, "---") {
		t.Fatalf("--- inside a fence turned into a rule:\n%s", got)
	}
	if strings.Contains(got, "```") {
		t.Fatalf("fence delimiters leaked into the transcript:\n%s", got)
	}
}

// TestTranscriptListWrapsWithHangingIndent: a long list item wraps within
// the viewport width, and continuation lines hang under the text rather
// than under the bullet.
func TestTranscriptListWrapsWithHangingIndent(t *testing.T) {
	m := newTestModel(t)
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 30})
	m.pushText(lineText, "- "+strings.Repeat("word ", 40))
	got := stripColor(m.renderTranscript())
	var first, cont string
	for _, l := range strings.Split(got, "\n") {
		if strings.HasPrefix(l, "- ") {
			first = l
		}
		if strings.HasPrefix(l, "  word") {
			cont = l
		}
	}
	if first == "" {
		t.Fatalf("list item missing:\n%s", got)
	}
	if cont == "" {
		t.Fatalf("no hanging-indent continuation line:\n%s", got)
	}
	if n := ansi.StringWidth(first); n > 40 {
		t.Errorf("item line is %d cells wide, want ≤ 40: %q", n, first)
	}
}
