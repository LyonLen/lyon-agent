package models

import (
	"math"
	"testing"
)

func TestParseContextSize(t *testing.T) {
	cases := []struct {
		name string
		tags string
		desc string
		want int64
	}{
		{"size tag", "Reasoning,Tools,Vision,1M", "", 1_000_000},
		{"lowercase k", "tool,Vision,128k", "", 128_000},
		{"uppercase K", "256K", "", 256_000},
		{"no size tag", "tool,Vision,Reasoning", "", 0},
		{"desc fallback", "", "支持 1M 上下文、视觉理解", 1_000_000},
		{"desc no space", "", "支持1M上下文", 1_000_000},
		{"spec line is not a window", "", "3T 级开放权重模型", 0},
		{"nothing to parse", "tool", "no size here", 0},
		{"decimal", "1.5M", "", 1_500_000},
	}
	for _, c := range cases {
		if got := parseContextSize(c.tags, c.desc); got != c.want {
			t.Errorf("%s: parseContextSize(%q, %q) = %d, want %d",
				c.name, c.tags, c.desc, got, c.want)
		}
	}
}

func TestMergeGatewayPricing(t *testing.T) {
	// A curated id must never be shadowed by the overlay: static wins.
	MergeGatewayPricing([]GatewayPricing{{
		ModelName: "claude-opus-5-5",
		Tags:      "Reasoning,1M",
		ModelRatio: 99,
	}})
	if info := Lookup("claude-opus-5-5"); info == nil || info.Context != 200_000 {
		t.Fatalf("static card shadowed by gateway row: %+v", info)
	}
	if Source("claude-opus-5-5") != "registry" {
		t.Fatalf("Source(claude-opus-5-5) = %q, want registry", Source("claude-opus-5-5"))
	}

	// An unknown id resolves through the overlay: window from the tag,
	// prices from the ratios (input = ratio × $2/M, output × completion).
	MergeGatewayPricing([]GatewayPricing{{
		ModelName:       "kimi/kimi-k3.1-pro",
		Description:     "Kimi K3.1 Pro — 支持 1M 上下文、视觉理解",
		Tags:            "Reasoning,Tools,Vision,1M",
		Icon:            "Moonshot",
		QuotaType:       0,
		ModelRatio:      0.8904109589041096,
		CompletionRatio: 5.230769230769231,
		CacheRatio:      0.1,
	}})
	info := Lookup("kimi/kimi-k3.1-pro")
	if info == nil {
		t.Fatal("overlay id did not resolve")
	}
	if info.Context != 1_000_000 {
		t.Errorf("Context = %d, want 1_000_000", info.Context)
	}
	if math.Abs(info.Pricing.Input-1.78) > 0.01 {
		t.Errorf("Input = %.4f, want ≈1.78 (0.8904×2)", info.Pricing.Input)
	}
	if math.Abs(info.Pricing.Output-9.32) > 0.01 {
		t.Errorf("Output = %.4f, want ≈9.32 (input×5.2308)", info.Pricing.Output)
	}
	if math.Abs(info.Pricing.CacheRead-0.18) > 0.01 {
		t.Errorf("CacheRead = %.4f, want ≈0.18 (input×0.1)", info.Pricing.CacheRead)
	}
	if !info.Vision || !info.Reasoning {
		t.Errorf("capability tags lost: vision=%v reasoning=%v", info.Vision, info.Reasoning)
	}
	if info.Provider != "Moonshot" {
		t.Errorf("Provider = %q, want Moonshot (icon)", info.Provider)
	}
	if Source("kimi/kimi-k3.1-pro") != "gateway" {
		t.Errorf("Source = %q, want gateway", Source("kimi/kimi-k3.1-pro"))
	}

	// Nothing resolves an id the registry and the gateway both omit —
	// the caller keeps the launch fallback instead of a guessed window.
	if Lookup("no-such-model") != nil {
		t.Error("Lookup(no-such-model) resolved; unknown ids must stay nil")
	}
	if Source("no-such-model") != "" {
		t.Errorf("Source(no-such-model) = %q, want empty", Source("no-such-model"))
	}
}

func TestGatewayInfoPricingEdges(t *testing.T) {
	// Per-call quota (quota_type 1) and zero ratios stay unpriced —
	// never invent rates the gateway did not state.
	pr := gatewayInfo(GatewayPricing{ModelName: "x", QuotaType: 1, ModelRatio: 5})
	if pr.Pricing.Known() {
		t.Errorf("per-call row priced: %+v", pr.Pricing)
	}
	pr = gatewayInfo(GatewayPricing{ModelName: "x", QuotaType: 0, ModelRatio: 0})
	if pr.Pricing.Input != 0 {
		t.Errorf("zero ratio priced: %+v", pr.Pricing)
	}
	// Ratios present but completion missing → input only, still unknown.
	pr = gatewayInfo(GatewayPricing{ModelName: "x", QuotaType: 0, ModelRatio: 1})
	if pr.Pricing.Input != 2 || pr.Pricing.Known() {
		t.Errorf("input-only row: input=%v known=%v, want 2/false",
			pr.Pricing.Input, pr.Pricing.Known())
	}
}
