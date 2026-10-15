package main

// The evolution panel — the steps panel's twin, toggled by alt+e (or
// /evolve) — makes design docs/agent-evolution.md visible while you work:
// one row per task class showing who sits the on-duty seat, how close the
// exam clock is (every ExamInterval production rounds), the on-duty
// agent's recent-10 mean, and the row's alert flag (exam due, failure
// streak, or an open seat). Data comes from eval.Leaderboard.Views — a
// locked snapshot, never the maps themselves (Record files cards from a
// goroutine while this paints).

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"lyon-agent/pkg/eval"
	"lyon-agent/pkg/routing"
)

// evoClassOrder is the panel's row order: routing's classes, most common
// first. Views appends any extra class the ledger grows, sorted, so a
// rename or addition lands at the bottom instead of vanishing.
var evoClassOrder = []string{
	string(routing.ClassCoding),
	string(routing.ClassFix),
	string(routing.ClassReview),
	string(routing.ClassResearch),
	string(routing.ClassWriting),
	string(routing.ClassDesign),
	string(routing.ClassData),
	string(routing.ClassGeneral),
}

// renderEvolve produces exactly h rows of at most w cells — the same
// contract as renderSteps, so composeSplit treats both panels alike.
// Layout: header row, one row per class (clipped to the room), blanks.
// The hint footer and the resident plan block are composed above/below
// by composeSplit — they span the whole right column (#1).
func (m *tuiModel) renderEvolve(w, h int) []string {
	if w <= 0 || h <= 0 {
		return nil
	}
	header := paint(cBold, tr("evolution", "进化")) +
		paint(cDim, tr(fmt.Sprintf(" · exam every %d rounds", eval.ExamInterval),
			fmt.Sprintf(" · 每 %d 轮考试", eval.ExamInterval)))
	out := []string{fitRow(header, w)}

	// The feed: recent evolution cards (score/exam) — the panel's own
	// output area while the panel is open (evoOutput, #47). Entries may
	// be multi-line; the newest rows win, capped so the class table
	// stays primary.
	var feed []string
	for _, e := range m.evoFeed {
		for _, l := range strings.Split(e, "\n") {
			feed = append(feed, l)
		}
	}
	feedCap := 6
	if h/3 < feedCap {
		feedCap = h / 3
	}
	if len(feed) > feedCap {
		feed = feed[len(feed)-feedCap:]
	}

	room := h - 1 - len(feed) // content rows; the header takes one more
	if room < 0 {
		room = 0
	}
	views := eval.OpenLeaderboard().Views(evoClassOrder)
	content := make([]string, 0, len(views))
	quiet := true
	for _, v := range views {
		if v.Cards > 0 || v.OnDuty != "" || v.Rounds > 0 {
			quiet = false
		}
		content = append(content, evolveRow(v, w))
	}
	if quiet {
		content = evoEmptyHints(w)
	}
	for i := 0; i < room && i < len(content); i++ {
		out = append(out, content[i])
	}
	for _, l := range feed {
		out = append(out, fitRow(l, w))
	}
	for len(out) < h {
		out = append(out, "")
	}
	return out
}

// evoFooter is the evolution panel's hint row — the column's shared
// bottom line, composed by composeSplit below the plan block (#1), so
// the plan sits in the corner whichever panel is open.
func evoFooter(w int) string {
	return fitRow(paint(cDim, tr("/exam · /route · alt+←→ width · alt+e close",
		"/exam · /route · alt+←→ 调宽 · alt+e 关闭")), w)
}

// evolveRow renders one class: class name, seat (model or "(open)"), the
// exam clock (rounds since last exam / interval), the on-duty mean, and
// one alert flag — EXAM due (yellow), failure streak (red), open seat
// (dim) — right-aligned so the columns stay put across rows. On overflow
// the flag yields first, then the seat's tail: the class name and clock
// are what a glance needs.
func evolveRow(v eval.View, w int) string {
	seat := tr("(open)", "(空缺)")
	if v.OnDuty != "" {
		seat = shortModel(v.OnDuty)
	}
	tail := fmt.Sprintf("%d/%d", v.Rounds, eval.ExamInterval)
	if v.OnDuty != "" {
		tail += fmt.Sprintf(" %.2f", v.Mean)
	}
	flag, fcolor := "", ""
	switch {
	case v.OnDuty == "":
		flag, fcolor = "open", cDim
	case v.Rounds > 0 && v.Rounds%eval.ExamInterval == 0:
		flag, fcolor = "EXAM", cYellow
	case v.ExamFails > 0:
		flag, fcolor = fmt.Sprintf("f%d", v.ExamFails), cRed
	}
	flagW := 0
	if flag != "" {
		flagW = 1 + len(flag) // ASCII flag + separating space
	}
	// Seat budget: class(8) + gap + seat + gap + tail + flag must fit w.
	seatMax := w - 8 - 1 - 1 - ansi.StringWidth(tail) - flagW
	if seatMax < 4 {
		seatMax = 4
	}
	if r := []rune(seat); len(r) > seatMax {
		seat = string(r[:seatMax-1]) + "…"
	}
	left := fmt.Sprintf("%-8s", v.Class) + " " + seat
	avail := w - ansi.StringWidth(left) - ansi.StringWidth(tail) - flagW
	if avail < 1 {
		return fitRow(left+" "+tail, w) // flag sacrificed to the width
	}
	row := left + strings.Repeat(" ", avail) + tail
	if flag != "" {
		row += " " + paint(fcolor, flag)
	}
	return fitRow(row, w)
}

// evoEmptyHints fills the body while the ledger has no evidence at all.
func evoEmptyHints(w int) []string {
	lines := []string{
		tr("  no scores yet", "  还没有评分"),
		tr("  every finished prompt files a", "  每次完成的提问都会记一张"),
		tr("  scorecard and books a round", "  评分卡并记一轮"),
		tr("  /exam [class] to challenge", "  /exam [class] 直接挑战"),
		tr("  the on-duty seat directly", "  在岗席位"),
	}
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, fitRow(paint(cDim, l), w))
	}
	return out
}
