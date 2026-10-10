package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/models"
)

// Resident context/cost gauge: where the session stands against the
// model's window, and what it has cost so far. Three data sources —
// turn stats (engine), per-model subagent usage (engine), and the
// pkg/models knowledge base (window + unit price). Pricing is a
// reference number: LYON_PRICE_IN / LYON_PRICE_OUT (USD per million
// tokens) override the knowledge base for gateways whose tariff we
// don't ship; both must be set for the override to take effect.
type usageGauge struct {
	used   int64   // estimated tokens in the live context
	window int64   // context window (knowledge base, config fallback)
	pct    float64 // used/window*100
	cost   float64 // session-cumulative USD (main turns + subagents)
	priced bool    // unit prices on file — cost is a number, not a guess
}

// livePricing resolves the effective unit price for the main model:
// knowledge base first, then environment overrides.
func livePricing(cfg agent.Config) models.Pricing {
	var p models.Pricing
	if info := models.Lookup(cfg.Model); info != nil {
		p = info.Pricing
	}
	if v, err := strconv.ParseFloat(os.Getenv("LYON_PRICE_IN"), 64); err == nil && v > 0 {
		p.Input = v
	}
	if v, err := strconv.ParseFloat(os.Getenv("LYON_PRICE_OUT"), 64); err == nil && v > 0 {
		p.Output = v
	}
	return p
}

// windowFor is the denominator: knowledge window when the model is on
// file, the configured ContextWindow otherwise (never guess).
func windowFor(cfg agent.Config) int64 {
	if info := models.Lookup(cfg.Model); info != nil && info.Context > 0 {
		return info.Context
	}
	return cfg.ContextWindow
}

// contextUsed estimates the live context from the last turn. A normal
// turn's prompt (input + cache read + cache write) is what the request
// carried, and its reply now lives in history — sum them. A compaction
// turn's prompt is the PRE-compact history; only its reply (the summary
// that replaced everything) remains in context.
func contextUsed(turns []agent.TurnStat) int64 {
	if len(turns) == 0 {
		return 0
	}
	last := turns[len(turns)-1]
	if last.StopReason == "compaction" {
		return last.Usage.Output
	}
	u := last.Usage
	return u.Input + u.CacheRead + u.CacheCreation + u.Output
}

// newGauge builds one snapshot. Subagent runs are priced with their own
// model's knowledge entry when known; otherwise they ride the main
// model's price (the common single-model-gateway case — subagent models
// are chosen to be cheaper, so this errs toward undercounting, never
// inflating the bill).
func newGauge(cfg agent.Config, turns []agent.TurnStat, sub map[string]agent.Usage) usageGauge {
	g := usageGauge{window: windowFor(cfg)}
	if g.window > 0 && len(turns) > 0 {
		g.used = contextUsed(turns)
		g.pct = float64(g.used) / float64(g.window) * 100
	}
	p := livePricing(cfg)
	if !p.Known() {
		return g // no price on file: tokens show, cost doesn't guess
	}
	g.priced = true
	for _, t := range turns {
		g.cost += p.CostUSD(t.Usage.Input, t.Usage.Output,
			t.Usage.CacheRead, t.Usage.CacheCreation)
	}
	for model, u := range sub {
		sp := p
		if !strings.EqualFold(model, cfg.Model) {
			if info := models.Lookup(model); info != nil && info.Pricing.Known() {
				sp = info.Pricing
			}
		}
		g.cost += sp.CostUSD(u.Input, u.Output, u.CacheRead, u.CacheCreation)
	}
	return g
}

// fmtCost: three decimals under $10 (prompts often cost cents), two
// above — always with a dollar sign so it reads as money, not tokens.
func fmtCost(usd float64) string {
	if usd < 10 {
		return fmt.Sprintf("$%.3f", usd)
	}
	return fmt.Sprintf("$%.2f", usd)
}

// pctColor: green headroom, yellow approaching the compaction zone,
// red near the window edge.
func pctColor(pct float64) string {
	switch {
	case pct >= 85:
		return cRed
	case pct >= 60:
		return cYellow
	default:
		return cCyan
	}
}

// headerSeg is the right-aligned resident readout for the TUI header:
//
//	ctx 68.2% · 136,400/200,000 · $0.412
//
// Empty until the session has usage — a fresh prompt starts clean.
func (g usageGauge) headerSeg() string {
	if g.used == 0 && g.cost == 0 {
		return ""
	}
	s := paint(pctColor(g.pct), fmt.Sprintf("ctx %.1f%%", g.pct)) +
		paint(cDim, fmt.Sprintf(" · %s/%s",
			groupDigits(g.used), groupDigits(g.window)))
	if g.priced {
		s += paint(cDim, " · ") + paint(cGreen, fmtCost(g.cost))
	}
	return s
}

// detailLine is the spelled-out form for the plain REPL and /stats:
//
//	context 68.2% · 136,400/200,000 tok · cost $0.412（参考价）
func (g usageGauge) detailLine() string {
	if g.used == 0 && g.cost == 0 {
		return ""
	}
	s := paint(pctColor(g.pct), fmt.Sprintf("context %.1f%%", g.pct)) +
		paint(cDim, fmt.Sprintf(" · %s/%s tok",
			groupDigits(g.used), groupDigits(g.window)))
	if g.priced {
		s += paint(cDim, " · cost ") + paint(cGreen, fmtCost(g.cost)) +
			paint(cDim, "（参考价）")
	} else {
		s += paint(cDim, " · cost 未知（LYON_PRICE_IN/OUT 可指定单价）")
	}
	return s
}
