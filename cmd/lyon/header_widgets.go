package main

// Header widgets (#16, content revised): the four status cells riding to
// the right of the LYON wordmark now carry machine/time/weather/brief —
// the glance the user asked to live at the TOP of the screen, fed by the
// dashboard's background cell store (zero extra network: the refresher
// goroutines already write every cell this reads). The header block's
// shape is frozen — identity line, six logo rows, rule (view_geom_test.go
// pins the frame math) — so the cells live INSIDE the logo rows: each
// wordmark row becomes the 36-cell wordmark plus one row of a four-cell
// region spread across the remaining width, │-separated, one space of
// padding inside each cell edge. Cells whose share has no text pad
// blank, so every logo row measures exactly the terminal width.
//
// Data sources, all local reads: machine reads the sysinfo cell (sysSnap
// from the OS probes, refreshed every 5s), time reads the clock directly
// (a fact, not a fetch), weather reads the weather cell plus the geo cell
// for the city line, brief reads the RSS cell's ranked headlines. The
// startup dashboard block (/home) shares the same store — the two views
// can never disagree. A module turned off in dashboard.json, or not yet
// refreshed (startup, -plain, tests), renders a dim "—" or its wait
// placeholder on its own line, in its own cell only; nothing is guessed,
// and one cell's missing data never takes the header down.
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
	"strings"
	"time"
)

const (
	// logoCells is the wordmark's fixed width — the figfont render of
	// "LYON" (pinned by view_geom_test.go).
	logoCells = 36
	// widgetCellMin is the smallest grid cell that still reads (its two
	// padding spaces included): below it titles truncate mid-word and the
	// grid folds to the compact line instead.
	widgetCellMin = 12
	// widgetGridMin is the narrowest region hosting four cells + the
	// three │ separators between them.
	widgetGridMin = widgetCellMin*4 + 3 // 51 → full grid from width 87
	// widgetMin is the narrowest region that can show anything — below it
	// the region stays blank (headerRows drops the widgets).
	widgetMin = 10
	// widgetBriefLines bounds the brief cell: the three newest ranked
	// headlines fit the six-row grid with room to spare.
	widgetBriefLines = 3
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
		m.widgetMachineCell(),
		m.widgetTimeCell(),
		m.widgetWeatherCell(),
		m.widgetBriefCell(),
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

// widgetMachineCell: CPU / memory / disk / network from the sysinfo cell
// — the OS probes' numbers, never invented; an unmeasurable value shows
// its "…" the same way the dashboard block does.
func (m *tuiModel) widgetMachineCell() widgetCell {
	c := widgetCell{title: paint(cBold, tr("machine", "机器"))}
	cell, ok := dashGetCell("sysinfo")
	s, isSnap := sysSnap{}, false
	if ok {
		s, isSnap = cell.data.(sysSnap)
	}
	if !ok || !isSnap {
		c.lines = []string{paint(cDim, "—")}
		return c
	}
	cpu := tr("cpu ", "CPU ") + "…"
	if s.CPUPct >= 0 {
		cpu = tr("cpu ", "CPU ") + fmt.Sprintf("%.0f%%", s.CPUPct)
	}
	mem := tr("mem ", "内存 ") + "…"
	if s.MemTotal > 0 {
		pct := s.MemPct
		if pct < 0 {
			pct = float64(s.MemUsed) / float64(s.MemTotal) * 100
		}
		mem = tr("mem ", "内存 ") + fmt.Sprintf("%s/%s %.0f%%",
			dashBytes(s.MemUsed), dashBytes(s.MemTotal), pct)
	}
	disk := tr("disk ", "磁盘 ") + "…"
	if s.DiskTotal > 0 {
		used := s.DiskTotal - s.DiskFree
		disk = tr("disk ", "磁盘 ") + fmt.Sprintf("%.0f%%",
			float64(used)/float64(s.DiskTotal)*100)
	}
	net := tr("net ", "网络 ") + "…"
	if s.NetOK {
		net = tr("net ", "网络 ") + fmt.Sprintf("↑%s ↓%s",
			dashBytes(uint64(s.NetUpBps)), dashBytes(uint64(s.NetDownBps)))
	}
	c.lines = []string{cpu, mem, disk, net}
	return c
}

// widgetTimeCell: the clock — a fact, not a fetch, so it reads the
// instant directly (the header repaints anyway) and never degrades.
func (m *tuiModel) widgetTimeCell() widgetCell {
	now := time.Now()
	week, _ := now.ISOWeek()
	date := now.Format("01-02") + " " + zhWeekdays[int(now.Weekday())] +
		" " + now.Format("15:04:05")
	if uiLang != "zh" {
		date = now.Format("Mon Jan 02 15:04:05")
	}
	return widgetCell{
		title: paint(cBold, tr("time", "时间")),
		lines: []string{
			date,
			paint(cDim, tr(fmt.Sprintf("week %d", week), fmt.Sprintf("第%d周", week))),
		},
	}
}

// widgetWeatherCell: temperature and wind from the weather cell, city
// from the geo cell — both fed by the dashboard refreshers, so the header
// adds no network of its own. Before the first refresh the wait copy
// matches the dashboard's own placeholders.
func (m *tuiModel) widgetWeatherCell() widgetCell {
	c := widgetCell{title: paint(cBold, tr("weather", "天气"))}
	if cell, ok := dashGetCell("weather"); ok {
		if w, ok := cell.data.(*weatherData); ok {
			en, zh := weatherDesc(w.Code)
			c.lines = []string{
				fmt.Sprintf("%.0f°C %s · %s %.0fkm/h",
					w.Temp, tr(en, zh), tr("wind", "风"), w.Wind),
				paint(cDim, fmt.Sprintf("%.0f/%.0f°", w.Hi, w.Lo)),
			}
		}
	}
	if cell, ok := dashGetCell("geo"); ok {
		if g, ok := cell.data.(*geoData); ok && g.City != "" {
			c.lines = append(c.lines, paint(cDim, g.City))
		}
	}
	if len(c.lines) == 0 {
		_, _, located := dashWeatherCoords()
		msg := tr("waiting for weather", "等待天气")
		if !located {
			msg = tr("waiting for location", "等待位置")
		}
		c.lines = []string{paint(cDim, msg)}
	}
	return c
}

// widgetBriefCell: the newest ranked headlines from the RSS cell — one
// line each, ellipsized by the composer to the cell's share. Offline or
// not-yet-fetched reads as a plain "—"; the dashboard block keeps the
// fuller fallback copy (its job to expand, the header's to glance).
func (m *tuiModel) widgetBriefCell() widgetCell {
	c := widgetCell{title: paint(cBold, tr("brief", "简报"))}
	cell, ok := dashGetCell("brief")
	if !ok {
		c.lines = []string{paint(cDim, "—")}
		return c
	}
	b, ok := cell.data.(*briefData)
	if !ok || len(b.Items) == 0 {
		c.lines = []string{paint(cDim, "—")}
		return c
	}
	n := len(b.Items)
	if n > widgetBriefLines {
		n = widgetBriefLines
	}
	for _, it := range b.Items[:n] {
		c.lines = append(c.lines, it.Title)
	}
	return c
}
