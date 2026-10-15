package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"lyon-agent/pkg/agent"
)

// TestStepEventRoutesLiveOutput: exec_output chunks append to the step
// carrying their ExecID; unknown ids and transcript storage stay untouched
// (render()/applyEvent deliberately fall through — only stepEvent handles
// this type).
func TestStepEventRoutesLiveOutput(t *testing.T) {
	m := newTestModel(t)
	m.applyEvent(agent.Event{Type: agent.EventToolUseStart, ToolID: "t1", ToolName: "run_shell"})
	m.applyEvent(agent.Event{Type: agent.EventToolExecStart, ExecID: 7, ToolName: "run_shell"})

	linesBefore := len(m.lines)
	m.applyEvent(agent.Event{Type: agent.EventToolExecOutput, ExecID: 7, Text: "line1\n"})
	m.applyEvent(agent.Event{Type: agent.EventToolExecOutput, ExecID: 7, Text: "line2\n"})

	s := m.steps[len(m.steps)-1]
	if s.execID != 7 || s.out != "line1\nline2\n" {
		t.Fatalf("step.out = %q (execID %d)", s.out, s.execID)
	}
	if len(m.lines) != linesBefore {
		t.Fatalf("output events pushed %d transcript lines", len(m.lines)-linesBefore)
	}

	// Unknown exec id: dropped, no phantom step, no state change.
	m.applyEvent(agent.Event{Type: agent.EventToolExecOutput, ExecID: 999, Text: "ghost\n"})
	if len(m.steps) != 1 || m.steps[0].out != "line1\nline2\n" {
		t.Fatalf("ghost chunk landed: steps=%d out=%q", len(m.steps), m.steps[0].out)
	}
}

// TestStepAppendOutputIgnoresBlank: execID 0 and empty text are no-ops.
func TestStepAppendOutputIgnoresBlank(t *testing.T) {
	m := newTestModel(t)
	s := m.addStep(stepTool, "", "run_shell", "x")
	s.execID = 5
	m.stepAppendOutput(0, "nope")
	m.stepAppendOutput(5, "")
	m.stepAppendOutput(6, "nope")
	if s.out != "" {
		t.Fatalf("blank routing changed out: %q", s.out)
	}
}

// TestStepAppendOutputCaps: the per-step tail never exceeds stepOutMax —
// newest bytes win, and a cut inside a line is trimmed to the next newline
// when one lands nearby (no half-line head).
func TestStepAppendOutputCaps(t *testing.T) {
	m := newTestModel(t)
	s := m.addStep(stepTool, "", "run_shell", "big")
	s.execID = 3

	// Text over the cap whose cut edge sits mid-"abcd…": the newline 4
	// bytes into the cut triggers the edge trim, so "abcd" is dropped and
	// exactly the trailing run survives.
	b := strings.Repeat("b", stepOutMax-5)
	text := strings.Repeat("x", 100) + "abcd\n" + b // len = stepOutMax+100
	m.stepAppendOutput(3, text)
	if s.out != b {
		t.Fatalf("cut edge not trimmed: len=%d want=%d, head=%q",
			len(s.out), len(b), s.out[:min(20, len(s.out))])
	}

	// No newline anywhere in sight of the cut: keep the bytes as they are.
	m.stepAppendOutput(3, strings.Repeat("z", 500)+s.out)
	if len(s.out) != stepOutMax || !strings.HasSuffix(s.out, strings.Repeat("b", 100)) {
		t.Fatalf("cap lost newest bytes: len=%d", len(s.out))
	}
}

// TestStepExpandLiveOutput: the open step shows its output tail between
// the capped detail rows and the pinned meta rows, newest wins, and the
// cut is marked.
func TestStepExpandLiveOutput(t *testing.T) {
	m := newTestModel(t)
	s := m.addStep(stepTool, "", "run_shell", "go test ./...")
	s.execID = 7

	// No output yet: detail + started, meta last.
	rows := m.stepExpandRows(*s, 60)
	plain := stripColor(strings.Join(rows, "\n"))
	if !strings.Contains(plain, "go test ./...") {
		t.Fatalf("detail missing:\n%s", plain)
	}
	if !strings.Contains(stripColor(rows[len(rows)-1]), "started") {
		t.Fatalf("meta row not pinned last: %q", rows[len(rows)-1])
	}

	m.stepAppendOutput(7, "ok pkg/a 12ms\nFAIL pkg/b\n")
	rows = m.stepExpandRows(*s, 60)
	plain = stripColor(strings.Join(rows, "\n"))
	if !strings.Contains(plain, "FAIL pkg/b") || !strings.Contains(plain, "go test ./...") {
		t.Fatalf("output not between detail and meta:\n%s", plain)
	}
	if !strings.Contains(stripColor(rows[len(rows)-1]), "started") {
		t.Fatalf("output pushed meta off the bottom: %q", rows[len(rows)-1])
	}
	for _, r := range rows {
		if ansi.StringWidth(r) > 60 {
			t.Fatalf("row overflows 60 cells: %q", r)
		}
	}
	if len(rows) > expandMaxRows {
		t.Fatalf("rows = %d > %d", len(rows), expandMaxRows)
	}

	// Overflow: newest kept, oldest dropped, cut marked (both languages
	// share the "…(-N …)" shape).
	var sb strings.Builder
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&sb, "out-%02d\n", i)
	}
	m.stepAppendOutput(7, sb.String())
	rows = m.stepExpandRows(*s, 60)
	plain = stripColor(strings.Join(rows, "\n"))
	if !strings.Contains(plain, "out-40") {
		t.Fatalf("newest line missing:\n%s", plain)
	}
	if strings.Contains(plain, "out-01") {
		t.Fatalf("oldest line should be trimmed:\n%s", plain)
	}
	if !strings.Contains(plain, "…(-") {
		t.Fatalf("trim marker missing:\n%s", plain)
	}
	if !strings.Contains(stripColor(rows[len(rows)-1]), "started") {
		t.Fatalf("meta row not pinned last: %q", rows[len(rows)-1])
	}
}

// TestStepExpandOutputSanitizes: CRLF keeps the line text, a lone \r lets
// the overwrite win, ANSI escapes never reach the panel.
func TestStepExpandOutputSanitizes(t *testing.T) {
	m := newTestModel(t)
	s := m.addStep(stepTool, "", "run_shell", "prog")
	s.execID = 1
	m.stepAppendOutput(1, "50%\r100%\r\n\x1b[31mred\x1b[0m\n")

	plain := stripColor(strings.Join(m.stepExpandRows(*s, 60), "\n"))
	if !strings.Contains(plain, "100%") {
		t.Fatalf("carriage-return overwrite lost:\n%q", plain)
	}
	if strings.Contains(plain, "50%") {
		t.Fatalf("stale progress text survived:\n%q", plain)
	}
	if !strings.Contains(plain, "red") || strings.Contains(plain, "\x1b") {
		t.Fatalf("ANSI not stripped:\n%q", plain)
	}
}
