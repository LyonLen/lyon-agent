package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"lyon-agent/internal/mockapi"
)

// TestDistillAuditTrace: every non-off exit of the distill pass writes one
// memory_distill line carrying its trigger and result — "did distillation
// even run, and why not?" must be answerable from the audit log alone (the
// gap the backlog called "audit 里没有蒸馏痕迹"). LYON_DISTILL=off stays
// silent: the gate is a silence switch, not a failure worth recording.
func TestDistillAuditTrace(t *testing.T) {
	longSeed := func() []anthropic.BetaMessageParam {
		return []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(
			anthropic.NewBetaTextBlock(strings.Repeat("filler ", 100)))}
	}

	// off: with enough history to otherwise reach the (nil) client, only
	// the gate can keep this quiet — a missing line is the assertion.
	t.Run("off", func(t *testing.T) {
		home := tempDir(t)
		t.Setenv("LYON_HOME", home)
		t.Setenv("LYON_DISTILL", "off")
		s, _, _ := newDistillSession(t)
		s.seed = longSeed()
		rep, err := s.DistillAt(context.Background(), "exit")
		if rep.Saved != 0 || err != nil {
			t.Fatalf("off: rep=%+v err=%v", rep, err)
		}
		s.eng.hooks.Flush()
		if a := readAudit(t, home); strings.Contains(a, "memory_distill") {
			t.Fatalf("LYON_DISTILL=off must not audit:\n%s", a)
		}
	})

	// short: below distillMinHistory — the line says so, named for the
	// caller that asked (DistillWithReport → the /remember trigger).
	t.Run("short", func(t *testing.T) {
		home := tempDir(t)
		t.Setenv("LYON_HOME", home)
		t.Setenv("LYON_DISTILL", "")
		s, _, _ := newDistillSession(t)
		s.seed = []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(
			anthropic.NewBetaTextBlock("hi"))}
		rep, err := s.DistillWithReport(context.Background())
		if rep.Saved != 0 || err != nil {
			t.Fatalf("short: rep=%+v err=%v", rep, err)
		}
		s.eng.hooks.Flush()
		a := readAudit(t, home)
		if !strings.Contains(a, `"event":"memory_distill"`) ||
			!strings.Contains(a, "trigger=remember result=short") {
			t.Fatalf("short-skip audit missing:\n%s", a)
		}
	})

	// unchanged: same history as the last pass — the fingerprint gate
	// skips the model call and says why (server reaps re-distill often).
	t.Run("unchanged", func(t *testing.T) {
		home := tempDir(t)
		t.Setenv("LYON_HOME", home)
		t.Setenv("LYON_DISTILL", "")
		s, _, _ := newDistillSession(t)
		s.seed = longSeed()
		s.mu.Lock()
		s.distillHash = distillKey(renderHistory(s.seed))
		s.mu.Unlock()
		rep, err := s.DistillAt(context.Background(), "reap")
		if rep.Saved != 0 || err != nil {
			t.Fatalf("unchanged: rep=%+v err=%v", rep, err)
		}
		s.eng.hooks.Flush()
		a := readAudit(t, home)
		if !strings.Contains(a, "trigger=reap result=unchanged") {
			t.Fatalf("unchanged-skip audit missing:\n%s", a)
		}
	})

	// saved: a real extraction pass through the mock gateway — the line
	// carries the count, and each write keeps its own memory_save.
	t.Run("saved", func(t *testing.T) {
		home := tempDir(t)
		t.Setenv("LYON_HOME", home)
		t.Setenv("LYON_DISTILL", "")
		newCompactMock(t, []mockapi.Turn{
			{Text: []string{`[{"name":"audit-fact","description":"d","body":"b","scope":"project"}]`}},
		})
		eng := NewEngine(Config{Workspace: tempDir(t), CompactThreshold: 0.9}, nil)
		t.Cleanup(eng.Close)
		sess := eng.NewSession(nil)
		defer sess.Close()
		sess.seed = longSeed()

		rep, err := sess.DistillAt(context.Background(), "park")
		if err != nil {
			t.Fatalf("saved: %v", err)
		}
		if rep.Saved != 1 || len(rep.Names) != 1 || rep.Names[0] != "audit-fact" {
			t.Fatalf("saved: rep=%+v, want 1 audit-fact", rep)
		}
		eng.Hooks().Flush()
		a := readAudit(t, home)
		if !strings.Contains(a, "trigger=park result=saved=1") {
			t.Fatalf("saved audit missing:\n%s", a)
		}
		if !strings.Contains(a, `"event":"memory_save"`) ||
			!strings.Contains(a, `"tool":"audit-fact"`) {
			t.Fatalf("memory_save line missing:\n%s", a)
		}
	})
}
