package flow

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// settle retries TempDir cleanup — Windows file scanners can hold a
// just-written file past the framework's immediate RemoveAll.
func settle(t *testing.T, dir string) {
	t.Helper()
	t.Cleanup(func() {
		var err error
		for i := 0; i < 40; i++ {
			if err = os.RemoveAll(dir); err == nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Logf("settle cleanup failed: %v", err)
	})
}

// fakeDeps builds runnable deps over scripted outputs.
type fakeDeps struct {
	runs    []string // outputs per Run call (last repeats on overflow)
	runErr  error
	shellOK bool
	judgeOK bool
	diag    string
	diagErr error

	runN      int
	diagN     int
	prompts   []string
	probes    []string
	judged    []string
	stepCalls []string
}

func (f *fakeDeps) deps() Deps {
	return Deps{
		Run: func(ctx context.Context, prompt string) (string, error) {
			f.prompts = append(f.prompts, prompt)
			out := "default output"
			if len(f.runs) > 0 {
				i := f.runN
				if i >= len(f.runs) {
					i = len(f.runs) - 1
				}
				out = f.runs[i]
			}
			f.runN++
			return out, f.runErr
		},
		Shell: func(ctx context.Context, command string) (string, int, error) {
			f.probes = append(f.probes, command)
			if f.shellOK {
				return "ok", 0, nil
			}
			return "boom", 1, nil
		},
		Judge: func(ctx context.Context, question, output string) (bool, error) {
			f.judged = append(f.judged, question+"|"+output)
			return f.judgeOK, nil
		},
		Diagnose: func(ctx context.Context, prompt, output, fail string) (string, error) {
			f.diagN++
			return f.diag, f.diagErr
		},
		Step: func(node string, attempt, max int, ok bool, note string) {
			f.stepCalls = append(f.stepCalls,
				node+"#"+itoa(attempt)+map[bool]string{true: " pass", false: " fail"}[ok])
		},
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func wf(nodes ...Node) *Workflow {
	return &Workflow{Slug: "t", Title: "t", Nodes: nodes}
}

// TestExecuteHappyPath: a clean run walks every node in order, records
// outputs and reports attempts truthfully.
func TestExecuteHappyPath(t *testing.T) {
	f := &fakeDeps{runs: []string{"hello DONE:"}, shellOK: true, judgeOK: true}
	w := wf(
		Node{Name: "a", Prompt: "step a {{input}}", Exit: Exit{Kind: ExitAlways}},
		Node{Name: "b", Prompt: "step b", Exit: Exit{Kind: ExitContains, Contains: "DONE:"}},
		Node{Name: "c", Prompt: "step c", Exit: Exit{Kind: ExitCommand, Command: "go test ./..."}},
		Node{Name: "d", Prompt: "step d", Exit: Exit{Kind: ExitJudge, Judge: "good?"}},
	)
	res := Execute(context.Background(), w, "the task", f.deps())
	if !res.OK || res.FailedNode != "" || len(res.Failed) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if res.Attempts != 4 {
		t.Fatalf("attempts = %d, want 4", res.Attempts)
	}
	if !strings.Contains(f.prompts[0], "the task") {
		t.Fatalf("{{input}} not substituted: %q", f.prompts[0])
	}
	if len(f.probes) != 1 || f.probes[0] != "go test ./..." {
		t.Fatalf("probes = %v", f.probes)
	}
	if len(f.judged) != 1 {
		t.Fatalf("judged = %v", f.judged)
	}
	if got := strings.Join(f.stepCalls, ","); !strings.Contains(got, "a#1 pass") ||
		!strings.Contains(got, "d#1 pass") {
		t.Fatalf("step calls = %v", f.stepCalls)
	}
}

// TestRetryFeedsDiagnosisBack: a flaky node fails twice, the post-mortem
// arrives as constraints on the next prompt, then passes — bounded §1.3
// retry inside the mechanical runner.
func TestRetryFeedsDiagnosisBack(t *testing.T) {
	f := &fakeDeps{
		runs: []string{"missing marker", "still missing", "now HAS-MARKER"},
		diag: "always emit the exact marker",
	}
	w := wf(Node{Name: "n", Prompt: "p", Exit: Exit{Kind: ExitContains, Contains: "HAS-MARKER"}})
	res := Execute(context.Background(), w, "", f.deps())
	if !res.OK || res.Attempts != 3 {
		t.Fatalf("result = %+v", res)
	}
	if f.diagN != 2 {
		t.Fatalf("diagnose calls = %d, want 2 (before retries 2 and 3)", f.diagN)
	}
	if !strings.Contains(f.prompts[2], "Constraints from previous failed attempts") ||
		!strings.Contains(f.prompts[2], "always emit the exact marker") {
		t.Fatalf("third prompt lacks constraints:\n%s", f.prompts[2])
	}
}

// TestRetryExhaustedAborts: three misses and the flow stops at the node
// — never an unbounded loop, never a silent skip.
func TestRetryExhaustedAborts(t *testing.T) {
	f := &fakeDeps{runs: []string{"nope"}, diag: "patch"}
	w := wf(
		Node{Name: "bad", Prompt: "p", Exit: Exit{Kind: ExitContains, Contains: "NEVER"}},
		Node{Name: "after", Prompt: "p", Exit: Exit{Kind: ExitAlways}},
	)
	res := Execute(context.Background(), w, "", f.deps())
	if res.OK || res.FailedNode != "bad" || res.Attempts != MaxAttempts {
		t.Fatalf("result = %+v", res)
	}
	if len(f.prompts) != MaxAttempts {
		t.Fatalf("runs = %d, want %d", len(f.prompts), MaxAttempts)
	}
	if len(res.Failed) != 1 || res.Failed[0] != "bad" {
		t.Fatalf("failed = %v", res.Failed)
	}
}

// TestContinueAndGoto: fallback continue records the miss and walks on;
// goto reroutes control flow — and still counts the budget.
func TestContinueAndGoto(t *testing.T) {
	f := &fakeDeps{runs: []string{"no-marker"}}
	w := wf(
		Node{Name: "skipme", Prompt: "p", Exit: Exit{Kind: ExitContains, Contains: "X"}, Fallback: FallbackContinue},
		Node{Name: "end", Prompt: "p", Exit: Exit{Kind: ExitAlways}},
	)
	res := Execute(context.Background(), w, "", f.deps())
	if res.OK || len(res.Failed) != 1 || res.Failed[0] != "skipme" {
		t.Fatalf("continue result = %+v", res)
	}
	if res.FailedNode != "" {
		t.Fatalf("continue must not abort: %+v", res)
	}

	// goto lands on the named node after a failure — the miss is still
	// recorded, but the flow continues past the detour.
	f2 := &fakeDeps{runs: []string{"no-marker"}}
	w2 := wf(
		Node{Name: "first", Prompt: "p", Exit: Exit{Kind: ExitContains, Contains: "X"}, Fallback: GotoPrefix + "second"},
		Node{Name: "detour", Prompt: "p", Exit: Exit{Kind: ExitAlways}},
		Node{Name: "second", Prompt: "p", Exit: Exit{Kind: ExitAlways}},
	)
	res2 := Execute(context.Background(), w2, "", f2.deps())
	if res2.FailedNode != "" || len(res2.Failed) != 1 || res2.Failed[0] != "first" {
		t.Fatalf("goto result = %+v", res2)
	}
	got := strings.Join(f2.stepCalls, ",")
	if !strings.Contains(got, "first") || !strings.Contains(got, "second") {
		t.Fatalf("goto steps = %v", f2.stepCalls)
	}
	if strings.Contains(got, "detour") {
		t.Fatalf("detour must be skipped by goto: %v", f2.stepCalls)
	}
}

// TestGotoCycleHitsBudget: a jump loop dies on the tick budget with an
// error, not by hanging the CLI.
func TestGotoCycleHitsBudget(t *testing.T) {
	f := &fakeDeps{}
	w := wf(
		Node{Name: "a", Prompt: "p", Exit: Exit{Kind: ExitAlways}, OnPass: "b"},
		Node{Name: "b", Prompt: "p", Exit: Exit{Kind: ExitAlways}, OnPass: "a"},
	)
	res := Execute(context.Background(), w, "", f.deps())
	if res.OK || !strings.Contains(res.Err, "tick budget") {
		t.Fatalf("result = %+v", res)
	}
}

// TestRunErrorAndContext: transport failures are verdicts (retried), a
// cancelled context stops the flow immediately.
func TestRunErrorAndContext(t *testing.T) {
	f := &fakeDeps{runErr: errors.New("gateway 500")}
	w := wf(Node{Name: "n", Prompt: "p", Exit: Exit{Kind: ExitAlways}})
	res := Execute(context.Background(), w, "", f.deps())
	if res.OK || res.Attempts != MaxAttempts {
		t.Fatalf("run-error result = %+v", res)
	}
	if !strings.Contains(res.Failed[0], "n") {
		t.Fatalf("failed = %v", res.Failed)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res2 := Execute(ctx, w, "", (&fakeDeps{}).deps())
	if res2.Err == "" {
		t.Fatalf("cancelled context must set Err: %+v", res2)
	}
}

// TestMatchScoring: coverage wins, ≥1 hit required, ties keep order.
func TestMatchScoring(t *testing.T) {
	wfs := []Workflow{
		{Slug: "bugfix", Trigger: []string{"fix", "报错"}},
		{Slug: "feature-dev", Trigger: []string{"feature", "功能", "需求"}},
		{Slug: "release", Trigger: []string{"release", "版本"}},
	}
	got, ok := Match(wfs, "fix the crash 报错 in login")
	if !ok || got.Slug != "bugfix" {
		t.Fatalf("match = %v ok=%v", got, ok)
	}
	got, ok = Match(wfs, "实现一个 feature 关于需求的功能")
	if !ok || got.Slug != "feature-dev" {
		t.Fatalf("match = %v ok=%v", got, ok)
	}
	if _, ok = Match(wfs, "hello there"); ok {
		t.Fatal("no keyword must miss")
	}
	// Ties: equal coverage → the earlier workflow wins.
	wfs2 := []Workflow{{Slug: "one", Trigger: []string{"x"}}, {Slug: "two", Trigger: []string{"x"}}}
	if got, _ = Match(wfs2, "x y z"); got.Slug != "one" {
		t.Fatalf("tie winner = %s", got.Slug)
	}
}

// TestValidateRejectsBrokenWorkflows: structural mistakes fail at load,
// not mid-run.
func TestValidateRejectsBrokenWorkflows(t *testing.T) {
	cases := []Workflow{
		{Slug: "s"}, // no nodes
		{Slug: "s", Nodes: []Node{{Name: "a"}, {Name: "a"}}},                   // dup names
		{Slug: "s", Nodes: []Node{{Name: "a", Exit: Exit{Kind: "vibes"}}}},     // bad kind
		{Slug: "s", Nodes: []Node{{Name: "a", Exit: Exit{Kind: ExitCommand}}}}, // no command
		{Slug: "s", Nodes: []Node{{Name: "a", OnPass: "ghost"}}},               // dangling on_pass
		{Slug: "s", Nodes: []Node{{Name: "a", Fallback: "goto:ghost"}}},        // dangling goto
	}
	for i, c := range cases {
		if err := validate(c); err == nil {
			t.Errorf("case %d: want error, got nil", i)
		}
	}
	ok := Workflow{Slug: "s", Nodes: []Node{{Name: "a", Exit: Exit{Kind: ExitAlways}}}}
	if err := validate(ok); err != nil {
		t.Errorf("valid workflow rejected: %v", err)
	}
}
