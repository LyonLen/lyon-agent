package main

import (
	"fmt"
	"strings"

	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/models"
)

// /model — the built-in model knowledge card.
//
//	/model        current model: context window, official params, pricing
//	/model list   every model in the knowledge base
//	/model <id>   look up any id without switching to it
//
// Unknown ids fall back to the configured values instead of guessing —
// the knowledge base never invents a context window. Core rendering is
// line-based so the TUI pushes it into the transcript and the plain REPL
// prints it verbatim.

// showModel dispatches /model args in the TUI.
func (m *tuiModel) showModel(args string) {
	m.flushPend()
	m.pushCmd(modelLines(m.cfg, strings.TrimSpace(args))...)
	m.refresh()
}

// modelLines renders /model for any caller: "list" → the registry table,
// "" → the live model, anything else → a card for that id.
func modelLines(cfg agent.Config, args string) []string {
	switch {
	case args == "list":
		return modelListLines()
	case args == "":
		return modelCardLines(cfg, cfg.Model)
	default:
		return modelCardLines(cfg, args)
	}
}

// modelCardLines: one knowledge card plus the effective runtime config
// when the card is for the model actually in use.
func modelCardLines(cfg agent.Config, id string) []string {
	out := []string{paint(cBold, "model card") + " " + id}
	info := models.Lookup(id)
	if info == nil {
		out = append(out, paint(cYellow, "  知识库未收录该型号 — 按配置执行，不猜参数"))
		return append(out, effectiveLine(cfg))
	}
	out = append(out, fmt.Sprintf("  %s · %s%s",
		info.Family, info.Provider, cutoffSuffix(info.Cutoff)))
	out = append(out, fmt.Sprintf("  %s %s tok · %s %s tok",
		paint(cCyan, "context"), groupDigits(info.Context),
		paint(cCyan, "max output"), groupDigits(info.MaxOutput)))
	out = append(out, fmt.Sprintf("  %s %s · %s %s",
		paint(cMagenta, "temp"), info.Temperature,
		paint(cMagenta, "top_p"), info.TopP))
	if info.Reasoning {
		out = append(out, "  "+paint(cYellow, "reasoning model — 推理强度用 effort 控制，采样参数固定"))
	}
	out = append(out, fmt.Sprintf("  %s %s", paint(cGreen, "price"), info.FormatPrice()))
	for _, n := range info.Notes {
		out = append(out, paint(cDim, "  · "+n))
	}
	// The card is for the live model: show what this run actually uses.
	if live := models.Lookup(cfg.Model); live == info || strings.EqualFold(id, cfg.Model) {
		out = append(out, effectiveLine(cfg))
	}
	return out
}

// effectiveLine: the config side of the equation — knowledge says what
// the model supports, this says what the session runs with.
func effectiveLine(cfg agent.Config) string {
	return fmt.Sprintf("  %s model=%s effort=%s max_tokens=%d ctx(配置)=%s compact@%.0f%%",
		paint(cDim, "effective:"),
		cfg.Model, cfg.Effort, cfg.MaxTokens, groupDigits(cfg.ContextWindow),
		cfg.CompactThreshold*100)
}

// modelListLines: the whole knowledge base in one table.
func modelListLines() []string {
	all := models.All()
	out := []string{
		paint(cBold, fmt.Sprintf("known models (%d)", len(all))),
		paint(cDim, "  id · context · price(参考) — /model <id> for details"),
	}
	for i := range all {
		e := &all[i]
		id := e.ID
		if len(id) > 24 {
			id = id[:23] + "…"
		}
		price := "—"
		if e.Pricing.Input > 0 || e.Pricing.Output > 0 {
			price = fmt.Sprintf("$%.2f/$%.2f", e.Pricing.Input, e.Pricing.Output)
		}
		out = append(out, fmt.Sprintf("  %-24s %10s  %-14s %s",
			id, groupDigits(e.Context), price,
			paint(cDim, e.Family)))
	}
	return out
}

func cutoffSuffix(c string) string {
	if c == "" {
		return ""
	}
	return paint(cDim, " · cutoff "+c)
}

// groupDigits formats a token count with thousands separators.
func groupDigits(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
	}
	for i := pre; i < len(s); i += 3 {
		if i > 0 || pre > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}
