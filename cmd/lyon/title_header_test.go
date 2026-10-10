package main

import (
	"strings"
	"testing"

	"lyon-agent/pkg/agent"
)

// TestHeaderShowsTitle: the pinned header appends the session title once
// one is recognized, and stays clean while there is none.
func TestHeaderShowsTitle(t *testing.T) {
	m := newTestModel(t)
	if got := m.headerLine(); strings.Contains(got, "·") {
		t.Fatalf("fresh header must have no title:\n%s", got)
	}
	// Titles arrive via snapshot restore (resume) or the async check;
	// either way headerLine reads it live off the session.
	m.sess.Restore(agent.Snapshot{Title: "修复登录页布局"})
	got := m.headerLine()
	if !strings.Contains(got, "修复登录页布局") {
		t.Fatalf("title missing from header:\n%s", got)
	}
	if !strings.Contains(got, "lyon chat") {
		t.Fatalf("base header lost:\n%s", got)
	}
	// View stays exactly m.height rows with the longer header.
	if rows := strings.Count(m.View(), "\n") + 1; rows != m.height {
		t.Fatalf("view rows = %d, want %d", rows, m.height)
	}
}
