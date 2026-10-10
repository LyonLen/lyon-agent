package main

import (
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/memory"
	"lyon-agent/pkg/tools"
)

// isolatedMemory points LYON_HOME inside this test's workspace: team and
// personal layers are shared per-process (the package TestMain redirects
// LYON_HOME once for everything), so without this a personal fact saved
// by an earlier test would leak into a later test's table.
func isolatedMemory(t *testing.T) string {
	t.Helper()
	ws := tempDir(t)
	t.Setenv("LYON_HOME", filepath.Join(ws, ".home"))
	return ws
}

// TestTUIMemoryCommands walks the /memory table: render with numbering,
// toggle hot by row number, forget by row number, and bad input pointing
// at usage instead of dying.
func TestTUIMemoryCommands(t *testing.T) {
	cfg := agent.DefaultConfig()
	cfg.Workspace = isolatedMemory(t)
	eng := agent.NewEngine(cfg, nil)
	sess := eng.NewSession(nil)
	t.Cleanup(sess.Close)
	m := newTUIModel(cfg, sess, tools.NewMetrics())
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	st := memory.Open(cfg.Workspace)
	st.Save(memory.Fact{Name: "build-cmd", Description: "build command", Scope: memory.ScopeProject, Body: "go build ./..."})
	st.Save(memory.Fact{Name: "toast-pref", Description: "toast pref", Scope: memory.ScopePersonal, Body: "use toasts"})

	m.slash("/memory")
	if len(m.memRows) != 2 {
		t.Fatalf("memRows = %d, want 2", len(m.memRows))
	}
	got := m.renderTranscript()
	if !strings.Contains(got, "memory  team=") || !strings.Contains(got, "hot=0") {
		t.Fatalf("header missing or wrong:\n%s", got)
	}
	if !strings.Contains(got, "build-cmd") || !strings.Contains(got, "toast-pref") {
		t.Fatalf("rows missing:\n%s", got)
	}
	if !strings.Contains(got, "/memory N") {
		t.Fatalf("usage footer missing:\n%s", got)
	}

	// Row 1 is (project) build-cmd — toggling heats exactly that one.
	m.slash("/memory 1")
	if f, ok := st.Get(memory.ScopeProject, "build-cmd"); !ok || !f.Hot {
		t.Fatalf("row 1 toggle: ok=%v %+v", ok, f)
	}
	if f, _ := st.Get(memory.ScopePersonal, "toast-pref"); f.Hot {
		t.Fatal("row 2 must stay cold")
	}
	if !strings.Contains(m.renderTranscript(), "hot=1") {
		t.Fatalf("table after toggle:\n%s", m.renderTranscript())
	}

	// Forget the second row (project fact is still row 1 after re-render).
	m.slash("/memory forget 1")
	if _, ok := st.Get(memory.ScopeProject, "build-cmd"); ok {
		t.Fatal("forget 1 removed the wrong row")
	}
	if _, ok := st.Get(memory.ScopePersonal, "toast-pref"); !ok {
		t.Fatal("personal fact should have survived")
	}
	if len(m.memRows) != 1 {
		t.Fatalf("memRows after forget = %d, want 1", len(m.memRows))
	}

	// Bad arguments: usage guidance, no panic.
	m.slash("/memory 99")
	if !strings.Contains(m.status, "usage") {
		t.Fatalf("row 99 status: %q", m.status)
	}
	m.slash("/memory banana")
	if !strings.Contains(m.status, "usage") {
		t.Fatalf("bad arg status: %q", m.status)
	}
}

// TestMemoryTablePlain checks the plain-REPL renderer end to end,
// including the numbering contract the subcommands rely on.
func TestMemoryTablePlain(t *testing.T) {
	ws := isolatedMemory(t)
	st := memory.Open(ws)
	st.Save(memory.Fact{Name: "上下文窗口", Description: "context window sizes", Scope: memory.ScopeProject, Body: "200k default"})

	lines, rows := memoryTable(ws)
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "上下文窗口") || !strings.Contains(joined, "context window sizes") {
		t.Fatalf("table:\n%s", joined)
	}
	if !strings.Contains(joined, "sync:") {
		t.Fatalf("sync note missing:\n%s", joined)
	}

	// toggle → forget through the plain handler, using the table's numbering.
	var buf strings.Builder
	handlePlainMemory(&buf, ws, "1")
	if f, _ := st.Get(memory.ScopeProject, "上下文窗口"); !f.Hot {
		t.Fatalf("toggle via plain handler failed:\n%s", buf.String())
	}
	buf.Reset()
	handlePlainMemory(&buf, ws, "forget 1")
	if _, ok := st.Get(memory.ScopeProject, "上下文窗口"); ok {
		t.Fatalf("forget via plain handler failed:\n%s", buf.String())
	}
	buf.Reset()
	handlePlainMemory(&buf, ws, "nonsense")
	if !strings.Contains(buf.String(), "usage") {
		t.Fatalf("bad arg not reported:\n%s", buf.String())
	}
}
