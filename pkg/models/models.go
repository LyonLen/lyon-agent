// Package models is the built-in model knowledge base: context windows,
// output caps, official parameter recommendations and reference pricing
// for the model families this CLI is likely to talk to. It powers the
// /model card in the TUI and feeds the context-usage display — no network,
// no surprises, and deliberately conservative: entries record only facts
// with a stable published source, and pricing is marked as reference
// (vendors reprice; trust the vendor page over this table).
//
// Lookup is exact first, then longest wildcard pattern ("gpt-4o-*"),
// case-insensitive. Unknown ids return nil — callers fall back to the
// configured ContextWindow rather than guessing.
package models

import (
	"fmt"
	"sort"
	"strings"
)

// Pricing is USD per million tokens (input / output). Zero = unknown.
// CacheRead/CacheWrite are USD per million for prompt-cache reads and
// writes; when both are zero CostUSD falls back to Anthropic's
// published multipliers (0.1× and 1.25× the input rate) — the only
// shape every other entry in this table states. A vendor that prices
// cache its own way fills them in instead: MiMo charges 2% of input
// for a hit and writes are free for now, so 0.1×/1.25× would overstate
// the bill by an order of magnitude.
type Pricing struct {
	Input      float64
	Output     float64
	CacheRead  float64 // USD/Mtok on a cache hit; 0 with CacheWrite 0 = use default multipliers
	CacheWrite float64 // USD/Mtok to write the cache; 0 beside a priced CacheRead = free
}

// Profile is a coarse capability map on a 0–5 scale — the self-knowledge
// data behind the recommendation router (docs/design/agent-evolution.md
// §5): how hard this model can think, how long it can stay on a task, how
// well it handles open-ended work, how fast it answers, how light the
// bill is. Curated hints, not benchmarks; CapProfile() defaults blanks
// to a flat mid-scale so an unprofiled entry never reads as "bad at
// everything".
type Profile struct {
	Reasoning byte `json:"reasoning"` // depth of thought on hard problems
	LongHaul  byte `json:"long_haul"` // staying power across long runs
	Openness  byte `json:"openness"`  // open-ended design/writing ability
	Speed     byte `json:"speed"`     // latency character (5 = fastest)
	Thrift    byte `json:"thrift"`    // cost efficiency (5 = cheapest)
}

// Info is one known model. Context/MaxOutput are token counts (0 =
// unstated). Temperature/TopP carry the vendor's recommendation as free
// text because the shapes differ (some models reject the fields entirely).
type Info struct {
	ID          string // exact id or pattern with a trailing wildcard
	Family      string // display family, e.g. "Claude Opus 4.5"
	Provider    string
	Context     int64 // input context window in tokens
	MaxOutput   int64 // max output tokens
	Pricing     Pricing
	Profile     Profile // capability dimensions (0–5); see CapProfile
	Temperature string  // recommended temperature, or "unsupported"
	TopP        string  // recommended top_p, or "unsupported"
	Reasoning   bool    // reasoning model: sampling params are fixed
	Vision      bool    // accepts image input (screenshots, photos)
	Cutoff      string  // knowledge cutoff, "YYYY-MM" or ""
	Notes       []string
}

// CapProfile returns the capability profile, filling an unset one with a
// flat 3s mid-scale.
func (i *Info) CapProfile() Profile {
	if i.Profile == (Profile{}) {
		return Profile{Reasoning: 3, LongHaul: 3, Openness: 3, Speed: 3, Thrift: 3}
	}
	return i.Profile
}

var registry = []Info{
	// ── Anthropic Claude ────────────────────────────────────────────────
	{ID: "claude-opus-5-5", Family: "Claude Opus 5", Provider: "Anthropic",
		Context: 200000, MaxOutput: 64000,
		Profile:     Profile{Reasoning: 5, LongHaul: 5, Openness: 5, Speed: 2, Thrift: 1},
		Vision:      true,
		Temperature: "1.0（官方默认）", TopP: "0.95 内",
		Notes: []string{"与 top_p 二选一，勿同时调", "1M 上下文为 beta 选项"}},
	{ID: "claude-opus-5*", Family: "Claude Opus 5", Provider: "Anthropic",
		Context: 200000, MaxOutput: 64000,
		Profile:     Profile{Reasoning: 5, LongHaul: 5, Openness: 5, Speed: 2, Thrift: 1},
		Vision:      true,
		Temperature: "1.0（官方默认）", TopP: "0.95 内",
		Notes: []string{"与 top_p 二选一，勿同时调"}},
	{ID: "claude-opus-4-5", Family: "Claude Opus 4.5", Provider: "Anthropic",
		Context: 200000, MaxOutput: 64000,
		Pricing:     Pricing{Input: 5, Output: 25},
		Profile:     Profile{Reasoning: 5, LongHaul: 5, Openness: 5, Speed: 2, Thrift: 2},
		Vision:      true,
		Temperature: "1.0（官方默认）", TopP: "0.95 内",
		Cutoff: "2025-01",
		Notes:  []string{"1M 上下文为 beta 选项"}},
	{ID: "claude-opus-4*", Family: "Claude Opus 4", Provider: "Anthropic",
		Context: 200000, MaxOutput: 32000,
		Pricing:     Pricing{Input: 15, Output: 75},
		Profile:     Profile{Reasoning: 5, LongHaul: 5, Openness: 5, Speed: 2, Thrift: 1},
		Vision:      true,
		Temperature: "1.0（官方默认）", TopP: "0.95 内",
		Cutoff: "2025-04"},
	{ID: "claude-sonnet-4-5*", Family: "Claude Sonnet 4.5", Provider: "Anthropic",
		Context: 200000, MaxOutput: 64000,
		Pricing:     Pricing{Input: 3, Output: 15},
		Profile:     Profile{Reasoning: 4, LongHaul: 4, Openness: 4, Speed: 3, Thrift: 3},
		Vision:      true,
		Temperature: "1.0（官方默认）", TopP: "0.95 内",
		Cutoff: "2025-07",
		Notes:  []string{"1M 上下文为 beta 选项"}},
	{ID: "claude-sonnet-4*", Family: "Claude Sonnet 4", Provider: "Anthropic",
		Context: 200000, MaxOutput: 64000,
		Pricing:     Pricing{Input: 3, Output: 15},
		Profile:     Profile{Reasoning: 4, LongHaul: 4, Openness: 4, Speed: 3, Thrift: 3},
		Vision:      true,
		Temperature: "1.0（官方默认）", TopP: "0.95 内",
		Cutoff: "2025-04"},
	{ID: "claude-haiku-4-5*", Family: "Claude Haiku 4.5", Provider: "Anthropic",
		Context: 200000, MaxOutput: 64000,
		Pricing:     Pricing{Input: 1, Output: 5},
		Profile:     Profile{Reasoning: 3, LongHaul: 3, Openness: 3, Speed: 5, Thrift: 4},
		Vision:      true,
		Temperature: "1.0（官方默认）", TopP: "0.95 内",
		Cutoff: "2025-07"},
	{ID: "claude-3-7-sonnet*", Family: "Claude 3.7 Sonnet", Provider: "Anthropic",
		Context: 200000, MaxOutput: 64000,
		Pricing:     Pricing{Input: 3, Output: 15},
		Profile:     Profile{Reasoning: 4, LongHaul: 4, Openness: 4, Speed: 3, Thrift: 3},
		Vision:      true,
		Temperature: "1.0（官方默认）", TopP: "0.95 内"},
	{ID: "claude-3-5-haiku*", Family: "Claude 3.5 Haiku", Provider: "Anthropic",
		Context: 200000, MaxOutput: 8192,
		Pricing:     Pricing{Input: 0.8, Output: 4},
		Profile:     Profile{Reasoning: 3, LongHaul: 2, Openness: 3, Speed: 5, Thrift: 4},
		Vision:      true,
		Temperature: "1.0（官方默认）", TopP: "0.95 内"},

	// ── OpenAI ─────────────────────────────────────────────────────────
	{ID: "gpt-4o-mini*", Family: "GPT-4o mini", Provider: "OpenAI",
		Context: 128000, MaxOutput: 16384,
		Pricing:     Pricing{Input: 0.15, Output: 0.6},
		Profile:     Profile{Reasoning: 3, LongHaul: 2, Openness: 3, Speed: 5, Thrift: 5},
		Vision:      true,
		Temperature: "1.0（官方默认）", TopP: "1.0（默认）", Cutoff: "2024-10"},
	{ID: "gpt-4o*", Family: "GPT-4o", Provider: "OpenAI",
		Context: 128000, MaxOutput: 16384,
		Pricing:     Pricing{Input: 2.5, Output: 10},
		Profile:     Profile{Reasoning: 3, LongHaul: 3, Openness: 3, Speed: 4, Thrift: 3},
		Vision:      true,
		Temperature: "1.0（官方默认）", TopP: "1.0（默认）", Cutoff: "2024-10"},
	{ID: "gpt-4.1-mini*", Family: "GPT-4.1 mini", Provider: "OpenAI",
		Context: 1047576, MaxOutput: 32768,
		Pricing:     Pricing{Input: 0.4, Output: 1.6},
		Profile:     Profile{Reasoning: 3, LongHaul: 3, Openness: 3, Speed: 4, Thrift: 5},
		Vision:      true,
		Temperature: "0.7（社区常用）", TopP: "1.0（默认）", Cutoff: "2024-01"},
	{ID: "gpt-4.1*", Family: "GPT-4.1", Provider: "OpenAI",
		Context: 1047576, MaxOutput: 32768,
		Pricing:     Pricing{Input: 2, Output: 8},
		Profile:     Profile{Reasoning: 4, LongHaul: 4, Openness: 4, Speed: 3, Thrift: 4},
		Vision:      true,
		Temperature: "0.7（社区常用）", TopP: "1.0（默认）", Cutoff: "2024-06"},
	{ID: "o3-mini*", Family: "o3-mini", Provider: "OpenAI",
		Context: 200000, MaxOutput: 100000,
		Pricing:     Pricing{Input: 1.1, Output: 4.4},
		Profile:     Profile{Reasoning: 4, LongHaul: 3, Openness: 3, Speed: 3, Thrift: 4},
		Temperature: "unsupported", TopP: "unsupported", Reasoning: true,
		Cutoff: "2024-04",
		Notes:  []string{"推理模型：temp/top_p 必须留默认，reasoning_effort 可调"}},
	{ID: "o3*", Family: "o3", Provider: "OpenAI",
		Context: 200000, MaxOutput: 100000,
		Pricing:     Pricing{Input: 2, Output: 8},
		Profile:     Profile{Reasoning: 5, LongHaul: 4, Openness: 3, Speed: 2, Thrift: 2},
		Vision:      true,
		Temperature: "unsupported", TopP: "unsupported", Reasoning: true,
		Cutoff: "2024-06",
		Notes:  []string{"推理模型：temp/top_p 必须留默认"}},
	{ID: "o4-mini*", Family: "o4-mini", Provider: "OpenAI",
		Context: 200000, MaxOutput: 100000,
		Pricing:     Pricing{Input: 1.1, Output: 4.4},
		Profile:     Profile{Reasoning: 4, LongHaul: 3, Openness: 3, Speed: 4, Thrift: 4},
		Vision:      true,
		Temperature: "unsupported", TopP: "unsupported", Reasoning: true,
		Notes: []string{"推理模型：temp/top_p 必须留默认"}},
	{ID: "o1*", Family: "o1", Provider: "OpenAI",
		Context: 200000, MaxOutput: 100000,
		Pricing:     Pricing{Input: 15, Output: 60},
		Profile:     Profile{Reasoning: 5, LongHaul: 4, Openness: 3, Speed: 1, Thrift: 1},
		Vision:      true,
		Temperature: "unsupported", TopP: "unsupported", Reasoning: true,
		Notes: []string{"推理模型：temp/top_p 必须留默认"}},

	// ── Google Gemini ──────────────────────────────────────────────────
	{ID: "gemini-2.5-pro*", Family: "Gemini 2.5 Pro", Provider: "Google",
		Context: 1048576, MaxOutput: 65536,
		Pricing:     Pricing{Input: 1.25, Output: 10},
		Profile:     Profile{Reasoning: 5, LongHaul: 5, Openness: 4, Speed: 3, Thrift: 3},
		Vision:      true,
		Temperature: "1.0（默认）", TopP: "0.95", Cutoff: "2025-01",
		Notes: []string{"≤200K 输入按低价档计费，超出阶梯加价", "2M 上下文为 preview"}},
	{ID: "gemini-2.5-flash*", Family: "Gemini 2.5 Flash", Provider: "Google",
		Context: 1048576, MaxOutput: 65536,
		Pricing:     Pricing{Input: 0.3, Output: 2.5},
		Profile:     Profile{Reasoning: 3, LongHaul: 3, Openness: 3, Speed: 5, Thrift: 4},
		Vision:      true,
		Temperature: "1.0（默认）", TopP: "0.95", Cutoff: "2025-01"},
	{ID: "gemini-2.0-flash*", Family: "Gemini 2.0 Flash", Provider: "Google",
		Context: 1048576, MaxOutput: 8192,
		Pricing:     Pricing{Input: 0.1, Output: 0.4},
		Profile:     Profile{Reasoning: 3, LongHaul: 2, Openness: 3, Speed: 5, Thrift: 5},
		Vision:      true,
		Temperature: "1.0（默认）", TopP: "0.95"},

	// ── DeepSeek ───────────────────────────────────────────────────────
	{ID: "deepseek-chat", Family: "DeepSeek V3", Provider: "DeepSeek",
		Context: 65536, MaxOutput: 8192,
		Pricing:     Pricing{Input: 0.27, Output: 1.1},
		Profile:     Profile{Reasoning: 3, LongHaul: 3, Openness: 3, Speed: 4, Thrift: 5},
		Temperature: "0.6（官方推荐）", TopP: "0.95 内", Cutoff: "2024-12"},
	{ID: "deepseek-reasoner", Family: "DeepSeek R1", Provider: "DeepSeek",
		Context: 65536, MaxOutput: 8192,
		Pricing:     Pricing{Input: 0.55, Output: 2.19},
		Profile:     Profile{Reasoning: 5, LongHaul: 4, Openness: 4, Speed: 3, Thrift: 5},
		Temperature: "0.6（官方推荐）", TopP: "0.95 内", Reasoning: true,
		Cutoff: "2024-12",
		Notes:  []string{"思维链输出计入 completion tokens"}},

	{ID: "deepseek-v4.1-flash*", Family: "DeepSeek V4.1 Flash", Provider: "DeepSeek",
		Context: 1000000,
		Profile: Profile{Reasoning: 4, LongHaul: 4, Openness: 3, Speed: 5, Thrift: 4},
		Vision:  true,
		Notes:   []string{"1M 上下文，支持图像输入"}},

	// ── 其他常见开源/托管 ───────────────────────────────────────────────
	{ID: "qwen2.5-72b-instruct*", Family: "Qwen2.5 72B", Provider: "Alibaba",
		Context: 131072, MaxOutput: 8192,
		Profile:     Profile{Reasoning: 3, LongHaul: 3, Openness: 3, Speed: 4, Thrift: 4},
		Temperature: "0.7（官方默认）", TopP: "0.95 内"},
	{ID: "qwen-max*", Family: "Qwen Max", Provider: "Alibaba",
		Context: 32768, MaxOutput: 8192,
		Profile:     Profile{Reasoning: 4, LongHaul: 4, Openness: 4, Speed: 3, Thrift: 4},
		Temperature: "0.7（官方默认）", TopP: "0.95 内",
		Notes: []string{"价格以人民币计，未收录换算"}},
	{ID: "qwen-plus*", Family: "Qwen Plus", Provider: "Alibaba",
		Context: 131072, MaxOutput: 8192,
		Profile:     Profile{Reasoning: 3, LongHaul: 3, Openness: 3, Speed: 4, Thrift: 5},
		Temperature: "0.7（官方默认）", TopP: "0.95 内",
		Notes: []string{"价格以人民币计，未收录换算"}},
	{ID: "llama-3.3-70b*", Family: "Llama 3.3 70B", Provider: "Meta",
		Context: 131072, MaxOutput: 8192,
		Profile:     Profile{Reasoning: 3, LongHaul: 3, Openness: 3, Speed: 4, Thrift: 4},
		Temperature: "0.6（官方示例）", TopP: "0.9 内",
		Notes: []string{"托管商价格差异大，未收录"}},
	{ID: "mistral-large*", Family: "Mistral Large", Provider: "Mistral",
		Context: 131072, MaxOutput: 8192,
		Profile:     Profile{Reasoning: 3, LongHaul: 3, Openness: 3, Speed: 4, Thrift: 4},
		Temperature: "0.7（官方默认）", TopP: "1.0（默认）"},

	// ── Xiaomi MiMo — the model this gateway actually serves ─────────
	// Source: mimo.mi.com/llms.txt → static/docs (model summary, the
	// pay-as-you-go price tables, model hyperparameters). All five text
	// models are 1M context / 128K output; prices are the overseas USD
	// rates (the domestic table is in RMB and is not converted here).
	// Cache rates are per-entry because MiMo's shape (hit at 2% of
	// input, write free for now) is nothing like Anthropic's.
	{ID: "mimo-v2.6-flash", Family: "MiMo V2.6 Flash", Provider: "Xiaomi",
		Context: 1000000, MaxOutput: 128000,
		Pricing:     Pricing{Input: 0.14, Output: 0.28, CacheRead: 0.0028},
		Profile:     Profile{Reasoning: 3, LongHaul: 3, Openness: 3, Speed: 5, Thrift: 5},
		Vision:      true,
		Temperature: "1.0（默认，0~1.5）", TopP: "0.95（默认，0.01~1）",
		Notes: []string{
			"缓存命中 $0.0028/M、写入限时期免费；Batch 半价",
			"Deep Thinking 由 reasoning 参数开启，开启时 temperature/top_p 强制 1.0/0.95",
			"限流 RPM 100 / TPM 10M",
		}},
	{ID: "mimo-v2.6-pro", Family: "MiMo V2.6 Pro", Provider: "Xiaomi",
		Context: 1000000, MaxOutput: 128000,
		Pricing:     Pricing{Input: 0.435, Output: 0.87, CacheRead: 0.0036},
		Profile:     Profile{Reasoning: 4, LongHaul: 4, Openness: 4, Speed: 4, Thrift: 4},
		Vision:      true,
		Temperature: "1.0（默认，0~1.5）", TopP: "0.95（默认，0.01~1）",
		Notes: []string{
			"缓存命中 $0.0036/M、写入限时期免费",
			"Deep Thinking 由 reasoning 参数开启，开启时 temperature/top_p 强制 1.0/0.95",
		}},
	{ID: "mimo-v2.6-pro-ultraspeed", Family: "MiMo V2.6 Pro UltraSpeed", Provider: "Xiaomi",
		Context: 1000000, MaxOutput: 128000,
		Pricing:     Pricing{Input: 4.35, Output: 8.7, CacheRead: 0.036},
		Profile:     Profile{Reasoning: 4, LongHaul: 4, Openness: 4, Speed: 5, Thrift: 1},
		Vision:      true,
		Temperature: "1.0（默认，0~1.5）", TopP: "0.95（默认，0.01~1）",
		Notes: []string{
			"缓存命中 $0.036/M、写入限时期免费；不支持 Batch API",
			"面向强实时场景，单价是 flash 的 31 倍",
		}},
	{ID: "mimo-v2.5", Family: "MiMo V2.5", Provider: "Xiaomi",
		Context: 1000000, MaxOutput: 128000,
		Pricing:     Pricing{Input: 0.14, Output: 0.28, CacheRead: 0.0028},
		Profile:     Profile{Reasoning: 3, LongHaul: 3, Openness: 3, Speed: 5, Thrift: 5},
		Vision:      true,
		Temperature: "1.0（默认，0~1.5）", TopP: "0.95（默认，0.01~1）",
		Notes: []string{
			"2026-10-21 10:00（北京时间）正式下线，迁 mimo-v2.6-flash",
			"官方价格与 mimo-v2.6-flash 同组",
		}},
	{ID: "mimo-v2.5-pro", Family: "MiMo V2.5 Pro", Provider: "Xiaomi",
		Context: 1000000, MaxOutput: 128000,
		Pricing:     Pricing{Input: 0.435, Output: 0.87, CacheRead: 0.0036},
		Profile:     Profile{Reasoning: 4, LongHaul: 4, Openness: 4, Speed: 4, Thrift: 4},
		Vision:      true,
		Temperature: "1.0（默认，0~1.5）", TopP: "0.95（默认，0.01~1）",
		Notes: []string{
			"2026-10-21 10:00（北京时间）正式下线，迁 mimo-v2.6-pro",
			"官方价格与 mimo-v2.6-pro 同组",
		}},
}

func init() {
	sort.SliceStable(registry, func(i, j int) bool { return registry[i].ID < registry[j].ID })
}

// Lookup resolves a model id to its knowledge card. Exact match first
// (case-insensitive), then the longest wildcard pattern that prefixes it.
// Unknown ids return nil — never guess a context window.
func Lookup(id string) *Info {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" {
		return nil
	}
	var best *Info
	bestLen := -1
	for i := range registry {
		e := &registry[i]
		p := e.ID
		if !strings.Contains(p, "*") {
			if strings.EqualFold(p, id) {
				return e
			}
			continue
		}
		prefix := strings.ToLower(strings.TrimSuffix(p, "*"))
		if strings.HasPrefix(id, prefix) && len(prefix) > bestLen {
			best, bestLen = e, len(prefix)
		}
	}
	return best
}

// All returns the full registry (sorted by id as initialized).
func All() []Info {
	out := make([]Info, len(registry))
	copy(out, registry)
	return out
}

// FormatPrice renders the pricing line, or a marker when unknown.
func (i *Info) FormatPrice() string {
	if i.Pricing.Input == 0 && i.Pricing.Output == 0 {
		return "未知（以官网为准）"
	}
	return fmt.Sprintf("$%.2f / $%.2f 每百万 token（参考价）",
		i.Pricing.Input, i.Pricing.Output)
}

// FormatContext renders the context window with thousands separators.
func (i *Info) FormatContext() string {
	if i.Context == 0 {
		return "未知"
	}
	s := fmt.Sprintf("%d", i.Context)
	var b strings.Builder
	for n, r := range s {
		if n > 0 && (len(s)-n)%3 == 0 && r != '-' {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Known reports whether both unit prices are on file — the only state
// in which cost is computed (a half-known price would silently mislead).
func (p Pricing) Known() bool { return p.Input > 0 && p.Output > 0 }

// CostUSD prices one API turn in USD: input and output at face value,
// cache read and cache write at the rates this entry carries. An entry
// that records no cache rate at all (both zero) keeps the published
// Anthropic multipliers — reads at 0.1×, writes at 1.25× the input
// rate. A zero CacheWrite beside a priced CacheRead means the vendor's
// writes are free (MiMo's limited-time offer), not "unset".
// Unknown pricing returns 0; gate on Known() first.
func (p Pricing) CostUSD(input, output, cacheRead, cacheCreation int64) float64 {
	if !p.Known() {
		return 0
	}
	cr, cw := p.CacheRead, p.CacheWrite
	if cr == 0 && cw == 0 {
		cr, cw = p.Input*0.1, p.Input*1.25
	}
	return (float64(input)*p.Input +
		float64(cacheRead)*cr +
		float64(cacheCreation)*cw +
		float64(output)*p.Output) / 1e6
}
