package main

// Tests for the header's four status cells (#16, machine/time/weather/
// brief): grid geometry (six rows, exact region width, three
// separators), the compact line and the blank fallbacks when data is
// missing, plus one fact check per cell. Machine/weather/brief facts are
// seeded straight into the dashboard cell store (the same store the
// refresher goroutines write), so the assertions never depend on — or
// leak into — the real ~/.lyon or any network.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// widgetTestModel isolates LYON_HOME, drops any cells another test
// cached, then builds the standard 100×30 model.
func widgetTestModel(t *testing.T) *tuiModel {
	t.Helper()
	t.Setenv("LYON_HOME", tempDir(t))
	dashCellsReset()
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
		tr("machine", "机器"), tr("time", "时间"),
		tr("weather", "天气"), tr("brief", "简报"),
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

// TestWidgetMachineCell: a seeded sysinfo snap reads back as the four
// fact rows; before the first refresh the cell stays honest with "—".
func TestWidgetMachineCell(t *testing.T) {
	m := widgetTestModel(t)
	if s := stripColor(strings.Join(m.widgetMachineCell().lines, "\n")); s != "—" {
		t.Fatalf("empty store should read —:\n%s", s)
	}

	dashPutCell("sysinfo", sysSnap{
		CPUPct: 85, MemPct: 72, MemUsed: 23 << 30, MemTotal: 32 << 30,
		DiskFree: 10 << 30, DiskTotal: 240 << 30,
		NetUpBps: 319 << 10, NetDownBps: 126 << 10, NetOK: true,
	}, time.Now(), false)
	lines := stripColor(strings.Join(m.widgetMachineCell().lines, "\n"))
	for _, want := range []string{
		"85%",
		tr("mem ", "内存 ") + "23G/32G 72%",
		"96%", // disk used share
		"↑319K ↓126K",
	} {
		if !strings.Contains(lines, want) {
			t.Fatalf("machine cell missing %q:\n%s", want, lines)
		}
	}
}

// TestWidgetTimeCell: the clock always has its two rows — date line and
// ISO week — because it reads the instant, not a fetch.
func TestWidgetTimeCell(t *testing.T) {
	m := widgetTestModel(t)
	c := m.widgetTimeCell()
	if len(c.lines) != 2 {
		t.Fatalf("time cell rows: %d, want 2 (%q)", len(c.lines), c.lines)
	}
	week, _ := time.Now().ISOWeek()
	if !strings.Contains(stripColor(c.lines[1]), fmt.Sprint(week)) {
		t.Fatalf("week row %q should carry week %d", c.lines[1], week)
	}
	if strings.Contains(stripColor(strings.Join(c.lines, "\n")), "—") {
		t.Fatalf("clock never degrades:\n%v", c.lines)
	}
}

// TestWidgetWeatherCell: seeded weather+geo cells read as temperature,
// hi/lo and the city line; an empty store shows the wait copy (no coords
// anywhere → waiting for location).
func TestWidgetWeatherCell(t *testing.T) {
	m := widgetTestModel(t)
	empty := stripColor(strings.Join(m.widgetWeatherCell().lines, "\n"))
	if !strings.Contains(empty, tr("waiting for location", "等待位置")) &&
		!strings.Contains(empty, tr("waiting for weather", "等待天气")) {
		t.Fatalf("fresh weather cell should wait, got:\n%s", empty)
	}

	dashPutCell("weather", &weatherData{Temp: 28, Code: 3, Wind: 18, Hi: 29, Lo: 25}, time.Now(), false)
	dashPutCell("geo", &geoData{City: "Hong Kong", IP: "203.0.113.9"}, time.Now(), false)
	lines := stripColor(strings.Join(m.widgetWeatherCell().lines, "\n"))
	for _, want := range []string{"28°C", tr("overcast", "阴"), "18km/h", "29/25°", "Hong Kong"} {
		if !strings.Contains(lines, want) {
			t.Fatalf("weather cell missing %q:\n%s", want, lines)
		}
	}
}

// TestWidgetBriefCell: ranked headlines render newest-first, clipped to
// the three-line budget; nothing fetched reads as "—".
func TestWidgetBriefCell(t *testing.T) {
	m := widgetTestModel(t)
	if s := stripColor(strings.Join(m.widgetBriefCell().lines, "\n")); s != "—" {
		t.Fatalf("fresh brief cell should read —:\n%s", s)
	}

	items := make([]rssItem, 5)
	for i := range items {
		items[i].Title = fmt.Sprintf("headline-%d", i)
	}
	dashPutCell("brief", &briefData{Items: items, FeedsOK: 2}, time.Now(), false)
	lines := stripColor(strings.Join(m.widgetBriefCell().lines, "\n"))
	for _, want := range []string{"headline-0", "headline-1", "headline-2"} {
		if !strings.Contains(lines, want) {
			t.Fatalf("brief cell missing %q:\n%s", want, lines)
		}
	}
	if strings.Contains(lines, "headline-3") {
		t.Fatalf("brief cell must clip to %d lines:\n%s", widgetBriefLines, lines)
	}
}
