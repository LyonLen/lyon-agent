package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"lyon-agent/pkg/tools"
)

// repeatTestSession is modeTestSession plus the two things a bare
// &Session{} lacks when real tool executions run through it: emit's
// event bus (buffered, undrained) and a live repeat guard.
func repeatTestSession(t *testing.T) *Session {
	t.Helper()
	s := modeTestSession(t)
	s.ctx = context.Background()
	s.events = make(chan Event, 4096)
	s.repeat = newRepeatGuard()
	return s
}

// repeatFake is §1.7's fake tool: canned results consumed in order (the
// last one repeats once the list runs out), a call counter, and a
// configurable ParallelSafe.
type repeatFake struct {
	name     string
	parallel bool
	results  []string
	calls    int
}

func (f *repeatFake) Name() string        { return f.name }
func (f *repeatFake) Description() string { return "repeat fake " + f.name }
func (f *repeatFake) InputSchema() anthropic.BetaToolInputSchemaParam {
	return anthropic.BetaToolInputSchemaParam{}
}
func (f *repeatFake) ParallelSafe() bool { return f.parallel }
func (f *repeatFake) Execute(ctx context.Context, input json.RawMessage) ([]anthropic.BetaToolResultBlockParamContentUnion, error) {
	i := f.calls
	f.calls++
	if i >= len(f.results) {
		i = len(f.results) - 1
	}
	return tools.TextResult(f.results[i]), nil
}

// runRepeat performs one full adapter execution — preTool seam, tool,
// observer feed — and returns exactly the text the model would receive.
func runRepeat(t *testing.T, s *Session, tool tools.Tool, input string) string {
	t.Helper()
	a := tools.Adapt(tool, tools.AdapterConfig{
		Observer: s,
		PreTool:  s.preTool,
	})
	out, err := a.Execute(context.Background(), json.RawMessage(input))
	if err != nil {
		t.Fatalf("execute %s(%s): %v", tool.Name(), input, err)
	}
	var b strings.Builder
	for _, c := range out {
		if c.OfText != nil {
			b.WriteString(c.OfText.Text)
		}
	}
	return b.String()
}

// TestRepeatGuardClosedOnIdenticalResult: same key + identical result
// twice seals the key (the 2nd call really executed); the 3rd
// short-circuits — tool call counter stays at 2, and the model gets the
// cached text with the [unchanged] marker.
func TestRepeatGuardClosedOnIdenticalResult(t *testing.T) {
	s := repeatTestSession(t)
	fake := &repeatFake{name: "read_like", parallel: true, results: []string{"same bytes"}}

	r1 := runRepeat(t, s, fake, `{"path":"a.txt"}`)
	r2 := runRepeat(t, s, fake, `{"path":"a.txt"}`)
	if fake.calls != 2 {
		t.Fatalf("calls after 2 = %d, want 2 (the 2nd must really execute)", fake.calls)
	}
	if r1 != "same bytes" || r2 != "same bytes" {
		t.Fatalf("results = %q / %q, want no marker yet", r1, r2)
	}

	verdict, cached := s.repeat.before("read_like", json.RawMessage(`{"path":"a.txt"}`), true)
	if verdict != repeatShortCircuit {
		t.Fatalf("verdict = %v, want repeatShortCircuit", verdict)
	}
	if cached != "same bytes" {
		t.Fatalf("cached = %q, want the sealed text", cached)
	}

	r3 := runRepeat(t, s, fake, `{"path":"a.txt"}`)
	if fake.calls != 2 {
		t.Fatalf("calls after short-circuit = %d, want 2 (3rd must not execute)", fake.calls)
	}
	if !strings.Contains(r3, "same bytes") || !strings.Contains(r3, "[unchanged") {
		t.Fatalf("short-circuit result missing cache or marker: %q", r3)
	}
}

// TestRepeatGuardReopensWhenResultChanges: a different result on the 2nd
// call is progress — no seal; the 3rd call executes normally.
func TestRepeatGuardReopensWhenResultChanges(t *testing.T) {
	s := repeatTestSession(t)
	fake := &repeatFake{name: "volatile", parallel: true, results: []string{"A", "B", "C"}}

	runRepeat(t, s, fake, `{"q":1}`)
	runRepeat(t, s, fake, `{"q":1}`)
	r3 := runRepeat(t, s, fake, `{"q":1}`)
	if fake.calls != 3 {
		t.Fatalf("calls = %d, want 3 (changed result must not seal)", fake.calls)
	}
	if r3 != "C" {
		t.Fatalf("3rd result = %q, want %q", r3, "C")
	}
	verdict, _ := s.repeat.before("volatile", json.RawMessage(`{"q":1}`), true)
	if verdict != repeatNone {
		t.Fatalf("verdict after changed result = %v, want repeatNone", verdict)
	}
}

// TestRepeatGuardMutatingNeverShortCircuits: ParallelSafe=false with an
// identical result 5 times — every call really executes (5), and from
// the 3rd on the result tail carries the [unchanged] marker.
func TestRepeatGuardMutatingNeverShortCircuits(t *testing.T) {
	s := repeatTestSession(t)
	fake := &repeatFake{name: "write_like", parallel: false, results: []string{"wrote it"}}

	var results []string
	for i := 0; i < 5; i++ {
		results = append(results, runRepeat(t, s, fake, `{"path":"b.txt"}`))
	}
	if fake.calls != 5 {
		t.Fatalf("calls = %d, want 5 (mutating calls never short-circuit)", fake.calls)
	}
	if strings.Contains(results[0], "[unchanged") || strings.Contains(results[1], "[unchanged") {
		t.Fatalf("marker before the seal exists: %q / %q", results[0], results[1])
	}
	for i := 2; i < 5; i++ {
		if !strings.Contains(results[i], "wrote it") || !strings.Contains(results[i], "[unchanged") {
			t.Fatalf("result %d missing sealed marker: %q", i+1, results[i])
		}
	}
}

// TestRepeatGuardResetsPerPrompt: drive() resets the guard at prompt
// entry — a seal from last prompt is unknown again.
func TestRepeatGuardResetsPerPrompt(t *testing.T) {
	s := repeatTestSession(t)
	fake := &repeatFake{name: "greet", parallel: true, results: []string{"hi"}}
	runRepeat(t, s, fake, `{}`)
	runRepeat(t, s, fake, `{}`)

	verdict, _ := s.repeat.before("greet", json.RawMessage(`{}`), true)
	if verdict != repeatShortCircuit {
		t.Fatalf("verdict before reset = %v, want repeatShortCircuit", verdict)
	}

	s.repeat.reset() // what drive does at prompt entry
	verdict, cached := s.repeat.before("greet", json.RawMessage(`{}`), true)
	if verdict != repeatNone {
		t.Fatalf("verdict after reset = %v, want repeatNone", verdict)
	}
	if cached != "" {
		t.Fatalf("cached after reset = %q, want empty", cached)
	}
}

// TestRepeatGuardCanonicalInput: key order, whitespace and 1-vs-1.0
// number formatting collapse to one key; distinct values and distinct
// tools stay distinct (§1.6).
func TestRepeatGuardCanonicalInput(t *testing.T) {
	same := []struct{ a, b string }{
		{`{"a":1,"b":2}`, `{"b": 2, "a": 1}`},
		{`{ "n" : 1 }`, "{\"n\":\n1.0}"},
		{`{"deep":{"x":[1,2,3]}}`, `{"deep": {"x": [1, 2, 3]}}`},
		{`{"n":1.00}`, `{"n":1}`},
	}
	for _, c := range same {
		if canonicalInput(json.RawMessage(c.a)) != canonicalInput(json.RawMessage(c.b)) {
			t.Errorf("canonicalInput diverged for equivalent inputs:\n %s\n %s", c.a, c.b)
		}
	}
	if canonicalInput(json.RawMessage(`{"n":1}`)) == canonicalInput(json.RawMessage(`{"n":2}`)) {
		t.Error("distinct inputs collapsed to one key")
	}
	if repeatKey("tool_a", json.RawMessage(`{}`)) == repeatKey("tool_b", json.RawMessage(`{}`)) {
		t.Error("repeatKey must namespace by tool")
	}
}

// TestRepeatGuardConcurrent: ParallelSafe tools hit before/after from 8
// goroutines on one key — state stays consistent and -race stays clean.
func TestRepeatGuardConcurrent(t *testing.T) {
	g := newRepeatGuard()
	input := json.RawMessage(`{"q":"same"}`)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 4; j++ {
				g.before("tool", input, true)
				g.after("tool", input, "fixed result")
			}
		}()
	}
	wg.Wait()

	g.mu.Lock()
	rec := g.last[repeatKey("tool", input)]
	count, closed := 0, false
	if rec != nil {
		count, closed = rec.count, rec.closed
	}
	g.mu.Unlock()
	if rec == nil || !closed {
		t.Fatalf("record = %+v, want closed", rec)
	}
	if count != 32 {
		t.Fatalf("count = %d, want 32 (8×4 identical results)", count)
	}
}

// TestRepeatGuardLongLegitRunNeverTrips is the workorder's regression:
// a normal long task — 40 read_file calls, each a different path — never
// earns a guard verdict or a marker. Workload length is not a defect
// (the explicit opposite of the MaxIterations=5 trap).
func TestRepeatGuardLongLegitRunNeverTrips(t *testing.T) {
	s := repeatTestSession(t)
	fake := &repeatFake{name: "read_file", parallel: true}
	for i := 0; i < 40; i++ {
		fake.results = append(fake.results, fmt.Sprintf("contents of file %d", i))
	}
	for i := 0; i < 40; i++ {
		input := fmt.Sprintf(`{"path":"dir/f%02d.txt"}`, i)
		out := runRepeat(t, s, fake, input)
		if strings.Contains(out, "[unchanged") {
			t.Fatalf("call %d marked unchanged: %q", i+1, out)
		}
		verdict, _ := s.repeat.before("read_file", json.RawMessage(input), true)
		if verdict != repeatNone {
			t.Fatalf("call %d verdict = %v, want repeatNone", i+1, verdict)
		}
	}
	if fake.calls != 40 {
		t.Fatalf("calls = %d, want 40 (every legitimate read executes)", fake.calls)
	}
}
