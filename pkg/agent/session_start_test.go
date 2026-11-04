package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"lyon-agent/internal/mockapi"
)

// readAudit returns today's audit log under home, or "" when no line has
// been written yet (no Fire → no file).
func readAudit(t *testing.T, home string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, "audit", time.Now().Format("2006-01-02")+".jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("audit file: %v", err)
	}
	return string(b)
}

// TestSessionStartStartupOnFirstPrompt: a never-restored session announces
// session_start (source=startup) exactly once, ahead of its first
// prompt_start — the audit reads create → prompt, and a second prompt does
// not announce again.
func TestSessionStartStartupOnFirstPrompt(t *testing.T) {
	home := tempDir(t)
	t.Setenv("LYON_HOME", home)
	newCompactMock(t, []mockapi.Turn{
		{Text: []string{"one"}},
		{Text: []string{"two"}},
	})
	eng := NewEngine(Config{Workspace: tempDir(t), CompactThreshold: 0.9}, nil)
	t.Cleanup(eng.Close)
	sess := eng.NewSession(nil)
	defer sess.Close()

	if err := sess.Submit("first question"); err != nil {
		t.Fatal(err)
	}
	waitPromptEndCollect(t, sess)
	if err := sess.Submit("second question"); err != nil {
		t.Fatal(err)
	}
	waitPromptEndCollect(t, sess)

	eng.Hooks().Flush()
	audit := readAudit(t, home)
	if n := strings.Count(audit, `"event":"session_start"`); n != 1 {
		t.Fatalf("session_start count = %d, want 1:\n%s", n, audit)
	}
	if !strings.Contains(audit, `"source":"startup"`) {
		t.Fatalf("source=startup missing:\n%s", audit)
	}
	// No seat on a TUI-shaped session: the bgKey fallback keeps the line
	// correlatable instead of session_id-less.
	if !strings.Contains(audit, `"session_id":"session-`) {
		t.Fatalf("session_id (bgKey fallback) missing:\n%s", audit)
	}
	is := strings.Index(audit, `"event":"session_start"`)
	ip := strings.Index(audit, `"event":"prompt_start"`)
	if is < 0 || ip < 0 || is > ip {
		t.Fatalf("session_start must precede the first prompt_start (is=%d ip=%d):\n%s", is, ip, audit)
	}
}

// TestSessionStartRestoreSources: content (or a known seat id) announces
// resume with the restored count; an empty snapshot announces clear; the
// latch keeps each session to exactly one line — a later first prompt on
// the restored session stays silent.
func TestSessionStartRestoreSources(t *testing.T) {
	home := tempDir(t)
	t.Setenv("LYON_HOME", home)
	eng := NewEngine(Config{Workspace: tempDir(t)}, nil)
	t.Cleanup(eng.Close)

	// resume: content + seat id ride in the snapshot.
	sess := eng.NewSession(nil)
	defer sess.Close()
	sess.Restore(Snapshot{
		ID: "seat-resume",
		Messages: []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(
			anthropic.NewBetaTextBlock("history line"))},
	})
	// Latch: a later first prompt must not announce again.
	sess.EnsureSessionStart()

	// clear: empty snapshot (Reset // /new).
	sess2 := eng.NewSession(nil)
	defer sess2.Close()
	sess2.Restore(Snapshot{})

	eng.Hooks().Flush()
	audit := readAudit(t, home)
	if n := strings.Count(audit, `"event":"session_start"`); n != 2 {
		t.Fatalf("session_start count = %d, want 2 (resume + clear):\n%s", n, audit)
	}
	if !strings.Contains(audit, `"source":"resume"`) ||
		!strings.Contains(audit, `"session_id":"seat-resume"`) ||
		!strings.Contains(audit, "1 messages restored") {
		t.Fatalf("resume line incomplete:\n%s", audit)
	}
	if !strings.Contains(audit, `"source":"clear"`) {
		t.Fatalf("clear line missing:\n%s", audit)
	}
}
