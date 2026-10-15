package main

// Tests for the header's four status cells (#16): grid geometry (six
// rows, exact region width, three separators), the compact line and the
// blank fallbacks when data is missing, plus one fact check per cell.
// Evolution facts are seeded into a temp LYON_HOME so the assertions
// never depend on — or leak into — the real ~/.lyon ledger.

import (
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/eval"
	"lyon-agent/pkg/routing"
)

// widgetTestModel isolates LYON_HOME (leaderboard/routing files) and
// drops any evoSnapshot another test cached, then builds the standard
// 100×30 model.
func widgetTestModel(t *testing.T) *tuiModel {
	t.Helper()
	t.Setenv("LYON_HOME", tempDir(t))
	resetEvoSnapshot()
	return newTestModel(t)
}

// TestWidgetColumnsGrid: the roomy grid is six rows of exactly the
// region width, four titled cells separated by three │, titles and all.
func TestWidgetColumnsGrid(t *testing.T) {
	m := widgetTestModel(t)
	const region = 100 - logoCells // newTestModel is 100 wide
	cols := widgetColumns(m, region)
	if len(cols) != 6 {
		t.Fatalf("grid rows: %d, want 6", len(cols))
	}
	for i, l := range cols {
		if n := ansi.StringWidth(l); n != region {
			t.Fatalf("grid row %d is %d cells, want region %d: %q", i, n, region, stripColor(l))
		}
		if n := strings.Count(stripColor(l), "│"); n != 3 {
			t.Fatalf("grid row %d has %d separators, want 3: %q", i, n, stripColor(l))
		}
	}
	all := stripColor(strings.Join(cols, "\n"))
	for _, title := range []string{
		tr("model", "模型"), tr("usage", "用量"),
		tr("evolution", "进化"), tr("activity", "动态"),
	} {
		if !strings.Contains(all, title) {
			t.Fatalf("cell title %q missing:\n%s", title, all)
		}
	}
}

// TestWidgetColumnsDegradation: a region too narrow for four cells folds
// to one compact line on row 0 with blank padding below; below that it
// returns nil so the caller draws the bare wordmark. Every returned row
// still measures exactly the requested width.
func TestWidgetColumnsDegradation(t *testing.T) {
	m := widgetTestModel(t)
	for _, width := range []int{widgetMin, 24, widgetGridMin - 1} {
		cols := widgetColumns(m, width)
		if len(cols) != 6 {
			t.Fatalf("region %d: %d rows, want 6", width, len(cols))
		}
		if strings.TrimSpace(stripColor(cols[0])) == "" {
			t.Fatalf("region %d: compact line is blank", width)
		}
		for i, l := range cols {
			if n := ansi.StringWidth(l); n != width {
				t.Fatalf("region %d row %d is %d cells, want %d", width, i, n, width)
			}
			if i > 0 && stripColor(l) != strings.Repeat(" ", width) {
				t.Fatalf("region %d row %d should pad blank: %q", width, i, stripColor(l))
			}
		}
	}
	if cols := widgetColumns(m, widgetMin-1); cols != nil {
		t.Fatalf("region %d should drop the widgets, got %d rows", widgetMin-1, len(cols))
	}
	// The grid threshold itself must open the grid, not the compact line.
	if cols := widgetColumns(m, widgetGridMin); len(cols) != 6 ||
		!strings.Contains(stripColor(strings.Join(cols, "\n")), "│") {
		t.Fatalf("region %d should render the grid", widgetGridMin)
	}
}

// TestHeaderRowsCarryWidgets: the header block keeps its 8/2-row shapes;
// every logo row now measures the full terminal width (wordmark +
// region), in the grid band and the compact band alike, and the frame
// invariants still hold.
func TestHeaderRowsCarryWidgets(t *testing.T) {
	m := widgetTestModel(t) // 100×30: roomy, grid band
	head := m.headerRows()
	if len(head) != 8 { // identity + 6 logo rows + rule
		t.Fatalf("roomy header rows: %d, want 8", len(head))
	}
	for i, l := range head[1:7] {
		if n := ansi.StringWidth(stripColor(l)); n != 100 {
			t.Fatalf("logo row %d is %d cells, want wordmark+region 100: %q",
				i, n, stripColor(l))
		}
	}
	checkView(t, m, "header widgets grid")

	// 70 wide: region 34 folds to the compact line, rows stay full width.
	m.Update(tea.WindowSizeMsg{Width: 70, Height: 30})
	head = m.headerRows()
	if len(head) != 8 {
		t.Fatalf("compact-band header rows: %d, want 8", len(head))
	}
	for i, l := range head[1:7] {
		if n := ansi.StringWidth(stripColor(l)); n != 70 {
			t.Fatalf("compact logo row %d is %d cells, want 70: %q", i, n, stripColor(l))
		}
	}
	checkView(t, m, "header widgets compact")

	// No-logo terminal: widgets vanish with the wordmark, block stays 2.
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	head = m.headerRows()
	if len(head) != 2 {
		t.Fatalf("narrow header rows: %d, want 2 (no widgets)", len(head))
	}
	checkView(t, m, "header widgets narrow")
}

// TestWidgetModelCell: model, effort, the all-states mode badge and the
// workspace basename.
func TestWidgetModelCell(t *testing.T) {
	m := widgetTestModel(t)
	m.workspace = filepath.Join("home", "dev", "lyon-ws")
	lines := stripColor(strings.Join(m.widgetModelCell().lines, "\n"))
	for _, want := range []string{
		shortModel(m.cfg.Model),
		tr("effort ", "档位 "),
		"[default]",
		"ws lyon-ws",
	} {
		if !strings.Contains(lines, want) {
			t.Fatalf("model cell missing %q:\n%s", want, lines)
		}
	}
	if _, err := m.sess.SetMode(agent.ModePlan); err != nil {
		t.Fatal(err)
	}
	if lines = stripColor(strings.Join(m.widgetModelCell().lines, "\n")); !strings.Contains(lines, "[plan]") {
		t.Fatalf("plan mode badge missing:\n%s", lines)
	}
}

// TestWidgetUsageFacts: fresh sessions say so with a dim "—" instead of
// invented numbers; restored turn stats turn into the turn/total/cache
// rows (one turn: in 1000 + out 500 + cache read 9000 + write 1000 =
// 11.5k, hit 9000/11000 = 82%).
func TestWidgetUsageFacts(t *testing.T) {
	m := widgetTestModel(t)
	m.hist = []string{"one", "two"}
	fresh := stripColor(strings.Join(m.widgetUsageCell().lines, "\n"))
	if !strings.Contains(fresh, tr("prompts ", "提问 ")+"2") {
		t.Fatalf("prompts row missing:\n%s", fresh)
	}
	if !strings.Contains(fresh, "—") {
		t.Fatalf("fresh usage should fall back to —:\n%s", fresh)
	}

	m.sess.Restore(agent.Snapshot{Turns: []agent.TurnStat{{
		Turn: 1, StopReason: "end_turn",
		Usage: agent.Usage{Input: 1000, Output: 500, CacheRead: 9000, CacheCreation: 1000},
	}}})
	lines := stripColor(strings.Join(m.widgetUsageCell().lines, "\n"))
	for _, want := range []string{
		tr("turn ", "本轮 ") + "11.5k",
		tr("all ", "累计 ") + "11.5k",
		"82%",
	} {
		if !strings.Contains(lines, want) {
			t.Fatalf("usage cell missing %q:\n%s", want, lines)
		}
	}
}

// TestWidgetEvolutionFacts: three booked rounds read back as the exam
// clock 3/10, the single scored class wins "top", and the routing
// table's stored model shows as the recommendation against the config
// model actually running.
func TestWidgetEvolutionFacts(t *testing.T) {
	m := widgetTestModel(t)
	prompt := "fix the rollback bug"
	class := string(routing.Classify(prompt))
	lb := eval.OpenLeaderboard()
	for i := 0; i < 3; i++ {
		lb.CountRound(class)
	}
	lb.Record(class, eval.Scorecard{Agent: "a1", Total: 0.9, Passed: true})
	routing.Open().Record(class, "a1", "test-model-5", 0.9)
	m.hist = append(m.hist, prompt)
	resetEvoSnapshot() // the seed landed after the first cached read

	lines := stripColor(strings.Join(m.widgetEvolutionCell().lines, "\n"))
	for _, want := range []string{
		"exam 3/10",
		tr("top ", "最强 ") + class,
		tr("score ", "分 ") + "0.90",
		tr("rec ", "推荐 ") + "test-model-5",
		tr("run ", "实际 ") + shortModel(m.cfg.Model),
	} {
		if !strings.Contains(lines, want) {
			t.Fatalf("evolution cell missing %q:\n%s", want, lines)
		}
	}
}

// TestWidgetEvolutionFresh: with no prompts and no scorecards the cell
// stays honest — dim "—" rows, no invented exam clock, but the running
// model is still a fact worth showing.
func TestWidgetEvolutionFresh(t *testing.T) {
	m := widgetTestModel(t)
	lines := stripColor(strings.Join(m.widgetEvolutionCell().lines, "\n"))
	for _, want := range []string{
		"—",
		tr("run ", "实际 ") + shortModel(m.cfg.Model),
	} {
		if !strings.Contains(lines, want) {
			t.Fatalf("fresh evolution cell missing %q:\n%s", want, lines)
		}
	}
	if strings.Contains(lines, "exam 0/10") {
		t.Fatalf("no prompt means no class clock:\n%s", lines)
	}
}

// TestWidgetActivityCell: idle when nothing is up; queue depth, live
// subagent count and the newest running step once work starts.
func TestWidgetActivityCell(t *testing.T) {
	m := widgetTestModel(t)
	if s := stripColor(strings.Join(m.widgetActivityCell().lines, "\n")); !strings.Contains(s, tr("idle", "空闲")) {
		t.Fatalf("quiet session should read idle:\n%s", s)
	}

	m.queued = []string{"parked one", "parked two"}
	m.steps = append(m.steps,
		execStep{n: 1, kind: stepTool, name: "run_subagent", status: stepRunning},
		execStep{n: 2, kind: stepTool, name: "run_shell", status: stepRunning},
		execStep{n: 3, kind: stepPrompt, detail: "the prompt group", status: stepRunning},
	)
	s := stripColor(strings.Join(m.widgetActivityCell().lines, "\n"))
	for _, want := range []string{
		tr("queue ", "排队 ") + "2",
		tr("subs ", "子代理 ") + "1",
		"run_shell", // newest execution, not the prompt container
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("activity cell missing %q:\n%s", want, s)
		}
	}
}

// TestFmtTokens: badge-sized compression — exact small, k, then M.
func TestFmtTokens(t *testing.T) {
	for n, want := range map[int64]string{
		0:           "0",
		999:         "999",
		11_500:      "11.5k",
		2_500_000:   "2.5M",
		123_456_789: "123.5M",
	} {
		if got := fmtTokens(n); got != want {
			t.Fatalf("fmtTokens(%d) = %q, want %q", n, got, want)
		}
	}
}
