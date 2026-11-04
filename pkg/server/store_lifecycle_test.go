package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"lyon-agent/pkg/agent"
)

// readAuditFile returns today's audit log under home, or "" when no line
// has been written yet (no Fire → no file).
func readAuditFile(t *testing.T, home string) string {
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

// newLifecycleFixture wires an engine + store under an isolated LYON_HOME
// so audit assertions see only this test's lines. Registered cleanup runs
// store CloseAll before engine Close (LIFO).
func newLifecycleFixture(t *testing.T) (home string, eng *agent.Engine, st *Store) {
	t.Helper()
	home = scratchHome(t)
	t.Setenv("LYON_DISTILL", "")
	eng = agent.NewEngine(agent.DefaultConfig(), nil)
	t.Cleanup(eng.Close)
	st = NewStore(0, 0)
	t.Cleanup(st.CloseAll)
	return home, eng, st
}

func shortHistory() []anthropic.BetaMessageParam {
	return []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(
		anthropic.NewBetaTextBlock("hi"))}
}

// TestStoreCreateFiresSessionStart: Create announces session_start with
// the seat id before any prompt — the server-side trigger point for
// hooks SessionStart.
func TestStoreCreateFiresSessionStart(t *testing.T) {
	home, eng, st := newLifecycleFixture(t)

	id, err := st.Create(eng.NewSession(nil))
	if err != nil {
		t.Fatal(err)
	}
	eng.Hooks().Flush()

	audit := readAuditFile(t, home)
	if !strings.Contains(audit, `"event":"session_start"`) {
		t.Fatalf("session_start missing:\n%s", audit)
	}
	if !strings.Contains(audit, `"session_id":"`+id+`"`) {
		t.Fatalf("session_id=%s missing:\n%s", id, audit)
	}
	if !strings.Contains(audit, `"source":"startup"`) {
		t.Fatalf("source=startup missing:\n%s", audit)
	}
}

// TestStoreRegisterAfterRestoreSingleStart: adopt (Restore → Register)
// lands exactly one session_start — the resume from Restore, not a second
// startup from Register's EnsureSessionStart.
func TestStoreRegisterAfterRestoreSingleStart(t *testing.T) {
	home, eng, st := newLifecycleFixture(t)

	sess := eng.NewSession(nil)
	sess.Restore(agent.Snapshot{
		ID:       "seat-adopt",
		Messages: shortHistory(),
	})
	if err := st.Register(sess, "seat-adopt"); err != nil {
		t.Fatal(err)
	}
	eng.Hooks().Flush()

	audit := readAuditFile(t, home)
	if n := strings.Count(audit, `"event":"session_start"`); n != 1 {
		t.Fatalf("session_start count = %d, want 1:\n%s", n, audit)
	}
	if !strings.Contains(audit, `"source":"resume"`) {
		t.Fatalf("source=resume missing:\n%s", audit)
	}
	if !strings.Contains(audit, `"session_id":"seat-adopt"`) {
		t.Fatalf("session_id=seat-adopt missing:\n%s", audit)
	}
	if !strings.Contains(audit, "1 messages restored") {
		t.Fatalf("restored-count detail missing:\n%s", audit)
	}
}

// TestStoreParkDistills: an unload with history runs the distill pass
// (bounded by the teardown budget) and stamps its trigger into the audit.
// The pass itself is best-effort — a short conversation just records why
// it skipped, with no model call.
func TestStoreParkDistills(t *testing.T) {
	home, eng, st := newLifecycleFixture(t)

	sess := eng.NewSession(nil)
	id, err := st.Create(sess)
	if err != nil {
		t.Fatal(err)
	}
	// One short message: enough for the unload guard to enter the pass,
	// short enough for the min-history gate to stop it before the API.
	sess.Restore(agent.Snapshot{Messages: shortHistory()})

	if !st.Park(id) {
		t.Fatalf("park of %s failed", id)
	}
	eng.Hooks().Flush()

	audit := readAuditFile(t, home)
	if !strings.Contains(audit, `"event":"memory_distill"`) {
		t.Fatalf("memory_distill missing:\n%s", audit)
	}
	if !strings.Contains(audit, "trigger=park result=short") {
		t.Fatalf("trigger=park result=short missing:\n%s", audit)
	}
}

// TestStoreDeleteDistills: Delete tears down with the same pass, labeled
// trigger=delete — the "delete 后不留痕" gap from the backlog.
func TestStoreDeleteDistills(t *testing.T) {
	home, eng, st := newLifecycleFixture(t)

	sess := eng.NewSession(nil)
	id, err := st.Create(sess)
	if err != nil {
		t.Fatal(err)
	}
	sess.Restore(agent.Snapshot{Messages: shortHistory()})

	if !st.Delete(id) {
		t.Fatalf("delete of %s failed", id)
	}
	eng.Hooks().Flush()

	audit := readAuditFile(t, home)
	if !strings.Contains(audit, "trigger=delete result=short") {
		t.Fatalf("trigger=delete result=short missing:\n%s", audit)
	}
}

// TestStoreParkEmptySkipsDistill: an empty seat's teardown has nothing to
// extract — no memory_distill line, only the session_start it earned at
// create time.
func TestStoreParkEmptySkipsDistill(t *testing.T) {
	home, eng, st := newLifecycleFixture(t)

	id, err := st.Create(eng.NewSession(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !st.Park(id) {
		t.Fatalf("park of %s failed", id)
	}
	eng.Hooks().Flush()

	audit := readAuditFile(t, home)
	if strings.Contains(audit, "memory_distill") {
		t.Fatalf("empty session must not distill:\n%s", audit)
	}
	if !strings.Contains(audit, `"event":"session_start"`) {
		t.Fatalf("session_start missing:\n%s", audit)
	}
}
