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
	"time"

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
// Layout: header row, one row per class, the card feed, then the two
// info blocks (#14: overall evolution summary, exam-set how-to), blanks.
// The blocks reserve a floor of the rows the class table will not need,
// so neither a loaded feed nor a tall table crowds them off the panel;
// within that budget the class table keeps priority, the feed yields
// first, and the blocks truncate at the bottom when the height truly
// runs out — the exact-h contract means the shared footer composed by
// composeSplit can never be pushed off screen. The hint footer and the
// resident plan block are composed above/below by composeSplit — they
// span the whole right column (#1).
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

	// The two bottom blocks (#14): the overall evolution summary and the
	// exam-set how-to — assembled from the same locked snapshot plus the
	// routing table, exam files and candidate field (each read per paint
	// like the leaderboard above, and only while the panel is open).
	blocks := append(
		renderEvoSummary(summarizeEvolution(views, routing.Open().All()), w),
		renderEvoExamSet(describeExamSet(views, m.cfg.Model), w)...)
	reserve := h - 1 - len(content) // rows the class table won't claim
	if reserve > len(blocks) {
		reserve = len(blocks)
	}
	if reserve < 0 {
		reserve = 0
	}
	topRoom := h - 1 - reserve // header already took one row

	for i := 0; i < topRoom && i < len(content); i++ {
		out = append(out, content[i])
	}
	for _, l := range feed {
		if len(out)-1 >= topRoom {
			break
		}
		out = append(out, fitRow(l, w))
	}
	for _, l := range blocks {
		if len(out) >= h {
			break
		}
		out = append(out, l)
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

// renderEvoSummary lays the overall-evolution block out (#14.1): title,
// totals, the newest-score range with strongest/weakest, the newest
// filed result, and a health verdict. A fact the snapshot cannot supply
// (version-over-version deltas — the ledger keeps no version history)
// is omitted, never approximated; the block is statistics only, no
// model call. Rows word-wrap to w through appendInfo so nothing a
// reader needs is cut at narrow panel widths.
func renderEvoSummary(s evoSummary, w int) []string {
	rows := []string{fitRow(paint(cBold, tr("overall evolution", "整体进化情况")), w)}
	rows = appendInfo(rows, tr(
		fmt.Sprintf("%d classes · %d tested · %d cards · seats %d held · %d open · %d routed",
			s.Classes, s.Tested, s.Cards, s.Held, s.Open, s.Routed),
		fmt.Sprintf("%d 类 · 已测 %d · 卡 %d · 在岗 %d · 空缺 %d · 路由 %d",
			s.Classes, s.Tested, s.Cards, s.Held, s.Open, s.Routed)), w, cDim)

	if !s.HasScores {
		rows = appendInfo(rows, tr(
			"health: idle — no scorecards yet; this block fills in as grades land",
			"健康度：待机 — 还没有评分卡，出分后此处更新"), w, cDim)
		return rows
	}
	switch {
	case s.Single:
		rows = appendInfo(rows, tr(
			fmt.Sprintf("only scored class: %s %.2f [%s]", s.BestClass, s.Best, shortModel(s.BestAgent)),
			fmt.Sprintf("唯一有分的类：%s %.2f [%s]", s.BestClass, s.Best, shortModel(s.BestAgent))), w, cCyan)
	default:
		rows = appendInfo(rows, tr(
			fmt.Sprintf("strongest %s %.2f [%s] · weakest %s %.2f [%s] · latest range %.2f–%.2f",
				s.BestClass, s.Best, shortModel(s.BestAgent),
				s.WorstClass, s.Worst, shortModel(s.WorstAgent), s.Worst, s.Best),
			fmt.Sprintf("最强 %s %.2f [%s] · 最弱 %s %.2f [%s] · 最新分区间 %.2f–%.2f",
				s.BestClass, s.Best, shortModel(s.BestAgent),
				s.WorstClass, s.Worst, shortModel(s.WorstAgent), s.Worst, s.Best)), w, cCyan)
	}
	if !s.RecentAt.IsZero() {
		kind, verdict := tr("score", "评分"), tr("pass", "通过")
		if s.RecentExam {
			kind = tr("exam", "考试")
		}
		if !s.RecentPassed {
			verdict = tr("fail", "未过")
		}
		rows = appendInfo(rows, tr(
			fmt.Sprintf("newest: %s %s %.2f %s · %s",
				s.RecentClass, kind, s.RecentTotal, verdict, fmtAgo(s.RecentAt)),
			fmt.Sprintf("最近：%s %s %.2f %s · %s",
				s.RecentClass, kind, s.RecentTotal, verdict, fmtAgo(s.RecentAt))), w, cDim)
	}
	switch {
	case s.Fails > 0:
		rows = appendInfo(rows, evoHealthLine(s), w, cRed)
	case s.Due > 0 || s.Open > 0:
		rows = appendInfo(rows, evoHealthLine(s), w, cYellow)
	default:
		rows = appendInfo(rows, tr(
			"health: healthy — no failure streaks, no exams due, no open seats",
			"健康度：健康 — 无连败、无临考、无空席"), w, cGreen)
	}
	return rows
}

// evoHealthLine states the attention counts behind an unhealthy verdict
// — the same numbers the class rows flag one by one, rolled up.
func evoHealthLine(s evoSummary) string {
	return tr(
		fmt.Sprintf("health: attention — failure streaks %d · exams due %d · open seats %d",
			s.Fails, s.Due, s.Open),
		fmt.Sprintf("健康度：注意 — 连败类 %d · 临考类 %d · 空席 %d",
			s.Fails, s.Due, s.Open))
}

// renderEvoExamSet lays the exam-set block out (#14.2): what the
// evaluation set IS — classes, on-disk banks, question counts, the
// candidate field — and how to adjust it: the real files, commands and
// environment toggles, each wording verified against the code path that
// reads it (exams → eval.LoadExam/SaveExam, agents → $LYON_HOME/agents,
// LYON_EXAM → countRound, LYON_SCORE → kickScore, LYON_LANG → lang.go).
func renderEvoExamSet(info examSetInfo, w int) []string {
	rows := []string{fitRow(paint(cBold, tr("exam set · adjust it", "测评集 · 如何调整")), w)}
	// Composition: every class with its on-disk question count, "–"
	// while the shipped bank has not been seeded to disk yet.
	parts := make([]string, 0, len(info.PerClass))
	for _, b := range info.PerClass {
		n := "–"
		if b.Seeded {
			n = fmt.Sprintf("%d", b.Quests)
		}
		parts = append(parts, b.Class+" "+n)
	}
	if info.Banks == 0 {
		rows = appendInfo(rows, tr(
			fmt.Sprintf("%d classes — banks seed to $LYON_HOME/exams on first /exam: held-out, frozen in the binary, never generated from the tasks they grade",
				info.Classes),
			fmt.Sprintf("%d 类 — 首次 /exam 时题库落盘 $LYON_HOME/exams：脱题冻结内置，绝不从被考核的生产任务生成",
				info.Classes)), w, cDim)
	} else {
		rows = appendInfo(rows, tr(
			fmt.Sprintf("%d classes · %d/%d banks on disk · %d held-out questions · closed-book · pass 75%% default",
				info.Classes, info.Banks, info.Classes, info.Questions),
			fmt.Sprintf("%d 类 · 磁盘题库 %d/%d · 脱题闭卷 %d 题 · 默认及格线 75%%",
				info.Classes, info.Banks, info.Classes, info.Questions)), w, cDim)
	}
	rows = appendInfo(rows, strings.Join(parts, " · "), w, cDim)
	rows = appendInfo(rows, tr(
		"items are hand-written facts graded by expected substring or keyword majority",
		"题目为手写事实题，按 expected 子串匹配或关键词过半判分"), w, cDim)
	rows = appendInfo(rows, tr(
		"add/remove/edit: $LYON_HOME/exams/<class>.json — fields q · expected · keywords · pass",
		"增删改题：$LYON_HOME/exams/<class>.json — 字段 q · expected · keywords · pass"), w, cDim)
	rows = appendInfo(rows, tr(
		fmt.Sprintf("candidates: %d in $LYON_HOME/agents/ — /agents add <id> <model> [effort] · /agents rm <id>",
			info.Candidates),
		fmt.Sprintf("候选：%d 名在 $LYON_HOME/agents/ — /agents add <id> <model> [档位] · /agents rm <id>",
			info.Candidates)), w, cDim)
	rows = appendInfo(rows, tr(
		"practice: /exam [class] now · /contest [class] whole field · /route evidence",
		"演练：/exam [类] 立即 · /contest [类] 全场 · /route 证据"), w, cDim)
	rows = appendInfo(rows, tr(
		"toggles: LYON_HOME=dir · LYON_EXAM=off · LYON_SCORE=off · LYON_LANG=zh",
		"开关：LYON_HOME=目录 · LYON_EXAM=off · LYON_SCORE=off · LYON_LANG=en"), w, cDim)
	return rows
}

// appendInfo wraps one plain info line to the panel width and appends
// the painted rows: how-to text degrades by wrapping, never truncation,
// and fitRow pins every emitted row to exactly w cells.
func appendInfo(rows []string, line string, w int, color string) []string {
	for _, l := range wrapInfo(line, w) {
		rows = append(rows, fitRow(paint(color, l), w))
	}
	return rows
}

// wrapInfo word-wraps plain (ANSI-free) text to w display cells —
// East-Asian runes count two — hard-splitting any lone token that still
// overflows, since a CJK run carries no spaces and an overlong path
// must wrap, not vanish.
func wrapInfo(s string, w int) []string {
	if w < 4 {
		w = 4
	}
	var out []string
	cur := ""
	flush := func() {
		if cur != "" {
			out = append(out, cur)
			cur = ""
		}
	}
	for _, word := range strings.Fields(s) {
		if ansi.StringWidth(word) > w {
			flush()
			for ansi.StringWidth(word) > w {
				head, rest := splitCells(word, w)
				out = append(out, head)
				word = rest
			}
			cur = word // the tail fits (≤ w); the next word may push it over
			continue
		}
		switch {
		case cur == "":
			cur = word
		case ansi.StringWidth(cur)+1+ansi.StringWidth(word) <= w:
			cur += " " + word
		default:
			out = append(out, cur)
			cur = word
		}
	}
	flush()
	if len(out) == 0 {
		out = append(out, "") // an empty line still becomes a padded row below
	}
	return out
}

// splitCells cuts s at w display cells, never mid-rune.
func splitCells(s string, w int) (head, rest string) {
	n := 0
	for i, r := range s {
		if n+ansi.StringWidth(string(r)) > w {
			return s[:i], s[i:]
		}
		n += ansi.StringWidth(string(r))
	}
	return s, ""
}

// fmtAgo compresses an age the way a glance reads it — minutes, hours,
// days — clamped at "now" for future-timestamp clock skew.
func fmtAgo(t time.Time) string {
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return tr("just now", "刚刚")
	case d < time.Hour:
		return fmt.Sprintf("%dm%s", int(d.Minutes()), tr(" ago", "前"))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%s", int(d.Hours()), tr(" ago", "前"))
	default:
		return fmt.Sprintf("%dd%s", int(d.Hours()/24), tr(" ago", "前"))
	}
}
