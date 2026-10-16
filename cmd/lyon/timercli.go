// `lyon timer` (#8): the non-interactive timer surface. Task-file
// management (add/list/enable/disable/rm) mirrors the TUI's /timer
// exactly — the same parse line, the same store helpers — and the
// register/unregister/os-list verbs drive pkg/timer's OS backend
// (Windows schtasks / POSIX crontab), which is the whole point of the
// CLI: configuring unattended timers without a chat session open.

package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"lyon-agent/pkg/timer"
)

func cmdTimer(args []string) int {
	if len(args) == 0 {
		timerCliUsage(os.Stderr)
		return 2
	}
	switch args[0] {
	case "help", "-h", "--help":
		timerCliUsage(os.Stdout)
		return 0

	case "add":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, paint(cYellow, "timer add: needs an id — lyon timer add <id> …"))
			return 2
		}
		ws, err := os.Getwd()
		if err != nil {
			ws = "" // prompt tasks then inherit the launcher's cwd at fire time
		}
		task, err := parseTimerAdd(strings.Join(args[1:], " "), ws, time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, paint(cYellow, "timer add: "+err.Error()))
			return 2
		}
		if err := addTimerTask(task); err != nil {
			fmt.Fprintln(os.Stderr, paint(cRed, "timer add: "+err.Error()))
			return 1
		}
		fmt.Println(paint(cGreen, tr("added", "已添加")) + "  " +
			truncRunes(task.ID+": "+timerDescribe(task, time.Now()), 96))
		return 0

	case "list":
		lines, err := timerListLines(time.Now())
		if err != nil {
			fmt.Fprintln(os.Stderr, paint(cRed, "timer list: "+err.Error()))
			return 1
		}
		for _, l := range lines {
			fmt.Println(l)
		}
		return 0

	case "enable", "disable":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, paint(cYellow, "usage: lyon timer "+args[0]+" <id>"))
			return 2
		}
		if err := setTimerEnabled(args[1], args[0] == "enable"); err != nil {
			fmt.Fprintln(os.Stderr, paint(cRed, "timer "+args[0]+": "+err.Error()))
			return 1
		}
		t, err := timer.Load(args[1])
		desc := args[1]
		if err == nil {
			desc = truncRunes(timerDescribe(t, time.Now()), 96)
		}
		what := tr("disabled", "已停用")
		if args[0] == "enable" {
			what = tr("enabled", "已启用")
		}
		fmt.Println(paint(cGreen, tr("timer ", "定时器 ")+args[1]+" "+what) + "  " + desc)
		fmt.Println(paint(cDim, tr(
			"note: a running 'lyon chat' re-arms at startup and re-saves its in-memory state — manage the live scheduler via /timer there",
			"注意：已在运行的 'lyon chat' 启动时才重新装载，且会按内存状态回写 — 实时调度请在其中用 /timer 管理")))
		return 0

	case "rm", "remove", "delete":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, paint(cYellow, "usage: lyon timer rm <id>"))
			return 2
		}
		// OS tasks outlive the file that inspired them — a best-effort
		// check so `rm` never silently strands a live schtasks entry.
		warnOSRegistered(args[1])
		if err := removeTimerTask(args[1]); err != nil {
			fmt.Fprintln(os.Stderr, paint(cRed, "timer rm: "+err.Error()))
			return 1
		}
		fmt.Println(paint(cGreen, tr("removed: ", "已删除: ")) + args[1])
		return 0

	case "register":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, paint(cYellow, "usage: lyon timer register <id>"))
			return 2
		}
		t, err := timer.Load(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, paint(cRed, "timer register: "+err.Error()))
			return 1
		}
		if err := timer.NewBackend().Register(t); err != nil {
			fmt.Fprintln(os.Stderr, paint(cRed, "timer register: "+err.Error()))
			return 1
		}
		fmt.Println(paint(cGreen, tr("registered OS task ", "已注册系统任务 ")) + timer.OSTaskName(t.ID))
		if t.Enabled {
			fmt.Println(paint(cDim, tr(
				"note: this task is also enabled in-process — 'lyon timer disable "+t.ID+"' to fire only through the OS",
				"注意：该任务同时启用了进程内调度 — 'lyon timer disable "+t.ID+"' 可只走系统调度")))
		}
		return 0

	case "unregister":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, paint(cYellow, "usage: lyon timer unregister <id>"))
			return 2
		}
		if err := timer.NewBackend().Unregister(args[1]); err != nil {
			fmt.Fprintln(os.Stderr, paint(cRed, "timer unregister: "+err.Error()))
			return 1
		}
		fmt.Println(paint(cGreen, tr("unregistered: ", "已注销: ")) + timer.OSTaskName(args[1]))
		return 0

	case "os-list":
		ids, err := timer.NewBackend().List()
		if err != nil {
			fmt.Fprintln(os.Stderr, paint(cRed, "timer os-list: "+err.Error()))
			return 1
		}
		if len(ids) == 0 {
			fmt.Println(paint(cDim, tr("no OS-registered timers", "无系统级定时器")))
			return 0
		}
		fmt.Println(paint(cBold, tr("OS-registered timers", "系统级定时器")))
		for _, id := range ids {
			fmt.Println("  " + timer.OSTaskName(id))
		}
		return 0

	default:
		fmt.Fprintln(os.Stderr, paint(cRed, tr(
			fmt.Sprintf("unknown timer subcommand %q", args[0]),
			fmt.Sprintf("未知 timer 子命令 %q", args[0]))))
		fmt.Fprintln(os.Stderr)
		timerCliUsage(os.Stderr)
		return 2
	}
}

// warnOSRegistered notes — best effort, never fatal — whether an OS task
// still carries this id, so `rm` does not strand a live schtasks/crontab
// entry without a word. Any query failure simply skips the hint.
func warnOSRegistered(id string) {
	ids, err := timer.NewBackend().List()
	if err != nil {
		return
	}
	for _, have := range ids {
		if have == id {
			fmt.Println(paint(cYellow, tr(
				"note: OS task "+timer.OSTaskName(id)+" is still registered — lyon timer unregister "+id,
				"注意：系统任务 "+timer.OSTaskName(id)+" 仍注册着 — lyon timer unregister "+id)))
			return
		}
	}
}

// timerCliUsage is `lyon timer help` — the subcommand table plus the
// shared add-line syntax.
func timerCliUsage(w io.Writer) {
	fmt.Fprint(w, tr(`lyon timer — manage timers (#8)

Usage:
  lyon timer add <id> <once|every|cron> <spec> <command|prompt> <text> [k=v…]
                                create a task file (same line as the TUI's
                                /timer add); k=v: ws= overlap= miss= timeout=
  lyon timer list               stored tasks: next fire, status, run count
  lyon timer enable|disable <id>
                                toggle in-process scheduling
  lyon timer rm <id>            delete the task file (warns if OS-registered)
  lyon timer register <id>      OS-level: schtasks (Windows) / crontab (POSIX)
  lyon timer unregister <id>    remove the OS-level registration
  lyon timer os-list            ids currently registered with the OS

Examples:
  lyon timer add beat every 5m command "echo hi"
  lyon timer add standup cron "0 9 * * 1-5" prompt draft the standup notes
  lyon timer add remind once +30m prompt stretch
`, `lyon timer — 管理定时器 (#8)

用法:
  lyon timer add <id> <once|every|cron> <规格> <command|prompt> <内容> [k=v…]
                                创建任务文件（与 TUI 的 /timer add 同一行语法）；
                                k=v：ws= overlap= miss= timeout=
  lyon timer list               已存任务：下次触发、状态、运行次数
  lyon timer enable|disable <id>
                                启用/停用进程内调度
  lyon timer rm <id>            删除任务文件（若仍注册系统级会提示）
  lyon timer register <id>      系统级注册：schtasks（Windows）/ crontab（POSIX）
  lyon timer unregister <id>    注销系统级注册
  lyon timer os-list            列出已注册到系统调度器的 id

示例:
  lyon timer add beat every 5m command "echo hi"
  lyon timer add standup cron "0 9 * * 1-5" prompt 起草站会纪要
  lyon timer add remind once +30m prompt 该休息了
`))
}
