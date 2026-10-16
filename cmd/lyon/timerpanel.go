// /timer (#8): the TUI surface of the in-process scheduler —
// add/list/enable/disable/rm over $LYON_HOME/timers. Every read is a
// local store snapshot and every write a tiny JSON file, so the command
// never blocks the render loop; OS-level registration is deliberately
// CLI-only (`lyon timer register …`).

package main

import (
	"strings"
	"time"

	"lyon-agent/pkg/timer"
)

// timerCommand backs the /timer slash command. Bare (and "list") render
// the store snapshot — next fire, status, run count per task — into the
// command-output area; the mutating verbs delegate to the scheduler-aware
// helpers so a live heap is re-armed or dropped in the same call.
func (m *tuiModel) timerCommand(args string) {
	args = strings.TrimSpace(args)
	verb, rest, _ := strings.Cut(args, " ")
	rest = strings.TrimSpace(rest)
	switch verb {
	case "", "list":
		lines, err := timerListLines(time.Now())
		if err != nil {
			m.pushCmd(paint(cRed, "timer list: "+err.Error()))
			return
		}
		m.pushCmd(lines...)
	case "add":
		task, err := parseTimerAdd(rest, m.workspace, time.Now())
		if err != nil {
			m.pushCmd(paint(cYellow, "timer add: "+err.Error()))
			return
		}
		if err := addTimerTask(task); err != nil {
			m.pushCmd(paint(cRed, "timer add: "+err.Error()))
			return
		}
		m.pushCmd(paint(cGreen, tr("timer added", "定时器已添加")) + "  " +
			truncRunes(task.ID+": "+timerDescribe(task, time.Now()), 96))
	case "enable", "disable":
		if rest == "" {
			m.pushCmd(timerUsageLines()...)
			return
		}
		if err := setTimerEnabled(rest, verb == "enable"); err != nil {
			m.pushCmd(paint(cRed, "timer "+verb+": "+err.Error()))
			return
		}
		t, err := timer.Load(rest)
		desc := rest
		if err == nil {
			desc = truncRunes(timerDescribe(t, time.Now()), 96)
		}
		what := tr("disabled", "已停用")
		if verb == "enable" {
			what = tr("enabled", "已启用")
		}
		m.pushCmd(paint(cGreen, tr("timer ", "定时器 ")+rest+" "+what) + "  " + desc)
	case "rm", "remove", "delete":
		if rest == "" {
			m.pushCmd(timerUsageLines()...)
			return
		}
		if err := removeTimerTask(rest); err != nil {
			m.pushCmd(paint(cRed, "timer rm: "+err.Error()))
			return
		}
		m.pushCmd(paint(cGreen, tr("timer removed: ", "定时器已删除: ")) + rest)
	default:
		m.pushCmd(timerUsageLines()...)
	}
}

// timerUsageLines is the /timer help body, bilingual with the rest of
// the UI (lang rebuild covers it — these are built per call).
func timerUsageLines() []string {
	return []string{
		paint(cBold, tr("usage:", "用法：")),
		"  " + tr("/timer                    list timers (next fire, status, runs)",
			"/timer                    列出定时器（下次触发、状态、次数）"),
		"  " + tr("/timer add <id> <once|every|cron> <spec> <command|prompt> <text> [k=v…]",
			"/timer add <id> <once|every|cron> <规格> <command|prompt> <内容> [k=v…]"),
		"  " + tr("e.g. /timer add beat every 5m command echo hi",
			"例：/timer add beat every 5m command echo hi"),
		"  " + tr("     /timer add standup cron 0 9 * * 1-5 prompt draft the standup notes",
			"     /timer add standup cron 0 9 * * 1-5 prompt 起草站会纪要"),
		"  " + tr("     /timer add remind once +30m prompt stretch",
			"     /timer add remind once +30m prompt 该休息了"),
		"  " + tr("     modifiers: ws=<dir> overlap=skip|queue miss=skip|once timeout=90s",
			"     修饰符：ws=<目录> overlap=skip|queue miss=skip|once timeout=90s"),
		"  " + tr("/timer enable <id> · /timer disable <id> · /timer rm <id>",
			"/timer enable <id> · /timer disable <id> · /timer rm <id>"),
		"  " + tr("OS-level (after lyon exits): lyon timer register <id> — see lyon timer help",
			"系统级（lyon 退出后仍触发）: lyon timer register <id> — 见 lyon timer help"),
	}
}
