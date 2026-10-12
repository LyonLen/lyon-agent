package main

import (
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"lyon-agent/pkg/agent"
)

// TestRollbackCommand: the idle command surface — empty stack, usage
// errors, manual save, and the newest-first list. Restore itself runs
// through the engine tests (pkg/agent/rollback_test.go); the deferred
// busy path reuses the /resume mechanism already covered elsewhere.
func TestRollbackCommand(t *testing.T) {
	m := newTestModel(t)

	m.slash("/rollback")
	if !strings.Contains(m.cmdOutText(), "no checkpoint") {
		t.Fatalf("empty stack not reported:\n%s", m.cmdOutText())
	}

	// No history yet: /rollback save fails cleanly too.
	m.slash("/rollback save before-risky-work")
	if !strings.Contains(m.cmdOutText(), "no history yet") {
		t.Fatalf("save without history:\n%s", m.cmdOutText())
	}

	m.slash("/rollback 0")
	if !strings.Contains(m.cmdOutText(), "usage: /rollback") {
		t.Fatalf("bad slot not reported:\n%s", m.cmdOutText())
	}
	m.slash("/rollback abc")
	if !strings.Contains(m.cmdOutText(), "usage: /rollback") {
		t.Fatalf("non-numeric slot not reported:\n%s", m.cmdOutText())
	}

	// Seed a real history through the public Restore path, then mark two
	// manual checkpoints (no network needed — Checkpoint just copies).
	m.sess.Restore(agent.Snapshot{Messages: []anthropic.BetaMessageParam{
		anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("seeded conversation")),
	}})
	m.slash("/rollback save third question")
	if !strings.Contains(m.cmdOutText(), "★ checkpoint #1 saved") {
		t.Fatalf("save not confirmed:\n%s", m.cmdOutText())
	}
	m.slash("/rollback save second question")

	m.slash("/rollback")
	out := m.cmdOutText()
	if !strings.Contains(out, "checkpoints (2, newest first)") ||
		!strings.Contains(out, "third question") ||
		!strings.Contains(out, "/rollback 1") {
		t.Fatalf("checkpoint list:\n%s", out)
	}

	// Out-of-range slot names the count instead of restoring anything.
	m.slash("/rollback 9")
	if !strings.Contains(m.cmdOutText(), "no slot 9") {
		t.Fatalf("out-of-range slot:\n%s", m.cmdOutText())
	}
}
