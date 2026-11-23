package tools

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/anthropics/anthropic-sdk-go"
)

// webBodyLimit caps how much of a fetched page we will read.
const webBodyLimit = 512 * 1024

// baiduBodyLimit: Baidu's SERP embeds the organic results deep in a
// ~1MB page (forensics 2026-11-23: first b_algo h3 at byte ~810k) —
// the webBodyLimit would truncate before the first result.
const baiduBodyLimit = 2 << 20

// webUserAgent avoids default-curl rejection on search endpoints.
const webUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36"

// 429 politeness for web_fetch. A rate-limited host must not be hammered:
// one bounded retry inside the tool call when the server offers a short
// Retry-After, then a per-host fence that turns later fetches into an
// immediate "use a different source" error instead of another hit.
const (
	// web429RetryWait is the backoff when a 429 carries no usable
	// Retry-After — long enough to escape a token-bucket refill, short
	// enough to stay invisible in a tool turn.
	web429RetryWait = 2 * time.Second
	// web429MaxWait caps the in-call sleep: a tool call blocking minutes
	// helps nobody — past this, fence the host and let the model route
	// around it this turn.
	web429MaxWait = 10 * time.Second
	// web429Cooldown is the fence floor (a 429 without Retry-After) and
	// the ceiling's lower bound; web429MaxCooldown caps a huge
	// Retry-After from fencing a host for the rest of the session.
	web429Cooldown    = 30 * time.Second
	web429MaxCooldown = 2 * time.Minute
)

// WebSearch performs a keyless web search through an engine ladder with
// a relevance gate. It is the client-side twin of Anthropic's
// server-side web_search, used when the API endpoint (e.g. a gateway)
// rewrites server tools into client tool calls. Parallel-safe: read-only
// outbound requests.
//
// Why a ladder: Bing from CN networks serves decoy results for tail
// queries — a tender query came back with air-fryer recipes, a person's
// obituary query with character-dictionary pages (forensics 2026-11-23;
// same family as the 2026-11-16 ensearch findings on searchRSSURL).
// CJK queries therefore go Baidu first, Latin queries keep Bing, and the
// other engine is the second opinion. Every engine's output must pass
// resultsRelevant before it reaches the model — unrelated results are an
// error the model can reword against, never a silently wrong answer.
type WebSearch struct {
	Client *http.Client
}

func (w *WebSearch) Name() string { return "web_search" }

func (w *WebSearch) Description() string {
	return "Search the web. Returns the top results as numbered title, URL and snippet (source + original text excerpt). Use this before web_fetch to find pages to read."
}

func (w *WebSearch) ParallelSafe() bool { return true }

type webSearchInput struct {
	Query string `json:"query" jsonschema:"required,description=The search query"`
}

func (w *WebSearch) InputSchema() anthropic.BetaToolInputSchemaParam {
	return schemaFor[webSearchInput]()
}

// searchEngine is one entry of the ladder: a name for the error report
// and the fetch+parse for that endpoint.
type searchEngine struct {
	name  string
	fetch func(ctx context.Context, client *http.Client, query string) ([]SearchResult, error)
}

// searchOrder picks the engine ladder for a query. CJK goes Baidu first
// — from CN networks Bing's index serves decoy/dictionary results for
// tail queries (forensics 2026-11-23) — while Latin queries keep Bing
// (its international index reads English better) with Baidu as the
// second opinion. Every engine's output faces resultsRelevant.
func searchOrder(query string) []searchEngine {
	baidu := searchEngine{name: "baidu", fetch: fetchBaidu}
	bing := searchEngine{name: "bing", fetch: fetchBing}
	if hasCJK(query) {
		return []searchEngine{baidu, bing}
	}
	return []searchEngine{bing, baidu}
}

func (w *WebSearch) Execute(ctx context.Context, input json.RawMessage) ([]anthropic.BetaToolResultBlockParamContentUnion, error) {
	var in webSearchInput
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, fmt.Errorf("invalid input: %w", err)
	}
	if strings.TrimSpace(in.Query) == "" {
		return nil, fmt.Errorf("empty query")
	}

	client := w.Client
	if client == nil {
		client = http.DefaultClient
	}

	var why []string
	for _, eng := range searchOrder(in.Query) {
		results, err := eng.fetch(ctx, client, in.Query)
		if err != nil {
			why = append(why, fmt.Sprintf("%s: %v", eng.name, err))
			continue
		}
		if len(results) == 0 {
			why = append(why, eng.name+": no results")
			continue
		}
		if !resultsRelevant(results, in.Query) {
			// The observed Bing failure mode: parseable, coherent, and
			// about something else entirely. Never hand that to the model.
			why = append(why, eng.name+": results unrelated to the query")
			continue
		}
		var b strings.Builder
		for i, r := range results {
			fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, r.Title, r.URL)
			if r.Snippet != "" {
				fmt.Fprintf(&b, "   %s\n", r.Snippet)
			}
		}
		return TextResult(b.String()), nil
	}
	return nil, fmt.Errorf(
		"search found nothing relevant (%s) — reword or shorten the query",
		strings.Join(why, "; "),
	)
}

// fetchBing hits Bing's RSS endpoint (HTML scrape as the fallback for
// when the format flag is ignored) — the pre-ladder behavior, kept as
// the Latin primary and the CJK second opinion.
func fetchBing(ctx context.Context, client *http.Client, query string) ([]SearchResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchRSSURL(query), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", webUserAgent)
	// Match the index the URL picked: a CJK query on the CN index with
	// Accept-Language: en-US is exactly the mismatch that fed decoys.
	if hasCJK(query) {
		req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	} else {
		req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("search endpoint returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, webBodyLimit))
	if err != nil {
		return nil, fmt.Errorf("read search results: %w", err)
	}
	// RSS first; when Bing ignores format=rss the body is still HTML and
	// the layout scrape gets the last word before we declare failure.
	results := parseBingRSS(string(body))
	if len(results) == 0 {
		results = parseBingResults(string(body))
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("no parseable results (endpoint layout may have changed)")
	}
	return results, nil
}

// fetchBaidu hits Baidu's web SERP. The response is a ~1MB HTML page
// whose organic results sit deep inside (see baiduBodyLimit); a
// seccaptcha interstitial (the ds_svcp stub) means rate-limited — an
// error the ladder reports, not results to parse.
func fetchBaidu(ctx context.Context, client *http.Client, query string) ([]SearchResult, error) {
	u := "https://www.baidu.com/s?wd=" + url.QueryEscape(query) + "&rn=10&ie=utf-8"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", webUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	if hasCJK(query) {
		req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	} else {
		req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("search endpoint returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, baiduBodyLimit))
	if err != nil {
		return nil, fmt.Errorf("read search results: %w", err)
	}
	page := string(body)
	if baiduBlocked(page) {
		return nil, fmt.Errorf("verification page (rate-limited)")
	}
	return parseBaiduResults(page), nil
}

// SearchResult is one organic result from the scraped page. Snippet is
// the result's own description text — the "原文" a reader needs before
// deciding the source is worth fetching.
type SearchResult struct {
	Title   string
	URL     string
	Snippet string
}

var (
	// Each organic result anchors on an <h2><a href="..."> link inside a
	// b_algo block; capturing the anchor covers title and destination.
	reBingResult = regexp.MustCompile(`(?s)<h2><a[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	reTag        = regexp.MustCompile(`<[^>]+>`)
	reSnippet    = regexp.MustCompile(`(?s)<p[^>]*>(.*?)</p>`)
)

// parseBingResults extracts title/URL pairs from Bing HTML, de-duplicated,
// capped at 8.
func parseBingResults(page string) []SearchResult {
	var out []SearchResult
	seen := make(map[string]bool)
	locs := reBingResult.FindAllStringSubmatchIndex(page, -1)
	for i, m := range locs {
		u := strings.TrimSpace(html.UnescapeString(page[m[2]:m[3]]))
		title := strings.TrimSpace(reTag.ReplaceAllString(page[m[4]:m[5]], ""))
		title = strings.Join(strings.Fields(title), " ")
		if u == "" || title == "" || seen[u] {
			continue
		}
		// Skip in-page anchors and bing's own chrome.
		if strings.HasPrefix(u, "#") || strings.HasPrefix(u, "/") {
			continue
		}
		// Snippet = the first paragraph between this result's link and the
		// next one (or end of page). Match indices because RE2 has no
		// lookahead: the tail must not consume the next result's <h2>.
		tail := page[m[1]:]
		if i+1 < len(locs) {
			tail = page[m[1]:locs[i+1][0]]
		}
		snippet := ""
		if sm := reSnippet.FindStringSubmatch(tail); sm != nil {
			s := strings.TrimSpace(reTag.ReplaceAllString(sm[1], ""))
			s = strings.Join(strings.Fields(html.UnescapeString(s)), " ")
			// Chrome paragraphs after the last real result are not answers.
			if len(s) > 0 && len(s) <= 400 {
				snippet = s
			}
		}
		seen[u] = true
		out = append(out, SearchResult{Title: title, URL: u, Snippet: snippet})
		if len(out) >= 8 {
			break
		}
	}
	return out
}

// searchRSSURL picks the Bing RSS endpoint by query language. CJK
// queries read www.bing.com's China index (that's where 千问/国务院
// results live); everything else flips cn.bing.com onto its
// international index (ensearch=1), because setlang/en alone still
// returned 百度百科 and 放假通知 for English tech queries (dump
// forensics 2026-11-16 — the model retried a dozen rewrites and gave
// up on search entirely).
func searchRSSURL(query string) string {
	q := url.QueryEscape(query)
	if hasCJK(query) {
		return "https://www.bing.com/search?q=" + q + "&format=rss&count=10"
	}
	return "https://cn.bing.com/search?q=" + q + "&format=rss&ensearch=1&count=10"
}

// hasCJK reports whether s carries Han/Kana/Hangul — the market split
// for searchRSSURL. Loops runes; queries are short.
func hasCJK(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			return true
		}
	}
	return false
}

// parseBingRSS extracts organic results from Bing's RSS feed. Titles and
// descriptions may carry inline HTML (<b> highlights, <p> wraps) — tags
// stripped, entities unescaped, capped at 8: same SearchResult shape as
// parseBingResults so the renderer and the model see one format. A
// non-XML body (Bing ignored format=rss) unmarshal-fails → nil.
func parseBingRSS(page string) []SearchResult {
	var doc struct {
		Channel struct {
			Items []struct {
				Title       string `xml:"title"`
				Link        string `xml:"link"`
				Description string `xml:"description"`
			} `xml:"item"`
		} `xml:"channel"`
	}
	if err := xml.Unmarshal([]byte(page), &doc); err != nil {
		return nil
	}
	var out []SearchResult
	seen := make(map[string]bool)
	for _, it := range doc.Channel.Items {
		u := strings.TrimSpace(it.Link)
		title := cleanSearchText(it.Title)
		if u == "" || title == "" || seen[u] {
			continue
		}
		// Skip in-page anchors (same guard as the HTML path).
		if strings.HasPrefix(u, "#") || strings.HasPrefix(u, "/") {
			continue
		}
		snippet := cleanSearchText(it.Description)
		// RSS descriptions run long sometimes; clip instead of the HTML
		// path's discard — the model reads a cut snippet, not none.
		if len([]rune(snippet)) > 400 {
			snippet = string([]rune(snippet)[:400]) + "…"
		}
		seen[u] = true
		out = append(out, SearchResult{Title: title, URL: u, Snippet: snippet})
		if len(out) >= 8 {
			break
		}
	}
	return out
}

// cleanSearchText strips inline HTML, unescapes entities, and folds
// whitespace into the one line a result row carries.
func cleanSearchText(s string) string {
	s = reTag.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	return strings.Join(strings.Fields(s), " ")
}

// cleanBaiduText prepares raw Baidu markup for snippet duty: paired
// script/style and comment blocks go first (their contents are page
// config, not prose), then unescape→strip→unescape→strip — Baidu mixes
// raw tags with ESCAPED tags (&lt;table&gt;), and the second unescape
// would otherwise re-materialize the escaped ones as visible markup
// after the first strip. Tag-stripping leaves spaces, which would
// shatter CJK titles around a highlighted <em> — Han gaps are re-closed
// (two passes: one covers alternating gaps, the second the remainder).
func cleanBaiduText(s string) string {
	s = reScriptish.ReplaceAllString(s, " ")
	s = reComment.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = reTag.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = reTag.ReplaceAllString(s, " ")
	s = strings.Join(strings.Fields(s), " ")
	s = reHanGap.ReplaceAllString(s, "$1$2")
	return reHanGap.ReplaceAllString(s, "$1$2")
}

// cutDangling trims a window cut mid-construction: an unterminated
// comment/script/style/tag at the end carries Baidu's embedded config
// JSON (<!--s-data:{...}-->), which must never surface as snippet text.
func cutDangling(s string) string {
	if i := strings.LastIndex(s, "<!--"); i >= 0 && !strings.Contains(s[i:], "-->") {
		s = s[:i]
	}
	if i := strings.LastIndex(s, "<script"); i >= 0 && !strings.Contains(s[i:], "</script>") {
		s = s[:i]
	}
	if i := strings.LastIndex(s, "<style"); i >= 0 && !strings.Contains(s[i:], "</style>") {
		s = s[:i]
	}
	if i := strings.LastIndex(s, "<"); i >= 0 && !strings.Contains(s[i:], ">") {
		s = s[:i]
	}
	return s
}

// reHanGap closes single spaces between Han characters (see
// cleanBaiduText).
var reHanGap = regexp.MustCompile(`(\p{Han}) +(\p{Han})`)

// baiduBlocked spots Baidu's anti-bot interstitial: a tiny stub page
// linking to seccaptcha/ds_svcp instead of a SERP (observed when
// requests arrive in a burst).
func baiduBlocked(page string) bool {
	return strings.Contains(page, "seccaptcha") ||
		strings.Contains(page, "ds_svcp") ||
		strings.Contains(page, "百度安全验证")
}

// reBaiduResult anchors on the organic result shape: <h3> wrapping a
// single <a href>, both bare of required attributes (Baidu varies
// classes per template but keeps this skeleton). Stray tags between
// h3 and the anchor are tolerated.
var (
	reBaiduResult = regexp.MustCompile(`(?s)<h3[^>]*>(?:\s*<[^>]+>)*\s*<a[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	// Snippet containers, tried in order — the hashed class names
	// (content-right_8Zs40) drift, the stable fragments do not.
	reBaiduSnip = []*regexp.Regexp{
		regexp.MustCompile(`(?s)class="[^"]*content-right[^"]*"[^>]*>(.*?)</(?:span|div)>`),
		regexp.MustCompile(`(?s)class="[^"]*c-abstract[^"]*"[^>]*>(.*?)</div>`),
		regexp.MustCompile(`(?s)class="[^"]*c-line-clamp\d[^"]*"[^>]*>(.*?)</div>`),
	}
)

// parseBaiduResults extracts title/URL/snippet from a Baidu SERP,
// de-duplicated, capped at 8 — same SearchResult shape as the Bing
// paths so the renderer and the model see one format. Links are
// baidu.com/link?url= redirects; web_fetch follows them to the target.
func parseBaiduResults(page string) []SearchResult {
	var out []SearchResult
	seen := make(map[string]bool)
	for _, m := range reBaiduResult.FindAllStringSubmatch(page, -1) {
		u := strings.TrimSpace(html.UnescapeString(m[1]))
		if strings.HasPrefix(u, "/") {
			u = "https://www.baidu.com" + u
		}
		title := cleanBaiduText(m[2])
		if u == "" || title == "" || seen[u] {
			continue
		}
		if strings.HasPrefix(u, "#") {
			continue
		}
		// Snippet: known containers inside the window after this title,
		// else the window's own text — better a rough excerpt than none.
		window := cutDangling(baiduWindow(page, m[0]))
		snippet := ""
		for _, re := range reBaiduSnip {
			if sm := re.FindStringSubmatch(window); sm != nil {
				snippet = cleanBaiduText(sm[1])
				break
			}
		}
		if snippet == "" {
			snippet = cleanBaiduText(window)
		}
		if rs := []rune(snippet); len(rs) > 240 {
			snippet = string(rs[:240]) + "…"
		}
		if len([]rune(snippet)) < 8 {
			snippet = ""
		}
		seen[u] = true
		out = append(out, SearchResult{Title: title, URL: u, Snippet: snippet})
		if len(out) >= 8 {
			break
		}
	}
	return out
}

// baiduWindow returns up to ~3KB of markup after a result's match —
// the region that holds its snippet container.
func baiduWindow(page, match string) string {
	at := strings.Index(page, match)
	if at < 0 {
		return ""
	}
	start := at + len(match)
	if start > len(page) {
		return ""
	}
	end := start + 3000
	if end > len(page) {
		end = len(page)
	}
	return page[start:end]
}

// ── relevance gate ──────────────────────────────────────────────────

// queryUnits is the vocabulary a result set must touch: each
// whitespace token contributes Han bigrams (生物信息学 → 生物/物信/信息/
// 息学 — a single Han rune stays itself) and lowercased ASCII runs of
// ≥2 (gpt-5, license). Bigrams keep the gate language-agnostic without
// a segmenter; requiring only a MINORITY of units (see resultsRelevant)
// keeps a legitimate one-aspect result from being rejected.
func queryUnits(q string) []string {
	seen := make(map[string]bool)
	var out []string
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, tok := range strings.Fields(q) {
		var han strings.Builder
		flushHan := func() {
			rs := []rune(han.String())
			switch {
			case len(rs) == 0:
			case len(rs) == 1:
				add(string(rs))
			default:
				for i := 0; i+1 < len(rs); i++ {
					add(string(rs[i : i+2]))
				}
			}
			han.Reset()
		}
		var asc strings.Builder
		flushAsc := func() {
			if asc.Len() >= 2 {
				add(strings.ToLower(asc.String()))
			}
			asc.Reset()
		}
		for _, r := range tok {
			switch {
			case unicode.Is(unicode.Han, r):
				flushAsc()
				han.WriteRune(r)
			case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
				(r >= '0' && r <= '9') || r == '_' || r == '-':
				flushHan()
				asc.WriteRune(r)
			default:
				flushHan()
				flushAsc()
			}
		}
		flushHan()
		flushAsc()
	}
	return out
}

// resultsRelevant reports whether the result set visibly concerns the
// query: at least min(2, units) distinct units must appear across the
// titles+snippets. The gate exists for Bing's decoy mode — air-fryer
// recipes for a tender query, dictionary pages for an obituary — which
// parses cleanly and would otherwise reach the model as fact.
func resultsRelevant(results []SearchResult, query string) bool {
	units := queryUnits(query)
	if len(units) == 0 {
		return true
	}
	need := 2
	if len(units) < need {
		need = len(units)
	}
	var b strings.Builder
	for _, r := range results {
		b.WriteString(r.Title)
		b.WriteByte('\n')
		b.WriteString(r.Snippet)
		b.WriteByte('\n')
	}
	text := strings.ToLower(b.String())
	hits := 0
	for _, u := range units {
		if strings.Contains(text, strings.ToLower(u)) {
			hits++
		}
	}
	return hits >= need
}

// WebFetch downloads a URL and returns its text content. HTML is stripped
// to readable text; other text types pass through. Parallel-safe.
type WebFetch struct {
	Client *http.Client

	// mu guards cooldowns: ParallelSafe means concurrent Executes, and
	// the engine shares one WebFetch across every session and subagent
	// (nestedTools pulls from the registry), so a fence raised by one
	// fan-out task spares the others.
	mu        sync.Mutex
	cooldowns map[string]time.Time // host → fenced until
}

func (w *WebFetch) Name() string { return "web_fetch" }

func (w *WebFetch) Description() string {
	return "Fetch a URL over HTTP(S) and return its text content (HTML tags stripped). Use for reading pages found via web_search or known documentation URLs."
}

func (w *WebFetch) ParallelSafe() bool { return true }

type webFetchInput struct {
	URL string `json:"url" jsonschema:"required,description=The http(s) URL to fetch"`
}

func (w *WebFetch) InputSchema() anthropic.BetaToolInputSchemaParam {
	return schemaFor[webFetchInput]()
}

func (w *WebFetch) Execute(ctx context.Context, input json.RawMessage) ([]anthropic.BetaToolResultBlockParamContentUnion, error) {
	var in webFetchInput
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, fmt.Errorf("invalid input: %w", err)
	}
	parsed, err := url.Parse(strings.TrimSpace(in.URL))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("url must be http or https: %q", in.URL)
	}

	if until, ok := w.cooldownFor(parsed.Host); ok {
		return nil, fmt.Errorf("fetch %s: on cooldown ~%ds after HTTP 429 — use a different source (search, another domain, or a reader proxy) instead of retrying this host",
			parsed.Host, int(time.Until(until).Seconds())+1)
	}

	client := w.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := w.get(ctx, client, parsed.String())
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}

	// 429: one bounded in-call retry, then fence the host. A short
	// Retry-After is worth waiting out inside the turn; anything longer
	// than the cap (or a second 429) means the model should source
	// elsewhere now and the host stays fenced for later tasks too.
	if resp.StatusCode == http.StatusTooManyRequests {
		ra, have := parseRetryAfter(resp.Header.Get("Retry-After"))
		wait := ra
		if !have {
			wait = web429RetryWait
		}
		if wait > web429MaxWait {
			fenced := w.fenceHost(parsed.Host, ra)
			resp.Body.Close()
			return nil, fmt.Errorf("fetch %s: HTTP 429, Retry-After %ds exceeds the %ds wait cap — host fenced ~%ds; use a different source",
				parsed.Host, int(ra.Seconds()), int(web429MaxWait.Seconds()), int(fenced.Seconds()))
		}
		drainClose(resp)
		if err := sleepCtx(ctx, wait); err != nil {
			return nil, fmt.Errorf("fetch %s: 429 backoff interrupted: %w", parsed.Host, err)
		}
		if resp, err = w.get(ctx, client, parsed.String()); err != nil {
			return nil, fmt.Errorf("fetch: %w", err)
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			ra, _ = parseRetryAfter(resp.Header.Get("Retry-After"))
			fenced := w.fenceHost(parsed.Host, ra)
			resp.Body.Close()
			return nil, fmt.Errorf("fetch %s: HTTP 429 twice in a row — host fenced ~%ds; use a different source",
				parsed.Host, int(fenced.Seconds()))
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("fetch %s: HTTP %d", parsed.Host, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, webBodyLimit))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	ct := resp.Header.Get("Content-Type")
	text := ""
	switch {
	case strings.Contains(ct, "html") || looksLikeHTML(body):
		text = htmlToText(string(body))
	case strings.Contains(ct, "text/"), ct == "":
		text = string(body)
	default:
		return TextResult(fmt.Sprintf("content-type %s, %d bytes: not text, not shown", ct, len(body))), nil
	}
	if len(text) > 32*1024 {
		text = text[:32*1024] + "\n... [truncated at 32KB]"
	}
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("%s produced no extractable text", parsed.Host)
	}
	return TextResult(text), nil
}

// get issues one GET with the shared browser-ish headers.
func (w *WebFetch) get(ctx context.Context, client *http.Client, rawurl string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", webUserAgent)
	req.Header.Set("Accept", "text/html,text/plain;q=0.9,*/*;q=0.5")
	return client.Do(req)
}

// cooldownFor reports a live fence for host, if any.
func (w *WebFetch) cooldownFor(host string) (time.Time, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	until, ok := w.cooldowns[host]
	return until, ok && time.Now().Before(until)
}

// fenceHost blocks fetches to host for ra (clamped to the floor/ceiling),
// extending rather than shortening an existing fence, and returns the
// duration applied — for the model-facing error message.
func (w *WebFetch) fenceHost(host string, ra time.Duration) time.Duration {
	d := ra
	if d < web429Cooldown {
		d = web429Cooldown
	}
	if d > web429MaxCooldown {
		d = web429MaxCooldown
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cooldowns == nil {
		w.cooldowns = make(map[string]time.Time)
	}
	until := time.Now().Add(d)
	if cur, ok := w.cooldowns[host]; !ok || until.After(cur) {
		w.cooldowns[host] = until
	}
	return d
}

// sleepCtx waits out a 429 backoff, giving up when the tool call's
// context is cancelled (timeout or interrupt) instead of sleeping on.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// drainClose consumes a little of the body (for connection reuse) and
// closes it — used when a 429 response is discarded before the retry.
func drainClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
}

// parseRetryAfter reads a Retry-After header: delta-seconds per RFC 9110,
// or an HTTP-date. The bool reports whether the header was present and
// recognizable; callers supply their own default when it is not.
func parseRetryAfter(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			secs = 0
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}

func looksLikeHTML(b []byte) bool {
	head := strings.ToLower(string(b[:min(len(b), 512)]))
	return strings.Contains(head, "<html") || strings.Contains(head, "<!doctype html")
}

var (
	reScriptish = regexp.MustCompile(`(?is)<(script|style|noscript|svg|head)\b[^>]*>.*?</(script|style|noscript|svg|head)>`)
	reBreak     = regexp.MustCompile(`(?i)<(br|/p|/div|/li|/h[1-6]|/tr|/td|/th|/section|/article)[^>]*>`)
	reComment   = regexp.MustCompile(`(?s)<!--.*?-->`)
)

// htmlToText converts HTML to plain text with a handful of stdlib regexes:
// drop head/script/style, turn block ends into newlines, strip remaining
// tags, unescape entities, collapse whitespace. Not a spec-compliant
// parser — good enough for feeding pages to a model.
func htmlToText(page string) string {
	s := reComment.ReplaceAllString(page, "")
	s = reScriptish.ReplaceAllString(s, "")
	s = reBreak.ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	lines := strings.Split(s, "\n")
	var out []string
	for _, line := range lines {
		line = strings.Join(strings.Fields(line), " ")
		if line != "" {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}
