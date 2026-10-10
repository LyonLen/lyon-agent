package models

import (
	"strings"
	"testing"
)

// TestLookupExact: bare ids resolve to themselves before any wildcard.
func TestLookupExact(t *testing.T) {
	got := Lookup("deepseek-chat")
	if got == nil || got.Family != "DeepSeek V3" {
		t.Fatalf("exact lookup: %+v", got)
	}
	// Case-insensitive exact.
	if got := Lookup("DEEPSEEK-CHAT"); got == nil || got.Family != "DeepSeek V3" {
		t.Fatalf("case-insensitive exact: %+v", got)
	}
}

// TestLookupWildcard: dated suffixed ids match the longest pattern —
// gpt-4o-mini-2024-07-18 must land on the mini card, not plain gpt-4o*.
func TestLookupWildcard(t *testing.T) {
	got := Lookup("gpt-4o-mini-2024-07-18")
	if got == nil || got.Family != "GPT-4o mini" {
		t.Fatalf("longest wildcard should win, got %+v", got)
	}
	if got := Lookup("gpt-4o-2024-08-06"); got == nil || got.Family != "GPT-4o" {
		t.Fatalf("gpt-4o dated: %+v", got)
	}
	// Anthropic versioned ids.
	if got := Lookup("claude-sonnet-4-5-20250929"); got == nil ||
		got.Family != "Claude Sonnet 4.5" {
		t.Fatalf("claude dated: %+v", got)
	}
}

// TestLookupMiss: unknown ids return nil — never guess a window.
func TestLookupMiss(t *testing.T) {
	for _, id := range []string{
		"", "   ", "totally-made-up-model", "gpt-5",
		"mimo-v2.6-flash", // the live gateway model — deliberately absent
	} {
		if got := Lookup(id); got != nil {
			t.Fatalf("Lookup(%q) = %+v, want nil", id, got)
		}
	}
}

// TestAllSanity: registry is non-empty, sorted, every entry carries a
// context window and a provider; wildcard ids end with '*'.
func TestAllSanity(t *testing.T) {
	all := All()
	if len(all) < 15 {
		t.Fatalf("registry too small: %d", len(all))
	}
	for i, e := range all {
		if e.Context <= 0 {
			t.Errorf("%s: no context window", e.ID)
		}
		if e.Provider == "" || e.Family == "" {
			t.Errorf("%s: missing provider/family", e.ID)
		}
		if strings.Contains(e.ID, "*") && !strings.HasSuffix(e.ID, "*") {
			t.Errorf("%s: wildcard not trailing", e.ID)
		}
		if i > 0 && all[i-1].ID >= e.ID {
			t.Errorf("registry not sorted: %s >= %s", all[i-1].ID, e.ID)
		}
	}
	// All() returns a copy — mutating it must not corrupt the registry.
	all[0].Family = "MUTATED"
	if fresh := All(); fresh[0].Family == "MUTATED" {
		t.Fatal("All() leaked the internal registry")
	}
}

// TestFormatting: price/context renderers, including the unknown marker.
func TestFormatting(t *testing.T) {
	priced := Lookup("deepseek-chat")
	if priced == nil {
		t.Fatal("deepseek-chat vanished")
	}
	if p := priced.FormatPrice(); !strings.Contains(p, "$0.27") ||
		!strings.Contains(p, "参考价") {
		t.Fatalf("FormatPrice: %q", p)
	}
	unknown := Lookup("claude-opus-5-5")
	if unknown == nil {
		t.Fatal("claude-opus-5-5 vanished")
	}
	if p := unknown.FormatPrice(); !strings.Contains(p, "未知") {
		t.Fatalf("zero pricing should read as unknown: %q", p)
	}
	if c := unknown.FormatContext(); c != "200000" {
		t.Fatalf("FormatContext: %q", c)
	}
}

// TestCostUSD: turn pricing with Anthropic's cache multipliers (read
// 0.1×, write 1.25×) — and no cost at all without a full price.
func TestCostUSD(t *testing.T) {
	// Known() needs both rates.
	if (Pricing{Input: 1}).Known() || (Pricing{}).Known() {
		t.Fatal("half-known pricing must not be Known()")
	}
	if c := (Pricing{}).CostUSD(1e6, 1e6, 1e6, 1e6); c != 0 {
		t.Fatalf("unknown pricing must cost 0, got %v", c)
	}

	p := Pricing{Input: 0.27, Output: 1.1}
	// 1000×0.27 + 2000×0.27×0.1 + 100×0.27×1.25 + 500×1.1
	want := (1000*0.27 + 2000*0.27*0.1 + 100*0.27*1.25 + 500*1.1) / 1e6
	got := p.CostUSD(1000, 500, 2000, 100)
	if diff := got - want; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("CostUSD = %.12f, want %.12f", got, want)
	}
	// 1M tokens at face value = the rates themselves.
	if diff := p.CostUSD(1e6, 0, 0, 0) - 0.27; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("1M input = %v, want 0.27", p.CostUSD(1e6, 0, 0, 0))
	}
}
