package main

import (
	"fmt"
	"strings"

	"lyon-agent/pkg/agent"
)

// /mode — plan / bypass / default switching, shared by the TUI (slash
// command, shift+tab cycle, header badge) and the plain REPL. The engine
// owns enforcement (preTool read-only gate, bypass auto-approve); this
// file is only the surface: parsing, wording, badges.

// modeStatusLine answers bare /mode.
func modeStatusLine(m agent.Mode) string {
	return paint(cBold, tr("mode: ", "模式: ")+string(m)) + paint(cDim,
		tr(" — /mode default|plan|bypass · shift+tab cycles",
			" — /mode default|plan|bypass · shift+tab 循环切换")+
			modeBlurb(m))
}

func modeBlurb(m agent.Mode) string {
	switch m {
	case agent.ModePlan:
		return tr(" · execution tools disabled (read-only exploration)",
			" · 执行类工具已禁用（只读探索）")
	case agent.ModeBypass:
		return tr(" · approvals auto-approved (policy & hooks still enforced)",
			" · 审批自动通过（策略与钩子仍生效）")
	default:
		return tr(" · approvals ask y/n (chat default; -approve=false disables)",
			" · 审批询问 y/n（聊天默认；-approve=false 关闭）")
	}
}

// modeSwitchLine confirms a change in the transcript.
func modeSwitchLine(from, to agent.Mode) string {
	if from == to {
		return paint(cDim, tr("mode: ", "模式: ")+string(to)+tr(" (unchanged)", "（未变）"))
	}
	head := paint(cGreen, "mode → "+string(to))
	switch to {
	case agent.ModePlan:
		head = paint(cYellow, tr("mode → plan — execution tools disabled (read-only exploration)",
			"mode → plan — 执行类工具已禁用（只读探索）"))
	case agent.ModeBypass:
		head = paint(cRed, tr("mode → bypass — approvals auto-approved (policy & hooks still enforced)",
			"mode → bypass — 审批自动通过（策略与钩子仍生效）"))
	case agent.ModeDefault:
		head = paint(cGreen, tr("mode → default — shell/write/edit ask y/n again (bypass skips them)",
			"mode → default — shell/write/edit 恢复询问 y/n（bypass 会跳过）"))
	}
	return head + paint(cDim, tr("  (/mode "+string(from)+" to go back)",
		"  (/mode "+string(from)+" 可切回)"))
}

// modeBadge pins the active mode into the TUI header — plan/bypass must
// be visible at a glance, always.
func modeBadge(m agent.Mode) string {
	switch m {
	case agent.ModePlan:
		return paint(cYellow, " · [plan]")
	case agent.ModeBypass:
		return paint(cRed, " · [bypass]")
	default:
		return ""
	}
}

// cycleMode walks default → plan → bypass → default (shift+tab).
func cycleMode(m agent.Mode) agent.Mode {
	switch m {
	case agent.ModeDefault:
		return agent.ModePlan
	case agent.ModePlan:
		return agent.ModeBypass
	default:
		return agent.ModeDefault
	}
}

// modeAction handles /mode for both frontends: bare prints status, a
// name switches, anything else is a usage error. Returns the line to
// display ("" on success — the switch line is pushed separately by the
// caller only when needed; here it is simpler to return one result line).
func modeAction(sess *agent.Session, args string) string {
	arg := strings.TrimSpace(args)
	if arg == "" {
		return modeStatusLine(sess.Mode())
	}
	prev, err := sess.SetMode(agent.Mode(arg))
	if err != nil {
		return paint(cYellow, err.Error()+" — usage: /mode [default|plan|bypass]")
	}
	return modeSwitchLine(prev, agent.Mode(arg))
}

// modeSummary is the compact one-liner for tool output (REPL echo).
func modeSummary(m agent.Mode) string {
	return fmt.Sprintf("mode=%s%s", m, modeBlurb(m))
}
