package flow

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestGateInjectsGapsIntoEveryNode: a missed coverage gate folds the gap
// lines into the task, and because nodes read {{input}}, every prompt in
// the run carries them (§2.2 定向注入). A passing gate injects nothing;
// a broken gate never blocks the run.
func TestGateInjectsGapsIntoEveryNode(t *testing.T) {
	// Miss with gaps → injected into every node prompt.
	f := &fakeDeps{runs: []string{"ok DONE:"}}
	d := f.deps()
	d.Gate = func(ctx context.Context, task string) (bool, []string, error) {
		return false, []string{"login returns code 42 on expiry", "rollback uses tx.Rollback"}, nil
	}
	w := wf(
		Node{Name: "a", Prompt: "task: {{input}}", Exit: Exit{Kind: ExitContains, Contains: "DONE:"}},
		Node{Name: "b", Prompt: "next: {{input}}", Exit: Exit{Kind: ExitContains, Contains: "DONE:"}},
	)
	res := Execute(context.Background(), w, "fix login", d)
	if !res.OK || !res.Gated || len(res.Injected) != 2 {
		t.Fatalf("result = %+v", res)
	}
	for i, p := range f.prompts {
		if !strings.Contains(p, "code 42 on expiry") || !strings.Contains(p, "fix login") {
			t.Errorf("prompt %d lacks injected gaps:\n%s", i, p)
		}
	}

	// Pass → untouched input, no injection note.
	f2 := &fakeDeps{runs: []string{"ok DONE:"}}
	d2 := f2.deps()
	d2.Gate = func(ctx context.Context, task string) (bool, []string, error) {
		return true, nil, nil
	}
	res2 := Execute(context.Background(), w, "fix login", d2)
	if !res2.OK || len(res2.Injected) != 0 || res2.GateNote != "" {
		t.Fatalf("passing gate result = %+v", res2)
	}
	if strings.Contains(f2.prompts[0], "Knowledge gaps") {
		t.Fatalf("passing gate must not inject:\n%s", f2.prompts[0])
	}

	// Broken gate → run proceeds, note records the skip.
	f3 := &fakeDeps{runs: []string{"ok DONE:"}}
	d3 := f3.deps()
	d3.Gate = func(ctx context.Context, task string) (bool, []string, error) {
		return false, nil, errors.New("quiz model unreachable")
	}
	res3 := Execute(context.Background(), w, "fix login", d3)
	if !res3.OK || res3.Gated || !strings.Contains(res3.GateNote, "quiz model unreachable") {
		t.Fatalf("broken gate result = %+v", res3)
	}
}

// TestNewGateAdaptsRunCoverage: the glue scores through eval's threshold
// and passes inject lines through untouched.
func TestNewGateAdaptsRunCoverage(t *testing.T) {
	gen := func(ctx context.Context, task string) (string, error) {
		return `[{"q":"What does X return on expiry?","expected":"42"}]`, nil
	}
	// Blind answer misses the only item → 0.0 < 0.6 → gate closes.
	bookMiss := func(ctx context.Context, questions []string) (string, error) {
		return `{"What does X return on expiry?":"unknown"}`, nil
	}
	ok, inject, err := NewGate(gen, bookMiss)(context.Background(), "task")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("zero coverage must not pass the 0.6 gate")
	}
	if len(inject) == 0 {
		t.Fatalf("missed item must surface as an inject line: %v", inject)
	}

	// Blind answer hits → gate opens, no injection.
	bookHit := func(ctx context.Context, questions []string) (string, error) {
		return `{"What does X return on expiry?":"42"}`, nil
	}
	ok, inject, err = NewGate(gen, bookHit)(context.Background(), "task")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || len(inject) != 0 {
		t.Fatalf("hit result: ok=%v inject=%v", ok, inject)
	}

	// Propagated generation error marks the gate broken, not fatal.
	_, _, err = NewGate(func(ctx context.Context, task string) (string, error) {
		return "", errors.New("gen down")
	}, bookHit)(context.Background(), "task")
	if err == nil {
		t.Fatal("generation error must surface from the gate")
	}
}

// TestNewDiagnoseFeedsConstraints: the runner's retry hook returns eval's
// constraint block; an asker failure propagates (Execute treats it as
// "no fresh constraints", never a crash).
func TestNewDiagnoseFeedsConstraints(t *testing.T) {
	ask := func(ctx context.Context, system, user string) (string, error) {
		if !strings.Contains(user, "Attempt output:") || !strings.Contains(user, "exit 1") {
			t.Errorf("trajectory lacks the attempt:\n%s", user)
		}
		if !strings.Contains(system, "failed_conditions") {
			t.Errorf("system is not the diagnoser prompt:\n%s", system)
		}
		return `{"failed_conditions":["marker never emitted"],"patch":"emit DONE: explicitly"}`, nil
	}
	got, err := NewDiagnose(ask)(context.Background(), "do the thing", "half done", "exit 1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "marker never emitted") ||
		!strings.Contains(got, "emit DONE: explicitly") {
		t.Fatalf("constraints = %q", got)
	}

	// Error path bubbles up.
	_, err = NewDiagnose(func(ctx context.Context, system, user string) (string, error) {
		return "", errors.New("rate limited")
	})(context.Background(), "p", "o", "f")
	if err == nil {
		t.Fatal("asker error must propagate")
	}
}

// TestFlowEndToEndWithAllHooks: gate + run + diagnose + judge in one
// offline pass — the machine exactly as production wires it (minus the
// network legs, which are injected).
func TestFlowEndToEndWithAllHooks(t *testing.T) {
	f := &fakeDeps{
		runs: []string{"first try", "all green DONE: ok", "VERDICT: lgtm"},
		diag: "emit the marker",
	}
	d := f.deps()
	d.Gate = func(ctx context.Context, task string) (bool, []string, error) {
		return false, []string{"gate gap"}, nil
	}
	w := wf(
		Node{Name: "draft", Prompt: "{{input}}", Exit: Exit{Kind: ExitContains, Contains: "DONE:"}},
		Node{Name: "verdict", Prompt: "{{input}}", Exit: Exit{Kind: ExitContains, Contains: "VERDICT:"}},
	)
	res := Execute(context.Background(), w, "review this", d)
	if !res.OK || !res.Gated || res.Attempts != 3 {
		t.Fatalf("result = %+v", res)
	}
	// Node one missed once and retried with the diagnosis attached.
	if !strings.Contains(f.prompts[1], "Constraints from previous failed attempts") {
		t.Fatalf("retry prompt lacks constraints:\n%s", f.prompts[1])
	}
	if f.diagN != 1 {
		t.Fatalf("diagnose calls = %d", f.diagN)
	}
}
