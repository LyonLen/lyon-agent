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
	return paint(cBold, "mode: "+string(m)) + paint(cDim,
		" — /mode default|plan|bypass · shift+tab cycles"+
			modeBlurb(m))
}

func modeBlurb(m agent.Mode) string {
	switch m {
	case agent.ModePlan:
		return " · execution tools disabled (read-only exploration)"
	case agent.ModeBypass:
		return " · approvals auto-approved (policy & hooks still enforced)"
	default:
		return ""
	}
}

// modeSwitchLine confirms a change in the transcript.
func modeSwitchLine(from, to agent.Mode) string {
	if from == to {
		return paint(cDim, "mode: "+string(to)+" (unchanged)")
	}
	head := paint(cGreen, "mode → "+string(to))
	switch to {
	case agent.ModePlan:
		head = paint(cYellow, "mode → plan — execution tools disabled (read-only exploration)")
	case agent.ModeBypass:
		head = paint(cRed, "mode → bypass — approvals auto-approved (policy & hooks still enforced)")
	}
	return head + paint(cDim, "  (/mode "+string(from)+" to go back)")
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
