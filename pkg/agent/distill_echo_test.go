package agent

import (
	"testing"

	"lyon-agent/pkg/memory"
)

// TestDistillReportEcho: applyDistill's report feeds the one-line echo —
// fact count, fact names, and the names that entered the hot tier, in
// both languages. An empty report renders no line at all so callers keep
// their own "nothing durable" wording.
func TestDistillReportEcho(t *testing.T) {
	if got := (DistillReport{}).EchoLine(); got != "" {
		t.Fatalf("empty report must render no line, got %q", got)
	}

	s, _, st := newDistillSession(t)

	// One plain fact + one born-hot fact: names and the promotion show.
	rep := s.applyDistill(st, []distillFact{
		{Name: "Build Cmd", Description: "how to build", Body: "go build ./...", Scope: "project"},
		{Name: "toast-pref", Description: "notifications", Body: "use toasts", Scope: "personal", Hot: true},
	})
	if rep.Saved != 2 || len(rep.Names) != 2 || len(rep.Promoted) != 1 || rep.Promoted[0] != "toast-pref" {
		t.Fatalf("report = %+v, want 2 saved with toast-pref promoted", rep)
	}
	wantEN := "memory: +2 facts (build-cmd, toast-pref) · 1 promoted to hot: toast-pref"
	if got := rep.EchoLine(); got != wantEN {
		t.Fatalf("en echo = %q, want %q", got, wantEN)
	}
	wantZH := "memory: +2 条事实（build-cmd, toast-pref） · 1 条晋升热层：toast-pref"
	if got := rep.EchoLineZH(); got != wantZH {
		t.Fatalf("zh echo = %q, want %q", got, wantZH)
	}

	// No promotion this pass → the tail is omitted, not dangled.
	rep = s.applyDistill(st, []distillFact{
		{Name: "plain-fact", Body: "body", Scope: "project"},
	})
	if rep.Saved != 1 || len(rep.Promoted) != 0 {
		t.Fatalf("report = %+v, want 1 saved, no promotion", rep)
	}
	if got, want := rep.EchoLine(), "memory: +1 facts (plain-fact)"; got != want {
		t.Fatalf("echo without promotion = %q, want %q", got, want)
	}

	// Hits-based graduation counts as a promotion…
	st.Save(memory.Fact{Name: "used-often", Scope: memory.ScopeProject, Body: "v1", Hits: 2})
	rep = s.applyDistill(st, []distillFact{
		{Name: "used-often", Body: "v2 refined", Scope: "project"},
	})
	if rep.Saved != 1 || len(rep.Promoted) != 1 || rep.Promoted[0] != "used-often" {
		t.Fatalf("graduation report = %+v, want used-often promoted", rep)
	}

	// …but re-saving a fact that is already hot is not a promotion:
	// Save carries the hot flag through, so no tier actually changes.
	rep = s.applyDistill(st, []distillFact{
		{Name: "used-often", Body: "v3 refined again", Scope: "project"},
	})
	if rep.Saved != 1 || len(rep.Promoted) != 0 {
		t.Fatalf("already-hot report = %+v, want saved without promotion", rep)
	}
	if got, want := rep.EchoLine(), "memory: +1 facts (used-often)"; got != want {
		t.Fatalf("already-hot echo = %q, want %q", got, want)
	}
}

// TestDistillEchoLineLang: the stdout echo — which never passes through
// cmd/lyon's tr() — picks its language from LYON_LANG the way lang.go's
// initLang does at startup; unset or English stays English.
func TestDistillEchoLineLang(t *testing.T) {
	rep := DistillReport{Saved: 1, Names: []string{"a"}}
	t.Setenv("LYON_LANG", "zh")
	if got := distillEchoLine(rep); got != rep.EchoLineZH() {
		t.Fatalf("zh: %q, want %q", got, rep.EchoLineZH())
	}
	t.Setenv("LYON_LANG", "en")
	if got := distillEchoLine(rep); got != rep.EchoLine() {
		t.Fatalf("en: %q, want %q", got, rep.EchoLine())
	}
	t.Setenv("LYON_LANG", "")
	if got := distillEchoLine(rep); got != rep.EchoLine() {
		t.Fatalf("unset: %q, want %q", got, rep.EchoLine())
	}
}
