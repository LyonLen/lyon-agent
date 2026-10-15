package main

// Header widgets (#16): the four status cells riding to the right of the
// LYON wordmark. The header block's shape is frozen — identity line, six
// logo rows, rule (view_geom_test.go pins the frame math) — so the cells
// live INSIDE the logo rows: each wordmark row becomes the 36-cell
// wordmark plus one row of a four-cell region (model / usage / evolution
// / activity) spread across the remaining width, │-separated, one space
// of padding inside each cell edge. Cells whose share has no text pad
// blank, so every logo row measures exactly the terminal width.
//
// Everything is local and read-only: model/session state straight off
// tuiModel, the exam clock and leaderboard through eval's locked Views
// snapshot, the routing table through routing.Open — the two files are
// TTL-cached (evoSnapshot) because unlike the evolution panel (read only
// while open) the header paints on every frame. A datum that does not
// exist renders as a dim "—" on its line, in its own cell only; nothing
// is guessed, and one cell's missing data never takes the header down.
//
// Degradation, two bands under the full grid:
//   - no-logo terminals (width < 46 or height < 26) get no cells at all —
//     headerRows keeps its two-row narrow shape, frame math untouched;
//   - a region too narrow for four readable cells (width - logoCells <
//     widgetGridMin) folds the four cells' first lines into one compact
//     line on row 0 and pads the other five rows blank, so the "36-cell
//     wordmark + region == terminal width" contract holds at every width
//     the wordmark itself is drawn at.

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/eval"
	"lyon-agent/pkg/routing"
)

const (
	// logoCells is the wordmark's fixed width — the figfont render of
	// "LYON" (pinned by view_geom_test.go).
	logoCells = 36
	// widgetCellMin is the smallest grid cell that still reads (its two
	// padding spaces included): below it titles like "evolution" truncate
	// mid-word and the grid folds to the compact line instead.
	widgetCellMin = 12
	// widgetGridMin is the narrowest region hosting four cells + the
	// three │ separators between them.
	widgetGridMin = widgetCellMin*4 + 3 // 51 → full grid from width 87
	// widgetMin is the narrowest region that can show anything — below it
	// the region stays blank (headerRows drops the widgets).
	widgetMin = 10
	// evoSnapshotTTL bounds how often the header re-reads leaderboard.json
	// and routing.json: every frame would hammer the disk for a glance
	// that changes at most as fast as prompts land.
	evoSnapshotTTL = 2 * time.Second
)

// widgetCell is one header cell: a title row plus up to five content
// lines, already painted; the composer pads or ellipsizes each row to the
// cell's share of the region.
type widgetCell struct {
	title string
	lines []string
}

// row is the cell's row r (0 = title). Rows past the content are blank —
// cells adapt to the facts they have instead of padding phantom lines.
func (c widgetCell) row(r int) string {
	if r == 0 {
		return c.title
	}
	if r-1 < len(c.lines) {
		return c.lines[r-1]
	}
	return ""
}

// widgetColumns renders the widget region: exactly six rows of width
// cells (the grid, four cells sharing the region), or the compact
// single line on row 0 with blank padding below when the region cannot
// host four readable cells. Returns nil when the region is too small to
// show anything — the caller falls back to the bare wordmark.
func widgetColumns(m *tuiModel, width int) []string {
	if width < widgetMin {
		return nil
	}
	cells := [4]widgetCell{
		m.widgetModelCell(),
		m.widgetUsageCell(),
		m.widgetEvolutionCell(),
		m.widgetActivityCell(),
	}
	if width < widgetGridMin {
		cols := make([]string, 6)
		parts := make([]string, 0, len(cells))
		for _, c := range cells {
			if s := c.row(1); s != "" {
				parts = append(parts, s)
			}
		}
		cols[0] = fitRow(strings.Join(parts, paint(cDim, " · ")), width)
		pad := strings.Repeat(" ", width)
		for i := 1; i < len(cols); i++ {
			cols[i] = pad
		}
		return cols
	}
	// Grid: three separators leave width-3 cells to split as evenly as
	// the remainder allows (the extra cells go left, one each).
	avail := width - 3
	base, rem := avail/4, avail%4
	share := [4]int{base, base, base, base}
	for i := 0; i < rem; i++ {
		share[i]++
	}
	cols := make([]string, 6)
	for r := range cols {
		var b strings.Builder
		for i, c := range cells {
			if i > 0 {
				b.WriteString(paint(cDim, "│"))
			}
			// One space of padding inside each cell edge — content never
			// touches the separator or the neighbour's glyphs.
			b.WriteString(" " + fitRow(c.row(r), share[i]-2) + " ")
		}
		cols[r] = b.String()
	}
	return cols
}

// widgetModelCell: the configured model and where this session operates —
// identity facts, never "—" unless the config itself is empty.
func (m *tuiModel) widgetModelCell() widgetCell {
	c := widgetCell{title: paint(cBold, tr("model", "模型"))}
	model := "—"
	if m.cfg.Model != "" {
		model = shortModel(m.cfg.Model)
	}
	effort := "—"
	if m.cfg.Effort != "" {
		effort = m.cfg.Effort
	}
	// The mode badge spells all three states — unlike headerLine's badge,
	// "default" is a state worth seeing too (it is the one that asks).
	badge := paint(cGreen, "[default]")
	if m.sess != nil {
		switch m.sess.Mode() {
		case agent.ModePlan:
			badge = paint(cYellow, "[plan]")
		case agent.ModeBypass:
			badge = paint(cRed, "[bypass]")
		}
	} else {
		badge = paint(cDim, "—")
	}
	ws := "—"
	if m.workspace != "" {
		ws = filepath.Base(filepath.Clean(m.workspace))
	}
	c.lines = []string{
		model,
		paint(cDim, tr("effort ", "档位 ")) + effort,
		badge,
		paint(cDim, "ws ") + ws,
	}
	return c
}

// widgetUsageCell: this session's prompt count and token accounting.
// Prompts count the TUI's own input history (what was asked here);
// tokens come from the engine's turn stats plus per-model subagent usage
// — the same accumulation /stats and the corner gauge read. The cache
// row is the hit share of everything that carried a prompt (reads +
// writes + fresh input), matching /stats' share.
func (m *tuiModel) widgetUsageCell() widgetCell {
	c := widgetCell{title: paint(cBold, tr("usage", "用量"))}
	c.lines = []string{
		paint(cDim, tr("prompts ", "提问 ")) + fmt.Sprintf("%d", len(m.hist)),
	}
	if m.sess == nil {
		c.lines = append(c.lines, paint(cDim, "—"))
		return c
	}
	turns := m.sess.Stats()
	if len(turns) == 0 {
		c.lines = append(c.lines, paint(cDim, "—")) // fresh session: no usage yet
		return c
	}
	last := turns[len(turns)-1].Usage
	var total agent.Usage
	for _, t := range turns {
		total.Input += t.Usage.Input
		total.Output += t.Usage.Output
		total.CacheRead += t.Usage.CacheRead
		total.CacheCreation += t.Usage.CacheCreation
	}
	for _, u := range m.sess.SubUsage() {
		total.Input += u.Input
		total.Output += u.Output
		total.CacheRead += u.CacheRead
		total.CacheCreation += u.CacheCreation
	}
	hitRow := paint(cDim, tr("cache ", "缓存 ")) + paint(cDim, "—")
	if den := total.CacheRead + total.CacheCreation + total.Input; den > 0 {
		hit := float64(total.CacheRead) / float64(den) * 100
		hitRow = paint(cDim, tr("cache ", "缓存 ")) +
			paint(pctColor(hit), fmt.Sprintf("%.0f%%", hit))
	}
	c.lines = append(c.lines,
		paint(cDim, tr("turn ", "本轮 "))+fmtTokens(last.Input+last.Output+last.CacheRead+last.CacheCreation),
		paint(cDim, tr("all ", "累计 "))+fmtTokens(total.Input+total.Output+total.CacheRead+total.CacheCreation),
		hitRow)
	return c
}

// widgetEvolutionCell: the exam clock for the class this session's last
// prompt books rounds into (the same counter countRound ticks), the
// leaderboard's strongest class by newest scorecard, and routing's
// stored recommendation against the model actually running. Reads the
// TTL snapshot — never the maps or files directly.
func (m *tuiModel) widgetEvolutionCell() widgetCell {
	c := widgetCell{title: paint(cBold, tr("evolution", "进化"))}
	snap := loadEvoSnapshot()

	examRow, recRow := paint(cDim, "—"), paint(cDim, "—")
	if len(m.hist) > 0 {
		class := string(routing.Classify(m.hist[len(m.hist)-1]))
		for _, v := range snap.views {
			if v.Class != class {
				continue
			}
			exam := fmt.Sprintf("%d/%d", v.Rounds, eval.ExamInterval)
			if v.Rounds > 0 && v.Rounds%eval.ExamInterval == 0 {
				examRow = paint(cYellow, exam) // clock struck — same flag as the panel
			} else {
				examRow = exam
			}
			break
		}
		for _, e := range snap.routes {
			if e.Class == class && e.Model != "" {
				recRow = shortModel(e.Model)
				break
			}
		}
	}
	run := "—"
	if m.runModel != "" {
		run = shortModel(m.runModel)
	} else if m.cfg.Model != "" {
		run = shortModel(m.cfg.Model)
	}
	c.lines = []string{
		paint(cDim, tr("exam ", "考试 ")) + examRow,
	}
	if snap.sum.HasScores {
		c.lines = append(c.lines,
			paint(cDim, tr("top ", "最强 "))+snap.sum.BestClass,
			paint(cDim, tr("score ", "分 "))+fmt.Sprintf("%.2f", snap.sum.Best))
	} else {
		c.lines = append(c.lines, paint(cDim, "—")) // no scorecards yet: one line, not two blanks
	}
	c.lines = append(c.lines,
		paint(cDim, tr("rec ", "推荐 "))+recRow,
		paint(cDim, tr("run ", "实际 "))+run)
	return c
}

// widgetActivityCell: what the session is doing right now — parked
// prompts, live subagent runs, and the newest executing step (or the
// spinner between steps). Nothing active reads as a plain idle.
func (m *tuiModel) widgetActivityCell() widgetCell {
	c := widgetCell{title: paint(cBold, tr("activity", "动态"))}
	queue := len(m.queued)
	subs := 0
	step := ""
	// Backwards: the first (newest) running execution names the step row,
	// while every run_subagent node counts toward the subagent tally. The
	// prompt group is a container, not an action; unnamed rows carry
	// nothing to show.
	for i := len(m.steps) - 1; i >= 0; i-- {
		s := &m.steps[i]
		if s.status != stepRunning {
			continue
		}
		if s.name == "run_subagent" {
			subs++
		}
		if step == "" && s.kind != stepPrompt && s.name != "" {
			step = s.name
		}
	}
	if queue == 0 && subs == 0 && step == "" && !m.busy {
		c.lines = []string{paint(cDim, tr("idle", "空闲"))}
		return c
	}
	c.lines = []string{
		paint(cDim, tr("queue ", "排队 ")) + fmt.Sprintf("%d", queue),
		paint(cDim, tr("subs ", "子代理 ")) + fmt.Sprintf("%d", subs),
	}
	switch {
	case step != "":
		c.lines = append(c.lines, paint(cGreen, "▸ ")+step)
	case m.busy:
		c.lines = append(c.lines, m.spin.View()+paint(cDim, tr(" working", " 运行中")))
	}
	return c
}

// fmtTokens compresses a token count the way a glance reads it: exact
// under a thousand, then k, then M — badge-sized cells must not drown in
// digits.
func fmtTokens(n int64) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 1_000_000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	}
}

// evoSnapshot is one TTL'd copy of the two file-backed evolution
// sources: eval's locked Views snapshot plus the routing table's entries
// and the summary both panels derive (summarizeEvolution — same facts as
// the evolution panel's block, so the header never disagrees with it).
type evoSnapshot struct {
	at     time.Time
	views  []eval.View
	routes []routing.Entry
	sum    evoSummary
}

var evoSnap evoSnapshot

// loadEvoSnapshot reads leaderboard.json + routing.json at most once per
// evoSnapshotTTL. Files are read once (subsequent rows work off the
// copy), and a missing or corrupt file yields an empty board — pkg/eval
// and pkg/routing's Open contract, so this can render forever without a
// ledger and never fatal.
func loadEvoSnapshot() evoSnapshot {
	if !evoSnap.at.IsZero() && time.Since(evoSnap.at) < evoSnapshotTTL {
		return evoSnap
	}
	views := eval.OpenLeaderboard().Views(evoClassOrder)
	routes := routing.Open().All()
	evoSnap = evoSnapshot{
		at:     time.Now(),
		views:  views,
		routes: routes,
		sum:    summarizeEvolution(views, routes),
	}
	return evoSnap
}

// resetEvoSnapshot drops the TTL cache — tests point LYON_HOME at a temp
// dir and need the next render to see that dir's files.
func resetEvoSnapshot() { evoSnap = evoSnapshot{} }
