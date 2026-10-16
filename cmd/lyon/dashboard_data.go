package main

// Dashboard data sources (#10): every fetcher is context-bounded, sets a
// browser-like User-Agent (several CDNs reject bare UAs), and takes its
// base URL from dashEndpoints — tests point those at httptest stubs, so
// the suite never touches the real network. All sources are free, keyless
// and CN-reachable; the report carries the URL/rationale table.

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"lyon-agent/pkg/memory"
)

// dashEndpoints holds the overridable bases. Production never mutates it;
// tests swap fields under dashEndpointsMu for the duration of a case.
var dashEndpoints = struct {
	mu sync.RWMutex
	// Geo is ipwho.is (HTTPS, keyless, city+coords). GeoFallback is
	// ip-api.com — HTTP-only on the free tier, used only on primary failure.
	Geo, GeoFallback string
	// Weather is open-meteo: keyless lat/lon forecast, no account needed.
	Weather string
	// LeetCode is the leetcode.cn GraphQL endpoint (questionOfTodayV2 —
	// the undocumented query the site's own frontend fires).
	LeetCode string
	// Stock is eastmoney's single-quote GET (fltt=2 returns plain numbers).
	Stock string
}{
	Geo:         "https://ipwho.is/",
	GeoFallback: "http://ip-api.com/json/",
	Weather:     "https://api.open-meteo.com/v1/forecast",
	LeetCode:    "https://leetcode.cn/graphql",
	Stock:       "https://push2.eastmoney.com/api/qt/stock/get",
}

func dashEndpoint(field string) string {
	dashEndpoints.mu.RLock()
	defer dashEndpoints.mu.RUnlock()
	switch field {
	case "geo":
		return dashEndpoints.Geo
	case "geoFallback":
		return dashEndpoints.GeoFallback
	case "weather":
		return dashEndpoints.Weather
	case "leetcode":
		return dashEndpoints.LeetCode
	case "stock":
		return dashEndpoints.Stock
	}
	return ""
}

func dashSetEndpoint(field, value string) {
	dashEndpoints.mu.Lock()
	defer dashEndpoints.mu.Unlock()
	switch field {
	case "geo":
		dashEndpoints.Geo = value
	case "geoFallback":
		dashEndpoints.GeoFallback = value
	case "weather":
		dashEndpoints.Weather = value
	case "leetcode":
		dashEndpoints.LeetCode = value
	case "stock":
		dashEndpoints.Stock = value
	}
}

// dashUA mimics a browser; ruanyifeng's Cloudflare challenge and several
// RSS hosts reject requests without one. Timeout rides the ctx — the
// refresher arms 15s budgets, never the TUI goroutine.
const dashUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36"

var dashHTTP = &http.Client{
	Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        32,
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	},
}

// dashGet fetches a URL into memory with the shared UA. Bodies are capped
// at 4MB — a runaway feed must not pin memory.
func dashGet(ctx context.Context, rawURL string, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", dashUA)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := dashHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("GET %s: status %d", rawURL, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

// ---------------------------------------------------------------------------
// Group 2: IP geolocation + weather
// ---------------------------------------------------------------------------

// geoData is one resolved location: city-level text plus the coordinates
// the weather module consumes.
type geoData struct {
	IP      string  `json:"ip"`
	City    string  `json:"city"`
	Region  string  `json:"region"`
	Country string  `json:"country"`
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
}

// fetchGeo hits ipwho.is first, then ip-api.com: both are keyless, but
// each occasionally drops — the pair makes "no location" rare, and a
// total failure degrades the weather block instead of panicking.
func fetchGeo(ctx context.Context) (any, error) {
	if g, err := fetchGeoPrimary(ctx); err == nil {
		return g, nil
	}
	return fetchGeoFallback(ctx)
}

func fetchGeoPrimary(ctx context.Context) (*geoData, error) {
	body, err := dashGet(ctx, dashEndpoint("geo"), nil)
	if err != nil {
		return nil, err
	}
	var r struct {
		Success bool    `json:"success"`
		IP      string  `json:"ip"`
		City    string  `json:"city"`
		Region  string  `json:"region"`
		Country string  `json:"country"`
		Lat     float64 `json:"latitude"`
		Lon     float64 `json:"longitude"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	if !r.Success || (r.Lat == 0 && r.Lon == 0 && r.City == "") {
		return nil, fmt.Errorf("ipwho.is: lookup not successful")
	}
	return &geoData{IP: r.IP, City: r.City, Region: r.Region, Country: r.Country, Lat: r.Lat, Lon: r.Lon}, nil
}

func fetchGeoFallback(ctx context.Context) (*geoData, error) {
	u := dashEndpoint("geoFallback") + "?fields=status,message,query,city,regionName,country,lat,lon"
	body, err := dashGet(ctx, u, nil)
	if err != nil {
		return nil, err
	}
	var r struct {
		Status string  `json:"status"`
		Query  string  `json:"query"`
		City   string  `json:"city"`
		Region string  `json:"regionName"`
		Ctry   string  `json:"country"`
		Lat    float64 `json:"lat"`
		Lon    float64 `json:"lon"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	if r.Status != "success" {
		return nil, fmt.Errorf("ip-api.com: %s", r.Status)
	}
	return &geoData{IP: r.Query, City: r.City, Region: r.Region, Country: r.Ctry, Lat: r.Lat, Lon: r.Lon}, nil
}

// weatherData mirrors open-meteo's current+daily slice for one day.
type weatherData struct {
	Lat  float64 `json:"lat"`
	Lon  float64 `json:"lon"`
	Temp float64 `json:"temp"`
	Code int     `json:"code"`
	Wind float64 `json:"wind"`
	Hi   float64 `json:"hi"`
	Lo   float64 `json:"lo"`
}

func fetchWeather(ctx context.Context, lat, lon float64) (any, error) {
	q := url.Values{}
	q.Set("latitude", fmt.Sprintf("%.4f", lat))
	q.Set("longitude", fmt.Sprintf("%.4f", lon))
	q.Set("current", "temperature_2m,weather_code,wind_speed_10m")
	q.Set("daily", "temperature_2m_max,temperature_2m_min")
	q.Set("timezone", "auto")
	q.Set("forecast_days", "1")
	body, err := dashGet(ctx, dashEndpoint("weather")+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	var r struct {
		Current struct {
			Temp float64 `json:"temperature_2m"`
			Code int     `json:"weather_code"`
			Wind float64 `json:"wind_speed_10m"`
		} `json:"current"`
		Daily struct {
			Hi []float64 `json:"temperature_2m_max"`
			Lo []float64 `json:"temperature_2m_min"`
		} `json:"daily"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	w := &weatherData{Lat: lat, Lon: lon, Temp: r.Current.Temp, Code: r.Current.Code, Wind: r.Current.Wind}
	if len(r.Daily.Hi) > 0 {
		w.Hi = r.Daily.Hi[0]
	}
	if len(r.Daily.Lo) > 0 {
		w.Lo = r.Daily.Lo[0]
	}
	return w, nil
}

// weatherDesc maps the WMO weather interpretation codes to a bilingual
// pair; unknown codes read as "cloudy" rather than a raw number.
func weatherDesc(code int) (en, zh string) {
	switch {
	case code == 0:
		return "clear", "晴"
	case code == 1:
		return "mainly clear", "大部晴朗"
	case code == 2:
		return "partly cloudy", "局部多云"
	case code == 3:
		return "overcast", "阴"
	case code == 45 || code == 48:
		return "fog", "雾"
	case code >= 51 && code <= 55:
		return "drizzle", "毛毛雨"
	case code >= 56 && code <= 59:
		return "freezing drizzle", "冻毛毛雨"
	case code >= 61 && code <= 63:
		return "rain", "雨"
	case code >= 65 && code <= 67:
		return "heavy rain", "强雨"
	case code >= 71 && code <= 77:
		return "snow", "雪"
	case code >= 80 && code <= 82:
		return "showers", "阵雨"
	case code == 85 || code == 86:
		return "snow showers", "阵雪"
	case code >= 95 && code <= 99:
		return "thunderstorm", "雷暴"
	}
	return "cloudy", "多云"
}

// ---------------------------------------------------------------------------
// Group 3: RSS news + personality profile + local tip bank
// ---------------------------------------------------------------------------

// rssItem is one headline from either feed dialect.
type rssItem struct {
	Title string    `json:"title"`
	Link  string    `json:"link"`
	Pub   time.Time `json:"pub"`
}

// briefData is one refresh of the news block: profile keywords distilled
// from the memory personal layer, ranked headlines, and how many feeds
// answered (feeds that failed simply contribute nothing).
type briefData struct {
	Profile []string  `json:"profile"`
	Items   []rssItem `json:"items"`
	FeedsOK int       `json:"feeds_ok"`
}

// rssDoc / atomDoc cover the two dialects the default feeds speak.
type rssDoc struct {
	Channel struct {
		Items []struct {
			Title   string `xml:"title"`
			Link    string `xml:"link"`
			PubDate string `xml:"pubDate"`
		} `xml:"item"`
	} `xml:"channel"`
}

type atomDoc struct {
	Entries []struct {
		Title     string `xml:"title"`
		Published string `xml:"published"`
		Updated   string `xml:"updated"`
		Links     []struct {
			Href string `xml:"href,attr"`
			Rel  string `xml:"rel,attr"`
		} `xml:"link"`
	} `xml:"entry"`
}

// parseFeed auto-detects Atom (root <feed>) vs RSS 2.0 (root <rss>),
// decodes entities once (feeds double-escape titles), and leaves Pub
// zero when no date parses — callers sort zeros last.
func parseFeed(data []byte) ([]rssItem, error) {
	trimmed := strings.TrimSpace(string(data))
	if strings.HasPrefix(trimmed, "<?xml") {
		if i := strings.Index(trimmed, ">"); i > 0 {
			trimmed = strings.TrimSpace(trimmed[i+1:])
		}
	}
	var items []rssItem
	if strings.HasPrefix(trimmed, "<feed") {
		var doc atomDoc
		if err := xml.Unmarshal(data, &doc); err != nil {
			return nil, err
		}
		for _, e := range doc.Entries {
			link := ""
			for _, l := range e.Links {
				if l.Rel == "" || l.Rel == "alternate" {
					link = l.Href
					break
				}
			}
			ts := e.Published
			if ts == "" {
				ts = e.Updated
			}
			items = append(items, rssItem{
				Title: cleanTitle(e.Title),
				Link:  link,
				Pub:   parseFeedTime(ts),
			})
		}
		return items, nil
	}
	var doc rssDoc
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	for _, it := range doc.Channel.Items {
		items = append(items, rssItem{
			Title: cleanTitle(it.Title),
			Link:  strings.TrimSpace(it.Link),
			Pub:   parseFeedTime(it.PubDate),
		})
	}
	return items, nil
}

// cleanTitle flattens whitespace, unescapes HTML entities and sanitizes
// escape sequences a hostile title might smuggle — the transcript is a
// terminal, not an HTML sink.
func cleanTitle(s string) string {
	s = html.UnescapeString(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return sanitizeText(s)
}

var feedTimeLayouts = []string{
	time.RFC1123Z, time.RFC1123, time.RFC822Z, time.RFC822,
	time.RFC3339, "Mon, 2 Jan 2006 15:04:05 -0700", "2006-01-02 15:04:05",
}

func parseFeedTime(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range feedTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// fetchBrief fans the configured feeds out concurrently (one goroutine
// each — a slow host must not delay the others), ranks headlines by
// profile-keyword overlap then recency, and succeeds as long as ONE feed
// answered; total failure propagates so the cell can go stale-on-cache.
func fetchBrief(ctx context.Context, feeds []string, workspace string) (any, error) {
	if len(feeds) == 0 {
		feeds = defaultDashFeeds()
	}
	type result struct {
		items []rssItem
		err   error
	}
	results := make([]result, len(feeds))
	var wg sync.WaitGroup
	for i, f := range feeds {
		wg.Add(1)
		go func(i int, f string) {
			defer wg.Done()
			body, err := dashGet(ctx, f, nil)
			if err != nil {
				results[i] = result{err: err}
				return
			}
			items, err := parseFeed(body)
			results[i] = result{items: items, err: err}
		}(i, f)
	}
	wg.Wait()

	profile, _ := dashProfile(workspace)
	var all []rssItem
	ok := 0
	for _, r := range results {
		if r.err == nil {
			ok++
			all = append(all, r.items...)
		}
	}
	if ok == 0 {
		return nil, fmt.Errorf("all %d feeds failed", len(feeds))
	}
	// Rank: profile overlap first (more matched keywords wins), newer
	// breaks ties; undated items sort last within their tier.
	keySet := map[string]bool{}
	for _, k := range profile {
		keySet[strings.ToLower(k)] = true
	}
	score := func(it rssItem) int {
		low := strings.ToLower(it.Title)
		n := 0
		for k := range keySet {
			if k != "" && strings.Contains(low, k) {
				n++
			}
		}
		return n
	}
	sort.SliceStable(all, func(i, j int) bool {
		si, sj := score(all[i]), score(all[j])
		if si != sj {
			return si > sj
		}
		return all[i].Pub.After(all[j].Pub)
	})
	// Dedupe near-identical titles (syndicated copies across feeds).
	seen := map[string]bool{}
	var uniq []rssItem
	for _, it := range all {
		key := strings.ToLower(strings.TrimSpace(it.Title))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		uniq = append(uniq, it)
	}
	if len(uniq) > 8 {
		uniq = uniq[:8]
	}
	return &briefData{Profile: profile, Items: uniq, FeedsOK: ok}, nil
}

// dashProfile reads the memory personal layer and distills it into
// profile keywords plus one industry bucket (drives the tip and keyword
// rotations). Local file reads only — never touches the network.
func dashProfile(workspace string) (keywords []string, industry string) {
	defer func() { _ = recover() }() // a malformed fact must not kill the refresh
	st := memory.Open(workspace)
	if st == nil {
		return nil, "generic"
	}
	facts := st.List(memory.ScopePersonal)
	seen := map[string]bool{}
	add := func(s string) {
		for _, tok := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' ||
				r == '+' || r == '#' || r == '.' || r == '-' || r == '_' || r > 127)
		}) {
			if len(tok) < 2 || len(tok) > 20 || seen[tok] {
				continue
			}
			seen[tok] = true
			keywords = append(keywords, tok)
		}
	}
	for _, f := range facts {
		for _, t := range f.Tags {
			add(t)
		}
		add(f.Name)
		if len(keywords) >= 12 {
			break
		}
	}
	if len(keywords) > 6 {
		keywords = keywords[:6]
	}
	return keywords, classifyIndustry(strings.Join(keywords, " "))
}

// classifyIndustry buckets a profile into the rotation families the tip
// and keyword banks know; unmatched profiles land on generic.
func classifyIndustry(s string) string {
	s = strings.ToLower(s)
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(s, w) {
				return true
			}
		}
		return false
	}
	switch {
	case has("ai", "llm", "model", "ml ", " agent", "claude", "gpt", "rag"):
		return "ai"
	case has("stock", "trade", "market", "finance", "fund", "bond", "crypto"):
		return "finance"
	case has("design", "ux", "ui", "figma", "brand"):
		return "design"
	case has("go", "golang", "rust", "web", "api", "code", "dev", "tui", "test", "git"):
		return "dev"
	}
	return "generic"
}

// dashTips is the fixed offline knowledge bank: bilingual engineering
// tips per industry, rotated by day so the row moves even with no
// network and no memory layer at all. This is the fallback the brief
// block leans on when every feed fails.
var dashTips = map[string][]struct{ en, zh string }{
	"ai": {
		{en: "measure retrieval recall before tuning the prompt", zh: "先量检索召回率，再调提示词"},
		{en: "log the full tool-call trace — most agent failures hide there", zh: "记录完整工具调用轨迹，多数 agent 故障藏在那里"},
		{en: "pin model versions; silent upgrades change eval scores", zh: "锁定模型版本，静默升级会让评测分数失真"},
		{en: "cap context per turn — long is not the same as good", zh: "限制每轮上下文长度，长不等于好"},
		{en: "a cached answer needs a staleness budget too", zh: "缓存答案也需要过期预算"},
	},
	"finance": {
		{en: "backtest the fee model, not just the signal", zh: "回测要连手续费模型一起算，不只是信号"},
		{en: "position sizing beats entry timing over a full cycle", zh: "整轮周期看，仓位管理胜过入场时机"},
		{en: "reconcile against a second data vendor monthly", zh: "每月用第二数据源对账一次"},
		{en: "every drawdown story starts with an unhedged assumption", zh: "每次回撤故事都始于一个未对冲的假设"},
		{en: "alerts fire on rules, never on vibes", zh: "告警按规则触发，绝不凭感觉"},
	},
	"design": {
		{en: "contrast ratio first — style cannot rescue unreadable text", zh: "先保证对比度，样式救不了读不清的字"},
		{en: "one primary action per view; the rest earns its click", zh: "每屏一个主动作，其余操作要自己挣点击"},
		{en: "design the empty state before the happy path", zh: "先设计空状态，再设计顺利路径"},
		{en: "measure the tap target, trust the ruler over the mock", zh: "点按区域要用尺量，别信视觉稿"},
		{en: "motion explains causality; it never decorates latency", zh: "动效解释因果，不为延迟涂脂抹粉"},
	},
	"dev": {
		{en: "the test that never flaked never proved anything", zh: "从不抖动的测试什么也没证明"},
		{en: "write the rollback before the deploy", zh: "上线之前先写回滚"},
		{en: "a race log beats a race explanation every time", zh: "竞态日志永远胜过竞态口述"},
		{en: "benchmarks without p99 are averages with a secret", zh: "没有 p99 的基准测试是藏了秘密的均值"},
		{en: "delete the workaround when the fix lands", zh: "修复落地时顺手删掉临时绕行"},
	},
	"generic": {
		{en: "write the runbook while the incident is still warm", zh: "趁事故还热乎，先把处置手册写下来"},
		{en: "automate the task you did three times this week", zh: "本周做了三次的事，就该自动化"},
		{en: "an unmeasured improvement is a wish", zh: "没量过的改进只是愿望"},
		{en: "name things by what they are, not where they live", zh: "命名按本质，不按存放位置"},
		{en: "small batches make every failure cheap", zh: "小批量让每次失败都很便宜"},
	},
}

// dashKeywords is the per-industry daily keyword rotation: same day →
// same word everywhere, next day → the next one. Keyless, offline, and
// stable enough to quote in a report.
var dashKeywords = map[string][]struct{ en, zh string }{
	"ai": {
		{en: "tool loops", zh: "工具循环"},
		{en: "context caching", zh: "上下文缓存"},
		{en: "eval harness", zh: "评测框架"},
		{en: "retrieval recall", zh: "检索召回"},
		{en: "agent memory", zh: "智能体记忆"},
		{en: "prompt budgets", zh: "提示预算"},
		{en: "streaming tokens", zh: "流式令牌"},
		{en: "guardrails", zh: "安全护栏"},
		{en: "fine-tune drift", zh: "微调漂移"},
		{en: "multi-agent", zh: "多智能体"},
	},
	"finance": {
		{en: "drawdown", zh: "回撤"},
		{en: "liquidity", zh: "流动性"},
		{en: "factor tilt", zh: "因子倾斜"},
		{en: "volatility band", zh: "波动区间"},
		{en: "rebalance", zh: "再平衡"},
		{en: "carry cost", zh: "持仓成本"},
		{en: "hedge ratio", zh: "对冲比率"},
		{en: "slippage", zh: "滑点"},
		{en: "open interest", zh: "持仓量"},
		{en: "risk parity", zh: "风险平价"},
	},
	"design": {
		{en: "empty states", zh: "空状态"},
		{en: "contrast ratio", zh: "对比度"},
		{en: "design tokens", zh: "设计令牌"},
		{en: "motion budget", zh: "动效预算"},
		{en: "tap targets", zh: "点按区域"},
		{en: "type scale", zh: "字阶"},
		{en: "progressive disclosure", zh: "渐进披露"},
		{en: "focus ring", zh: "聚焦环"},
		{en: "dark surface", zh: "深色表面"},
		{en: "grid gutter", zh: "栅格槽"},
	},
	"dev": {
		{en: "race detector", zh: "竞态检测"},
		{en: "p99 latency", zh: "p99 延迟"},
		{en: "feature flag", zh: "特性开关"},
		{en: "blue-green", zh: "蓝绿部署"},
		{en: "idempotency", zh: "幂等性"},
		{en: "schema migration", zh: "结构迁移"},
		{en: "trace sampling", zh: "链路采样"},
		{en: "cold start", zh: "冷启动"},
		{en: "backpressure", zh: "背压"},
		{en: "hermetic test", zh: "封闭测试"},
	},
	"generic": {
		{en: "deep work", zh: "深度工作"},
		{en: "small batches", zh: "小批量"},
		{en: "written decisions", zh: "书面决策"},
		{en: "weekly review", zh: "每周复盘"},
		{en: "single source", zh: "单一事实源"},
		{en: "timeboxing", zh: "时间盒"},
		{en: "checklists", zh: "检查清单"},
		{en: "deferred commit", zh: "延迟提交"},
		{en: "fresh eyes", zh: "新鲜视角"},
		{en: "energy budget", zh: "精力预算"},
	},
}

// dayIndex is the shared daily rotation counter: one integer that moves
// exactly once per calendar day, never negative.
func dayIndex(t time.Time, mod int) int {
	if mod <= 0 {
		return 0
	}
	d := int(t.Unix() / 86400)
	return ((d % mod) + mod) % mod
}

// dashPickTip returns today's tip for an industry from the local bank.
func dashPickTip(industry string, t time.Time) (en, zh string) {
	bank := dashTips[industry]
	if len(bank) == 0 {
		bank = dashTips["generic"]
	}
	p := bank[dayIndex(t, len(bank))]
	return p.en, p.zh
}

// dashPickKeyword returns today's keyword for an industry.
func dashPickKeyword(industry string, t time.Time) (en, zh string) {
	bank := dashKeywords[industry]
	if len(bank) == 0 {
		bank = dashKeywords["generic"]
	}
	p := bank[dayIndex(t, len(bank))]
	return p.en, p.zh
}

// ---------------------------------------------------------------------------
// Group 4: ruanyifeng digest, leetcode daily, stocks, oscilloscope
// ---------------------------------------------------------------------------

// digestData is one fetch of 阮一峰的网络日志 atom.xml: the newest entry
// only — a digest row is a pointer, not a reader.
type digestData struct {
	Title string    `json:"title"`
	Date  string    `json:"date"`
	Link  string    `json:"link"`
	Pub   time.Time `json:"pub"`
}

// fetchDigest loads the configured digest feed (default ruanyifeng's
// atom.xml — Cloudflare-fronted, hence the UA in dashGet).
func fetchDigest(ctx context.Context, feedURL string) (any, error) {
	body, err := dashGet(ctx, feedURL, nil)
	if err != nil {
		return nil, err
	}
	items, err := parseFeed(body)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("digest feed has no entries")
	}
	newest := items[0]
	for _, it := range items {
		if it.Pub.After(newest.Pub) {
			newest = it
		}
	}
	d := &digestData{Title: newest.Title, Link: newest.Link, Pub: newest.Pub}
	if !newest.Pub.IsZero() {
		d.Date = newest.Pub.Format("2006-01-02")
	}
	return d, nil
}

// leetcodeData is the daily problem as shown by the row.
type leetcodeData struct {
	Date       string `json:"date"`
	FrontendID string `json:"fid"`
	Title      string `json:"title"`
	Translated string `json:"zh"`
	Difficulty string `json:"difficulty"`
	Slug       string `json:"slug"`
}

// leetcodeQuery is questionOfTodayV2 — extracted from leetcode.cn's own
// frontend bundle: the documented API is introspection-blocked, and the
// legacy query names stopped working. POST with the Referer the
// endpoint expects.
const leetcodeQuery = `query questionOfTodayV2 { todayRecord { date question { titleSlug title translatedTitle questionFrontendId difficulty } } }`

func fetchLeetCode(ctx context.Context) (any, error) {
	payload, err := json.Marshal(map[string]string{
		"operationName": "questionOfTodayV2",
		"variables":     "{}",
		"query":         leetcodeQuery,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dashEndpoint("leetcode"), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", dashUA)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Referer", "https://leetcode.cn/problemset/")
	resp, err := dashHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("leetcode graphql: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var r struct {
		Data struct {
			TodayRecord []struct {
				Date     string `json:"date"`
				Question struct {
					TitleSlug       string `json:"titleSlug"`
					Title           string `json:"title"`
					TranslatedTitle string `json:"translatedTitle"`
					FrontendID      string `json:"questionFrontendId"`
					Difficulty      string `json:"difficulty"`
				} `json:"question"`
			} `json:"todayRecord"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	if len(r.Data.TodayRecord) == 0 || r.Data.TodayRecord[0].Question.FrontendID == "" {
		return nil, fmt.Errorf("leetcode graphql: empty todayRecord")
	}
	rec := r.Data.TodayRecord[0]
	q := rec.Question
	return &leetcodeData{
		Date: rec.Date, FrontendID: q.FrontendID, Title: q.Title,
		Translated: q.TranslatedTitle, Difficulty: q.Difficulty, Slug: q.TitleSlug,
	}, nil
}

// stockQuote is one eastmoney quote after fltt=2 normalisation.
type stockQuote struct {
	SecID string  `json:"secid"`
	Name  string  `json:"name"`
	Price float64 `json:"price"`
	Pct   float64 `json:"pct"` // percent, signed
}

// stocksData keeps the config order so the row reads like the list the
// user wrote.
type stocksData struct {
	Quotes []stockQuote `json:"quotes"`
}

// fetchStocks fans one request per secid concurrently (maximal
// concurrency is a hard product requirement — serial quote polling is a
// defect), merges in the requested order, and succeeds when at least
// one quote answered.
func fetchStocks(ctx context.Context, secids []string) (any, error) {
	type res struct {
		q   stockQuote
		err error
	}
	results := make([]res, len(secids))
	var wg sync.WaitGroup
	for i, secid := range secids {
		wg.Add(1)
		go func(i int, secid string) {
			defer wg.Done()
			q, err := fetchOneStock(ctx, secid)
			results[i] = res{q: q, err: err}
		}(i, secid)
	}
	wg.Wait()
	var quotes []stockQuote
	for _, r := range results {
		if r.err == nil {
			quotes = append(quotes, r.q)
		}
	}
	if len(quotes) == 0 {
		return nil, fmt.Errorf("all %d quotes failed", len(secids))
	}
	return &stocksData{Quotes: quotes}, nil
}

func fetchOneStock(ctx context.Context, secid string) (stockQuote, error) {
	q := url.Values{}
	q.Set("secid", secid)
	q.Set("fltt", "2")
	q.Set("invt", "2")
	q.Set("fields", "f43,f57,f58,f60,f170")
	body, err := dashGet(ctx, dashEndpoint("stock")+"?"+q.Encode(), nil)
	if err != nil {
		return stockQuote{}, err
	}
	var r struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return stockQuote{}, err
	}
	if len(r.Data) == 0 {
		return stockQuote{}, fmt.Errorf("empty quote for %s", secid)
	}
	num := func(key string) float64 {
		raw, ok := r.Data[key]
		if !ok {
			return 0
		}
		var f float64
		if err := json.Unmarshal(raw, &f); err == nil {
			return f
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			var out float64
			fmt.Sscanf(s, "%g", &out)
			return out
		}
		return 0
	}
	str := func(key string) string {
		raw, ok := r.Data[key]
		if !ok {
			return ""
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
		return ""
	}
	quote := stockQuote{
		SecID: secid,
		Name:  str("f58"),
		Price: num("f43"),
		Pct:   num("f170"),
	}
	if quote.Price == 0 && quote.Name == "" {
		return stockQuote{}, fmt.Errorf("no quote data for %s", secid)
	}
	// Some responses omit f170; derive it from last vs previous close.
	if quote.Pct == 0 {
		if prev := num("f60"); prev > 0 && quote.Price > 0 {
			quote.Pct = (quote.Price - prev) / prev * 100
		}
	}
	return quote, nil
}

// dashWave renders the decorative oscilloscope: a deterministic-from-time
// composite sine with a fixed jitter, sampled into block glyphs. Purely
// local — synthetic data is explicitly allowed for this module.
func dashWave(width int, t time.Time) string {
	if width < 4 {
		return ""
	}
	glyphs := []byte("▁▂▃▄▅▆▇█")
	sec := float64(t.UnixNano()) / float64(time.Second)
	out := make([]byte, width)
	for x := 0; x < width; x++ {
		fx := float64(x)
		// Two detuned sines + a per-day phase shuffle: reads as a live
		// signal without claiming to be one.
		v := math.Sin(fx*0.35+sec*2.1)*0.55 +
			math.Sin(fx*0.11-sec*1.3)*0.35 +
			math.Sin(fx*0.83+sec*0.7)*0.10
		v += math.Sin(fx*12.9898+float64(t.Unix()/86400)*78.233) * 0.12
		idx := int((v + 1) / 2 * float64(len(glyphs)-1))
		if idx < 0 {
			idx = 0
		}
		if idx >= len(glyphs) {
			idx = len(glyphs) - 1
		}
		out[x] = glyphs[idx]
	}
	return string(out)
}
