package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"lyon-agent/pkg/agent"
)

// TestModeCommand: bare /mode reports, named modes switch with a
// transcript line, unknown names usage-error, and the header badge
// follows the engine state (plan/bypass visible at a glance).
func TestModeCommand(t *testing.T) {
	m := newTestModel(t)

	m.slash("/mode")
	if !strings.Contains(m.cmdOutText(), "mode: default") {
		t.Fatalf("bare /mode:\n%s", m.cmdOutText())
	}

	m.slash("/mode plan")
	out := m.cmdOutText()
	if !strings.Contains(out, "mode → plan") ||
		!strings.Contains(out, "execution tools disabled") {
		t.Fatalf("switch to plan:\n%s", out)
	}
	if !strings.Contains(m.headerLine(), "[plan]") {
		t.Fatalf("header missing plan badge: %q", stripColor(m.headerLine()))
	}
	if m.sess.Mode() != agent.ModePlan {
		t.Fatalf("engine mode = %s", m.sess.Mode())
	}

	m.slash("/mode bogus")
	if !strings.Contains(m.cmdOutText(), "usage: /mode") {
		t.Fatalf("unknown mode:\n%s", m.cmdOutText())
	}
	if m.sess.Mode() != agent.ModePlan {
		t.Fatal("failed switch must not change the mode")
	}

	m.slash("/mode default")
	if m.sess.Mode() != agent.ModeDefault ||
		strings.Contains(m.headerLine(), "[plan]") {
		t.Fatalf("back to default: mode=%s head=%q", m.sess.Mode(),
			stripColor(m.headerLine()))
	}
	checkView(t, m, "mode command frame")
}

// TestModeShiftTab: shift+tab cycles default → plan → bypass → default,
// with the badge appearing and leaving accordingly.
func TestModeShiftTab(t *testing.T) {
	m := newTestModel(t)
	cycle := func(want agent.Mode) {
		t.Helper()
		m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
		if m.sess.Mode() != want {
			t.Fatalf("cycle → %s, got %s", want, m.sess.Mode())
		}
	}
	cycle(agent.ModePlan)
	if !strings.Contains(m.headerLine(), "[plan]") {
		t.Fatalf("plan badge missing: %q", stripColor(m.headerLine()))
	}
	cycle(agent.ModeBypass)
	if !strings.Contains(m.headerLine(), "[bypass]") ||
		!strings.Contains(m.cmdOutText(), "mode → bypass") {
		t.Fatalf("bypass badge/line: head=%q\n%s",
			stripColor(m.headerLine()), m.cmdOutText())
	}
	cycle(agent.ModeDefault)
	if h := m.headerLine(); strings.Contains(h, "[plan]") ||
		strings.Contains(h, "[bypass]") {
		t.Fatalf("default must show no badge: %q", stripColor(h))
	}
	checkView(t, m, "mode cycle frame")
}
