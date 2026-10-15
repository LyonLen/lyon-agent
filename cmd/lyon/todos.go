package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// todoItem mirrors one plan entry of a todo_write call (#66 readable step
// feedback): the input always carries the COMPLETE list, so the newest
// event replaces the whole plan — no merge, no drift.
type todoItem struct {
	Content string `json:"content"`
	Status  string `json:"status"` // pending | in_progress | completed
}

// todoItemMax keeps one rendered row inside a terminal line.
const todoItemMax = 120

// parseTodos reads a todo_write input payload, dropping malformed items —
// a stray status must not hide the rest of the plan from the reader.
func parseTodos(raw json.RawMessage) []todoItem {
	var in struct {
		Todos []todoItem `json:"todos"`
	}
	if json.Unmarshal(raw, &in) != nil || len(in.Todos) == 0 {
		return nil
	}
	out := make([]todoItem, 0, len(in.Todos))
	for _, it := range in.Todos {
		c := strings.TrimSpace(it.Content)
		if c == "" {
			continue
		}
		switch it.Status {
		case "pending", "in_progress", "completed":
		default:
			continue
		}
		out = append(out, todoItem{Content: truncateRunes(c, todoItemMax), Status: it.Status})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// todoRows renders the plan as transcript rows: a scale header plus one
// glyph line per subtask. Shared by the TUI block and the plain REPL.
func todoRows(items []todoItem) []string {
	done, total := 0, len(items)
	rows := make([]string, 0, total+1)
	for _, it := range items {
		if it.Status == "completed" {
			done++
		}
	}
	rows = append(rows, fmt.Sprintf("%s %s %d/%d", paint(cYellow, "📋"),
		tr("plan", "计划"), done, total))
	for _, it := range items {
		rows = append(rows, todoGlyphRow(it))
	}
	return rows
}

// todoGlyphRow renders one plan item — status glyph + content, painted
// by status — shared by the transcript block and the corner panel.
func todoGlyphRow(it todoItem) string {
	switch it.Status {
	case "completed":
		return paint(cGreen, "☑ ") + it.Content
	case "in_progress":
		return paint(cCyan, "◐ ") + paint(cCyan, it.Content)
	default:
		return paint(cDim, "☐ ") + paint(cDim, it.Content)
	}
}

// setTodos replaces the live plan and rewrites the transcript block in
// place — the block is one tagged region, updated not appended, so ten
// plan refreshes don't bury the conversation (Claude Code behavior).
func (m *tuiModel) setTodos(items []todoItem) {
	m.todos = items
	m.pushTodos()
}

// pushTodos splices the rendered plan into the transcript: the existing
// todo-tagged run is replaced, a first plan appends at the tail.
func (m *tuiModel) pushTodos() {
	start, n := -1, 0
	for i := range m.lines {
		if m.lines[i].todo {
			if start < 0 {
				start = i
			}
			n++
		} else if start >= 0 {
			break // the block is one contiguous run
		}
	}
	rows := todoRows(m.todos)
	block := make([]tline, len(rows))
	for i, r := range rows {
		block[i] = tline{kind: lineStyled, raw: indTool + r, todo: true}
	}
	switch {
	case start < 0:
		m.curBlk = 0
		m.lines = append(m.lines, block...)
	default:
		tail := append([]tline(nil), m.lines[start+n:]...)
		m.lines = append(m.lines[:start], block...)
		m.lines = append(m.lines, tail...)
	}
}

// todoPreview is the steps-panel row for a todo_write call: the plan
// scale plus the current subtask — the panel reads as progress, not JSON.
func todoPreview(raw json.RawMessage) string {
	items := parseTodos(raw)
	if items == nil {
		return "plan"
	}
	done, total := 0, len(items)
	current := ""
	for _, it := range items {
		if it.Status == "completed" {
			done++
		}
		if it.Status == "in_progress" && current == "" {
			current = it.Content
		}
	}
	out := fmt.Sprintf("%d/%d", done, total)
	if current != "" {
		out += " · " + truncateRunes(current, 60)
	}
	return out
}

// todoProgress feeds the busy status strip: scale plus the subtask the
// model is on right now ("" when the plan has no in_progress item).
func (m *tuiModel) todoProgress() (done, total int, current string) {
	for _, it := range m.todos {
		total++
		if it.Status == "completed" {
			done++
		}
		if it.Status == "in_progress" && current == "" {
			current = it.Content
		}
	}
	return done, total, current
}

// todosState serializes the plan for the snapshot; nil when empty so an
// unplanned session never grows its snapshot file. The payload keeps the
// tool-input shape ({"todos":[…]}) so parseTodos reads both paths.
func todosState(items []todoItem) json.RawMessage {
	if len(items) == 0 {
		return nil
	}
	raw, err := json.Marshal(struct {
		Todos []todoItem `json:"todos"`
	}{items})
	if err != nil {
		return nil
	}
	return raw
}

// applyTodos restores the plan block after /resume (false = nothing to
// draw — old snapshots carry no Todos field).
func (m *tuiModel) applyTodos(raw json.RawMessage) bool {
	items := parseTodos(raw)
	if items == nil {
		return false
	}
	m.setTodos(items)
	return true
}

// planSectionH is the resident plan block's height in a column of h
// rows (footer excluded): one header row plus every item while they
// fit, capped at a third of the column so the panel keeps the bulk, and
// never 0 — the corner seat exists even before the first todo_write (#1).
func planSectionH(items []todoItem, h int) int {
	if h < 1 {
		return 0
	}
	want := len(items) + 1
	if maxH := h / 3; want > maxH && maxH >= 1 {
		want = maxH
	}
	if want > h {
		want = h
	}
	return want
}

// renderPlan is the right column's resident plan block (#1): exactly h
// rows of at most w cells — a scale header carrying the subtask the
// model is on, one glyph row per item windowed from the top with a
// "+n more" tail when the plan outruns the block, and a one-line dim
// placeholder before the first todo_write so the seat always reads as
// the plan's. The transcript keeps its own updating block; this is the
// live corner view.
func (m *tuiModel) renderPlan(w, h int) []string {
	if w <= 0 || h <= 0 {
		return nil
	}
	out := make([]string, 0, h)
	if len(m.todos) == 0 {
		out = append(out, fitRow(paint(cDim, tr("📋 plan · none yet", "📋 计划 · 还没有")), w))
		for len(out) < h {
			out = append(out, "")
		}
		return out
	}
	done, total, current := m.todoProgress()
	head := fmt.Sprintf("%s %s %d/%d", paint(cYellow, "📋"),
		tr("plan", "计划"), done, total)
	if current != "" {
		// The header carries the live subtask so a windowed block can
		// never hide what the model is actually doing right now.
		if avail := w - ansi.StringWidth(head) - 6; avail > 8 {
			head += paint(cDim, " ▸ "+truncateRunes(current, avail))
		}
	}
	out = append(out, fitRow(head, w))

	// Window from the top: when the plan outruns the block, one row is
	// reserved for the "+n more" tail instead of a cut item.
	items := m.todos
	room := h - len(out)
	if hide := len(items) - room; hide > 0 {
		if room > 0 {
			hide++ // reserve the tail row
		}
		items = items[:len(items)-hide]
	}
	for _, it := range items {
		out = append(out, fitRow(todoGlyphRow(it), w))
	}
	if more := len(m.todos) - len(items); more > 0 {
		out = append(out, fitRow(paint(cDim,
			fmt.Sprintf(tr("…(+%d more)", "…(+%d 项)"), more)), w))
	}
	for len(out) < h {
		out = append(out, "")
	}
	return out[:h]
}
