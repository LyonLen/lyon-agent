package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"lyon-agent/internal/mockapi"
	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/market"
	"lyon-agent/pkg/routing"
	"lyon-agent/pkg/tools"
)

const marketProbe = "fix the failing unit test in the parser package"

// TestSendPromptShowsMarketEstimate: a new task arriving gets its price —
// the ⏱ line names the class and the mean of recent successful peers.
// No history, no line.
func TestSendPromptShowsMarketEstimate(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))
	t.Setenv("LYON_SCORE", "off")

	class := string(routing.Classify(marketProbe))
	if class == "" {
		t.Fatal("probe text classifies to nothing")
	}
	for i := 0; i < 3; i++ {
		market.Open().Record(market.Record{Class: class, Prompt: "earlier peer", DurMS: 10_000})
	}

	mock := mockapi.New(mockapi.Config{Script: []mockapi.Turn{{Text: []string{"ok"}}}})
	t.Cleanup(mock.Close)
	t.Setenv("ANTHROPIC_BASE_URL", mock.URL())
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "market-tui-test-token")

	cfg := agent.DefaultConfig()
	eng := agent.NewEngine(cfg, nil)
	sess := eng.NewSession(nil)
	t.Cleanup(sess.Close)
	go func() {
		for range sess.Events() {
		}
	}()
	m := newTUIModel(cfg, sess, tools.NewMetrics())
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	m.sendPrompt(marketProbe)

	var estLine string
	for _, l := range m.lines {
		if strings.Contains(l.raw, "⏱") {
			estLine = l.raw
			break
		}
	}
	if estLine == "" {
		t.Fatal("no ⏱ estimate line after submit despite priced history")
	}
	for _, want := range []string{"10.0s", class} {
		if !strings.Contains(estLine, want) {
			t.Fatalf("estimate line %q missing %q", estLine, want)
		}
	}

	// A cold class on a cold ledger prints nothing: wipe and resubmit.
	t.Setenv("LYON_HOME", tempDir(t))
	m2 := newTUIModel(cfg, sess, tools.NewMetrics())
	m2.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m2.sendPrompt(marketProbe)
	for _, l := range m2.lines {
		if strings.Contains(l.raw, "⏱") {
			t.Fatalf("cold ledger must not print an estimate: %q", l.raw)
		}
	}
}

// TestPromptEndFilesMarketRecord: the done card's wall time lands on the
// ledger with the run's class, model and iterations — failed runs file
// too, stamped with their error so they never price an estimate.
func TestPromptEndFilesMarketRecord(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))
	m := newTestModel(t)

	m.hist = append(m.hist, marketProbe)
	m.runModel = "route-x"
	m.applyEvent(agent.Event{Type: agent.EventPromptEnd, DurationMS: 42_000, Iterations: 3})
	r := market.Open().Recent(1)
	if len(r) != 1 {
		t.Fatalf("ledger after a clean run = %d rows, want 1", len(r))
	}
	got := r[0]
	if got.Class != string(routing.Classify(marketProbe)) || got.Model != "route-x" ||
		got.DurMS != 42_000 || got.Iters != 3 || got.Err != "" {
		t.Fatalf("filed record = %+v", got)
	}

	m.hist = append(m.hist, marketProbe)
	m.applyEvent(agent.Event{Type: agent.EventPromptEnd, DurationMS: 9_000, Err: "boom"})
	r = market.Open().Recent(1)
	if len(r) != 1 || r[0].Err != "boom" || r[0].DurMS != 9_000 {
		t.Fatalf("failed run filed wrong: %+v", r)
	}
	// The 9s failure must not move the class price: the clean 42s row is
	// still the only successful sample.
	est, ok := market.Open().Estimate(marketProbe)
	if !ok || est.DurMS != 42_000 || est.Samples != 1 {
		t.Fatalf("estimate after failure = %+v ok=%v, want the single clean row", est, ok)
	}

	// A prompt_end without wall time files nothing (interrupt bookkeeping).
	m.hist = append(m.hist, marketProbe)
	m.applyEvent(agent.Event{Type: agent.EventPromptEnd})
	if n := len(market.Open().Recent(5)); n != 2 {
		t.Fatalf("zero-duration end filed a row: %d rows", n)
	}
}

// TestMarketCommandListsLedger: /market shows the per-class rollup and
// the newest tasks; an empty ledger says so instead of erroring.
func TestMarketCommandListsLedger(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))
	m := newTestModel(t)

	m.cmdOut = nil
	m.marketCommand("")
	if got := m.cmdOutText(); !strings.Contains(got, "task hour market") ||
		!strings.Contains(got, "no finished tasks yet") {
		t.Fatalf("empty ledger output:\n%s", got)
	}

	market.Open().Record(market.Record{Class: "coding", Prompt: "fix the parser bug", DurMS: 30_000})
	market.Open().Record(market.Record{Class: "coding", Prompt: "patch the flaky test", DurMS: 50_000})
	market.Open().Record(market.Record{Class: "coding", Prompt: "flaky run", DurMS: 1_000, Err: "interrupted"})

	m.cmdOut = nil
	m.marketCommand("")
	got := m.cmdOutText()
	for _, want := range []string{"coding", "40.0s", "50.0s", "30.0s", "fix the parser bug", "✗"} {
		if !strings.Contains(got, want) {
			t.Fatalf("/market output missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "flaky run") || !strings.Contains(got, "✗") {
		t.Fatalf("failed task must show in the ledger, marked ✗:\n%s", got)
	}

	m.cmdOut = nil
	m.marketCommand("clear the ledger")
	if got := m.cmdOutText(); !strings.Contains(got, "usage: /market") {
		t.Fatalf("unknown args must show usage:\n%s", got)
	}
}
