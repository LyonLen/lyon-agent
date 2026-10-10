package main

import (
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	tea "github.com/charmbracelet/bubbletea"

	"lyon-agent/pkg/agent"
)

// TestContextUsed: the live-context estimate — normal turns sum prompt
// + reply; a compaction turn's prompt is the discarded pre-compact
// history, so only its reply (the surviving summary) counts.
func TestContextUsed(t *testing.T) {
	if got := contextUsed(nil); got != 0 {
		t.Fatalf("empty turns = %d, want 0", got)
	}
	normal := agent.TurnStat{StopReason: "end_turn", Usage: agent.Usage{
		Input: 1000, Output: 500, CacheRead: 2000, CacheCreation: 100,
	}}
	if got := contextUsed([]agent.TurnStat{normal}); got != 3600 {
		t.Fatalf("normal turn context = %d, want 3600", got)
	}
	compaction := agent.TurnStat{StopReason: "compaction", Usage: agent.Usage{
		Input: 150000, Output: 800, CacheRead: 0, CacheCreation: 0,
	}}
	if got := contextUsed([]agent.TurnStat{normal, compaction}); got != 800 {
		t.Fatalf("post-compaction context = %d, want only the summary (800)", got)
	}
}

// TestGaugeKnowledgePricing: knowledge base supplies window + unit
// price; subagents on a known cheaper model bill at their own rate.
func TestGaugeKnowledgePricing(t *testing.T) {
	cfg := agent.Config{Model: "deepseek-chat", ContextWindow: 999} // KB window wins
	turns := []agent.TurnStat{{Usage: agent.Usage{
		Input: 1000, Output: 500, CacheRead: 2000, CacheCreation: 100,
	}}}
	g := newGauge(cfg, turns, nil)
	if g.window != 65536 {
		t.Fatalf("window = %d, want knowledge base 65536", g.window)
	}
	if g.used != 3600 || g.pct <= 0 {
		t.Fatalf("used = %d pct = %f", g.used, g.pct)
	}
	if !g.priced {
		t.Fatal("deepseek-chat has prices — gauge must be priced")
	}
	// (1000×0.27 + 2000×0.27×0.1 + 100×0.27×1.25 + 500×1.1) / 1e6
	want := (1000*0.27 + 2000*0.27*0.1 + 100*0.27*1.25 + 500*1.1) / 1e6
	if diff := g.cost - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("cost = %.9f, want %.9f", g.cost, want)
	}

	// Subagent on its own known price: haiku $1/M input.
	sub := map[string]agent.Usage{"claude-haiku-4-5-20251001": {Input: 100000, Output: 0}}
	g2 := newGauge(cfg, turns, sub)
	if diff := (g2.cost - g.cost) - 0.1; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("subagent cost delta = %f, want $0.10 (100k×$1/M)", g2.cost-g.cost)
	}

	// Same-model subagent rides the main (env-overridden) price — the
	// single-model-gateway case where a second price would be wrong.
	t.Setenv("LYON_PRICE_IN", "2")
	t.Setenv("LYON_PRICE_OUT", "10")
	base := newGauge(cfg, turns, nil) // main turns repriced by the env now
	g3 := newGauge(cfg, turns, map[string]agent.Usage{"deepseek-chat": {Input: 1000000}})
	// 1M input tokens × $2/Mtok = $2 exactly.
	if diff := (g3.cost - base.cost) - 2; diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("env-priced subagent delta = %f, want $2", g3.cost-base.cost)
	}
}

// TestGaugeUnknownAndOverride: an unregistered model (the live gateway
// case) shows tokens but never invents a price — unless the operator
// declares one via LYON_PRICE_IN/OUT (both required).
func TestGaugeUnknownAndOverride(t *testing.T) {
	cfg := agent.Config{Model: "mimo-v2.6-flash", ContextWindow: 200000}
	turns := []agent.TurnStat{{Usage: agent.Usage{Input: 5000, Output: 1000}}}
	g := newGauge(cfg, turns, nil)
	if g.window != 200000 || g.priced || g.cost != 0 {
		t.Fatalf("unknown model gauge: %+v", g)
	}
	if line := g.detailLine(); !strings.Contains(line, "未知") ||
		!strings.Contains(line, "context") {
		t.Fatalf("unpriced detail line: %q", line)
	}

	t.Setenv("LYON_PRICE_IN", "0.5")
	if g := newGauge(cfg, turns, nil); g.priced {
		t.Fatal("one-sided price override must not enable pricing")
	}
	t.Setenv("LYON_PRICE_OUT", "2")
	g = newGauge(cfg, turns, nil)
	if !g.priced {
		t.Fatal("both prices set — gauge must be priced")
	}
	// 5000×0.5 + 1000×2 = 4500 per Mtok → $0.0045
	if diff := g.cost - 0.0045; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("env cost = %.9f, want 0.0045", g.cost)
	}
}

// TestGaugeDisplay: threshold colors, empty-fresh suppression, cost
// formatting, and the right-aligned header segment.
func TestGaugeDisplay(t *testing.T) {
	if seg := (usageGauge{}).headerSeg(); seg != "" {
		t.Fatalf("fresh session must show nothing, got %q", seg)
	}
	for _, c := range []struct {
		pct  float64
		want string
	}{
		{12, "\x1b[36m"}, // cyan
		{65, "\x1b[33m"}, // yellow: compaction zone ahead
		{92, "\x1b[31m"}, // red: window edge
	} {
		if got := pctColor(c.pct); got != c.want {
			t.Errorf("pctColor(%v) = %q, want %q", c.pct, got, c.want)
		}
	}
	// Values chosen to sit clear of binary rounding edges at the two
	// precisions (%.3f under $10, %.2f above).
	if s := fmtCost(0.0046); s != "$0.005" {
		t.Errorf("fmtCost small = %q", s)
	}
	if s := fmtCost(12.346); s != "$12.35" {
		t.Errorf("fmtCost large = %q", s)
	}
	g := usageGauge{used: 136400, window: 200000, pct: 68.2, cost: 0.412, priced: true}
	seg := g.headerSeg()
	if !strings.Contains(seg, "ctx 68.2%") ||
		!strings.Contains(seg, "136,400/200,000") ||
		!strings.Contains(seg, "$0.412") {
		t.Fatalf("header seg: %q", seg)
	}
}

// TestHeaderLineGauge: the resident readout pins right on a wide
// terminal and steps aside (title intact, no half-clipped figure) on a
// narrow one — frame invariants hold either way.
func TestHeaderLineGauge(t *testing.T) {
	m := newTestModel(t)
	m.sess.Restore(agent.Snapshot{
		Messages: []anthropic.BetaMessageParam{
			anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("hi")),
		},
		Turns: []agent.TurnStat{{Turn: 1, StopReason: "end_turn",
			Usage: agent.Usage{Input: 10000, Output: 5000}}},
	})

	m.Update(tea.WindowSizeMsg{Width: 220, Height: 30})
	head := m.headerLine()
	if !strings.Contains(head, "ctx ") || !strings.Contains(head, "model=") {
		t.Fatalf("wide header missing gauge or identity: %q", stripColor(head))
	}
	checkView(t, m, "wide with gauge")

	// Narrow: gauge drops, title side survives, frame still exact.
	m.Update(tea.WindowSizeMsg{Width: 70, Height: 30})
	head = m.headerLine()
	if strings.Contains(head, "ctx ") {
		t.Fatalf("gauge should drop on narrow terminal: %q", stripColor(head))
	}
	checkView(t, m, "narrow without gauge")
}
