// Gateway overlay: New-API-compatible gateways expose GET /api/pricing —
// rows carrying a free-text description, capability tags
// ("Reasoning,Tools,Vision,1M") and quota ratios. The curated registry
// stays the source of truth for the ids it knows; this overlay fills only
// the gaps, so a model the registry has never heard of still reports the
// context window its gateway declares (the ctx gauge and the compaction
// denominator) instead of the 200k launch fallback. This package still
// performs no network I/O: callers fetch the payload (server-side
// refreshGatewayKB) and hand the rows to MergeGatewayPricing.
package models

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// GatewayPricing is the subset of a New-API /api/pricing row lyon reads.
// JSON tags mirror the wire names exactly (probe: model_name, tags,
// icon, owner_by, quota_type, ratios…).
type GatewayPricing struct {
	ModelName        string  `json:"model_name"`
	Description      string  `json:"description"`
	Tags             string  `json:"tags"`
	Icon             string  `json:"icon"`
	OwnerBy          string  `json:"owner_by"`
	QuotaType        int     `json:"quota_type"` // 0 = per-token quota, 1 = per-call
	ModelRatio       float64 `json:"model_ratio"`
	CompletionRatio  float64 `json:"completion_ratio"`
	CacheRatio       float64 `json:"cache_ratio"`
	CreateCacheRatio float64 `json:"create_cache_ratio"`
}

// ratioToUSD converts one-api's quota convention to USD per million
// tokens: model_ratio 1 = $0.002 / 1K tokens = $2 / Mtok. Verified
// against the live gateway (kimi-k3.1-pro ratio 0.8904… → $1.78/M in).
const ratioToUSD = 2.0

var (
	gatewayMu sync.RWMutex
	gatewayKB = map[string]*Info{} // key: lowercased model id
)

// MergeGatewayPricing folds one /api/pricing payload into the overlay.
// Ids the curated registry knows are skipped — their cards carry profiles,
// output caps and notes a gateway cannot express, and static wins; every
// other named row replaces its overlay entry. Safe for concurrent use;
// returns the overlay size after the merge.
func MergeGatewayPricing(rows []GatewayPricing) int {
	gatewayMu.Lock()
	defer gatewayMu.Unlock()
	for _, r := range rows {
		id := strings.ToLower(strings.TrimSpace(r.ModelName))
		if id == "" || lookupStatic(id) != nil {
			continue
		}
		gatewayKB[id] = gatewayInfo(r)
	}
	return len(gatewayKB)
}

// lookupGateway reads the overlay. The id must already be lowercased —
// Lookup and Source normalize before calling.
func lookupGateway(id string) *Info {
	gatewayMu.RLock()
	defer gatewayMu.RUnlock()
	return gatewayKB[id]
}

// Source names where a resolved card came from: "registry" (curated),
// "gateway" (a /api/pricing sync), or "" when nothing resolves the id.
// Callers that report a window must still gate on info.Context > 0 — an
// overlay row without a parseable size is a pricing source but must not
// claim to be the gauge's denominator.
func Source(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" {
		return ""
	}
	if lookupStatic(id) != nil {
		return "registry"
	}
	if lookupGateway(id) != nil {
		return "gateway"
	}
	return ""
}

var (
	// tagSizeRe matches a standalone size tag: "1M", "128k", "256K".
	tagSizeRe = regexp.MustCompile(`(?i)^(\d+(?:\.\d+)?)([km])$`)
	// descSizeRe finds the window inside a description: "支持 1M 上下文",
	// "128k 上下文". Only 上下文 counts — a spec line like "3T 级权重"
	// is not a context claim, so it never matches. Case-insensitive on
	// the unit: descriptions write both "1M" and "128k".
	descSizeRe = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*([km])\s*上下文`)
)

// sizeTokens scales a numeric size with its unit: 1M → 1_000_000,
// 128k → 128_000 (decimals allowed: 1.5M → 1_500_000). Anything
// unparseable returns 0.
func sizeTokens(num, unit string) int64 {
	v, err := strconv.ParseFloat(num, 64)
	if err != nil || v <= 0 {
		return 0
	}
	switch strings.ToLower(unit) {
	case "k":
		v *= 1_000
	case "m":
		v *= 1_000_000
	default:
		return 0
	}
	return int64(math.Round(v))
}

// parseContextSize reads the window off the gateway's own words: the
// size tag first ("Reasoning,Tools,Vision,1M"), then the description
// ("支持 1M 上下文"). Returns 0 when neither states one — callers keep
// the launch fallback rather than inventing a number.
func parseContextSize(tags, desc string) int64 {
	for _, t := range strings.Split(tags, ",") {
		if m := tagSizeRe.FindStringSubmatch(strings.TrimSpace(t)); m != nil {
			if n := sizeTokens(m[1], m[2]); n > 0 {
				return n
			}
		}
	}
	if m := descSizeRe.FindStringSubmatch(desc); m != nil {
		return sizeTokens(m[1], m[2])
	}
	return 0
}

// round2 rounds a USD rate to cents — the picker's display precision.
func round2(v float64) float64 { return math.Round(v*100) / 100 }

// gatewayInfo turns one pricing row into an Info card. Quota_type 0 rows
// carry token ratios: input = model_ratio × $2/M, output = input ×
// completion_ratio, cache legs from their ratios; a missing ratio stays
// unpriced (Known() false) rather than being invented. Tags are
// lowercased into a comma-wrapped haystack so ",vision," cannot match a
// name that merely contains the letters.
func gatewayInfo(r GatewayPricing) *Info {
	var pr Pricing
	if r.QuotaType == 0 && r.ModelRatio > 0 {
		in := r.ModelRatio * ratioToUSD
		pr = Pricing{Input: round2(in)}
		if r.CompletionRatio > 0 {
			pr.Output = round2(in * r.CompletionRatio)
		}
		if r.CacheRatio > 0 {
			pr.CacheRead = round2(in * r.CacheRatio)
		}
		if r.CreateCacheRatio > 0 {
			pr.CacheWrite = round2(in * r.CreateCacheRatio)
		}
	}
	hay := "," + strings.ToLower(strings.ReplaceAll(r.Tags, " ", "")) + ","
	provider := strings.TrimSpace(r.Icon)
	if provider == "" {
		provider = strings.TrimSpace(r.OwnerBy)
	}
	if provider == "" {
		provider = "网关"
	}
	var notes []string
	if d := strings.TrimSpace(r.Description); d != "" {
		notes = append(notes, d)
	}
	notes = append(notes, "来自网关 /api/pricing：窗口取标签，价格按倍率换算")
	return &Info{
		ID:        strings.TrimSpace(r.ModelName),
		Provider:  provider,
		Context:   parseContextSize(r.Tags, r.Description),
		Pricing:   pr,
		Vision:    strings.Contains(hay, ",vision,"),
		Reasoning: strings.Contains(hay, ",reasoning,"),
		Notes:     notes,
	}
}
