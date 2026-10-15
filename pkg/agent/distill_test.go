package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"lyon-agent/pkg/hooks"
	"lyon-agent/pkg/memory"
)

func TestParseDistillFacts(t *testing.T) {
	// Plain JSON array.
	got := parseDistillFacts(`[{"name":"a","body":"x"}]`)
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("plain: %+v", got)
	}
	// Fenced, the common model habit.
	got = parseDistillFacts("```json\n[{\"name\":\"b\",\"body\":\"y\",\"hot\":true}]\n```")
	if len(got) != 1 || got[0].Name != "b" || !got[0].Hot {
		t.Fatalf("fenced: %+v", got)
	}
	// Prose around the array.
	got = parseDistillFacts(`Here are the facts: [{"name":"c","body":"z"}] — hope that helps`)
	if len(got) != 1 || got[0].Name != "c" {
		t.Fatalf("prose: %+v", got)
	}
	// Empty answer is a valid outcome, not an error.
	if got = parseDistillFacts("[]"); got == nil || len(got) != 0 {
		t.Fatalf("empty array: %#v", got)
	}
	// Garbage and truncated JSON yield nothing — best-effort by design.
	if got = parseDistillFacts("no facts here"); got != nil {
		t.Fatalf("garbage: %#v", got)
	}
	if got = parseDistillFacts(`[{"name":`); got != nil {
		t.Fatalf("truncated: %#v", got)
	}
}

// newDistillSession builds the minimum Session applyDistill touches: a
// workspace and a hook manager, no client (applyDistill never calls one).
func newDistillSession(t *testing.T) (*Session, string, *memory.Store) {
	t.Helper()
	ws := tempDir(t)
	s := &Session{eng: &Engine{cfg: Config{Workspace: ws}, hooks: hooks.New(ws)}}
	return s, ws, memory.Open(ws)
}

func TestApplyDistill(t *testing.T) {
	s, _, st := newDistillSession(t)

	// Save + slug + bogus scope falls back to project; the report
	// carries the saved name for the visible echo.
	rep := s.applyDistill(st, []distillFact{
		{Name: "Build Cmd", Description: "how to build", Body: "go build ./...", Scope: "bogus"},
	})
	if rep.Saved != 1 || len(rep.Names) != 1 || rep.Names[0] != "build-cmd" || len(rep.Promoted) != 0 {
		t.Fatalf("report = %+v, want 1 saved build-cmd, no promotion", rep)
	}
	f, ok := st.Get(memory.ScopeProject, "build-cmd")
	if !ok || !strings.Contains(f.Body, "go build") || f.Description != "how to build" {
		t.Fatalf("stored: ok=%v %+v", ok, f)
	}

	// Idempotent: exit distills re-run constantly — no-change saves nothing.
	if rep = s.applyDistill(st, []distillFact{
		{Name: "Build Cmd", Description: "how to build", Body: "go build ./...", Scope: "project"},
	}); rep.Saved != 0 {
		t.Fatalf("re-apply saved %d, want 0", rep.Saved)
	}

	// hot=true from the model pins the fact into the resident tier.
	s.applyDistill(st, []distillFact{
		{Name: "toast-pref", Description: "notifications", Body: "use toasts", Scope: "personal", Hot: true},
	})
	if f, _ := st.Get(memory.ScopePersonal, "toast-pref"); !f.Hot {
		t.Fatal("model hot flag not applied")
	}

	// Proven-useful facts (hits >= 2) graduate to hot on re-distill.
	st.Save(memory.Fact{Name: "used-often", Scope: memory.ScopeProject, Body: "v1", Hits: 2})
	if rep = s.applyDistill(st, []distillFact{
		{Name: "used-often", Body: "v2 refined", Scope: "project"},
	}); rep.Saved != 1 {
		t.Fatalf("re-distill saved %d, want 1", rep.Saved)
	}
	if f, _ := st.Get(memory.ScopeProject, "used-often"); !f.Hot || !strings.Contains(f.Body, "refined") {
		t.Fatalf("promotion/update: %+v", f)
	}

	// Empty bodies and nameless/descless facts are dropped.
	if rep = s.applyDistill(st, []distillFact{
		{Body: "   "},
		{Name: "", Description: "", Body: "orphan"},
	}); rep.Saved != 0 {
		t.Fatalf("junk saved %d, want 0", rep.Saved)
	}

	// The per-pass cap keeps a runaway model from flooding the store.
	var flood []distillFact
	for i := 0; i < 10; i++ {
		flood = append(flood, distillFact{Name: "f" + string(rune('a'+i)), Body: "body"})
	}
	if rep = s.applyDistill(st, flood); rep.Saved != distillMaxFacts {
		t.Fatalf("capped saved = %d, want %d", rep.Saved, distillMaxFacts)
	}
}

func TestDistillGates(t *testing.T) {
	// LYON_DISTILL=off short-circuits before any client or history work.
	t.Setenv("LYON_DISTILL", "off")
	s, _, _ := newDistillSession(t)
	if n, err := s.Distill(context.Background()); n != 0 || err != nil {
		t.Fatalf("off: n=%d err=%v", n, err)
	}

	// Enabled but empty history: returns before touching the (zero) client.
	t.Setenv("LYON_DISTILL", "")
	s2, _, _ := newDistillSession(t)
	if n, err := s2.Distill(context.Background()); n != 0 || err != nil {
		t.Fatalf("empty: n=%d err=%v", n, err)
	}

	// Short history also skips — tiny chats have nothing durable to say.
	s3, _, _ := newDistillSession(t)
	s3.seed = []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(
		anthropic.NewBetaTextBlock("just chatting: hi, hello, thanks — " + strings.Repeat("filler ", 20)))}
	if n, err := s3.Distill(context.Background()); n != 0 || err != nil {
		t.Fatalf("short: n=%d err=%v", n, err)
	}
}

// TestLockHistForDistill: bounded wait — a held history mutex (drive still
// settling at exit) times out to false instead of hanging, and the lock is
// usable again once released.
func TestLockHistForDistill(t *testing.T) {
	s := &Session{}
	s.histMu.Lock()
	if s.lockHistForDistill(50 * time.Millisecond) {
		t.Fatal("must not acquire a held histMu")
	}
	s.histMu.Unlock()
	if !s.lockHistForDistill(50 * time.Millisecond) {
		t.Fatal("must acquire a free histMu")
	}
	s.histMu.Unlock()
}
