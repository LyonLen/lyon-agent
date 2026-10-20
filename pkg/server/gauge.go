package server

import (
	"os"
	"strconv"
	"strings"

	lyonv1 "lyon-agent/gen/go/lyon/v1"
	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/models"
)

// Server-side mirror of the TUI's resident gauge (cmd/lyon/usage.go).
// The web footer reads these numbers instead of re-deriving usage from
// raw turn events, so both surfaces — and a session's FIRST paint, before
// any turn_end streams — agree by construction.

// livePricing resolves the effective unit price for the main model:
// knowledge base first, then environment overrides (LYON_PRICE_IN/OUT
// in USD per million tokens, both must be set to take effect).
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

// contextUsed estimates live context from the last turn (usage.go twin):
// a normal turn's prompt (input + cache read + cache write) plus its
// reply, which now lives in history; a compaction turn's prompt was the
// PRE-compact history — only its reply (the summary) remains.
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

// sessionCostUSD walks cumulative spend — main turns at live pricing,
// subagent runs at their own model's knowledge price when known (else
// the main model's price: subagent models are picked to be cheaper, so
// this errs toward undercounting). priced=false means no price on file:
// show tokens, never guess dollars.
func sessionCostUSD(cfg agent.Config, turns []agent.TurnStat, sub map[string]agent.Usage) (float64, bool) {
	p := livePricing(cfg)
	if !p.Known() {
		return 0, false
	}
	var usd float64
	for _, t := range turns {
		usd += p.CostUSD(t.Usage.Input, t.Usage.Output, t.Usage.CacheRead, t.Usage.CacheCreation)
	}
	for model, u := range sub {
		sp := p
		if !strings.EqualFold(model, cfg.Model) {
			if info := models.Lookup(model); info != nil && info.Pricing.Known() {
				sp = info.Pricing
			}
		}
		usd += sp.CostUSD(u.Input, u.Output, u.CacheRead, u.CacheCreation)
	}
	return usd, true
}

// decorateGauge stamps the server-computed snapshot onto turn-boundary
// events — the frames the web footer reads. Stats and SubUsage are copies
// under the session lock, so a concurrent turn can only make the stamped
// value newer, never torn.
func decorateGauge(se *lyonv1.SessionEvent, s *Server, sess *agent.Session) {
	if se.TypeName != string(agent.EventTurnEnd) {
		return
	}
	turns := sess.Stats()
	se.CtxUsed = contextUsed(turns)
	if usd, ok := sessionCostUSD(s.eng.Config(), turns, sess.SubUsage()); ok {
		se.CostUsd = &usd
	}
}

// sessionGauge fills the Session wire snapshot (open-time display).
func sessionGauge(s *Server, sess *agent.Session) (int64, *float64) {
	turns := sess.Stats()
	used := contextUsed(turns)
	if usd, ok := sessionCostUSD(s.eng.Config(), turns, sess.SubUsage()); ok {
		return used, &usd
	}
	return used, nil
}
