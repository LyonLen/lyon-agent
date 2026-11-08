package tools

import (
	"context"
	"encoding/json"
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

	"github.com/anthropics/anthropic-sdk-go"
)

// webBodyLimit caps how much of a fetched page we will read.
const webBodyLimit = 512 * 1024

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

// WebSearch performs a keyless web search by scraping Bing's HTML endpoint.
// It is the client-side twin of Anthropic's server-side web_search, used when
// the API endpoint (e.g. a gateway) rewrites server tools into client tool
// calls. Parallel-safe: read-only outbound requests.
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

func (w *WebSearch) Execute(ctx context.Context, input json.RawMessage) ([]anthropic.BetaToolResultBlockParamContentUnion, error) {
	var in webSearchInput
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, fmt.Errorf("invalid input: %w", err)
	}
	if strings.TrimSpace(in.Query) == "" {
		return nil, fmt.Errorf("empty query")
	}

	endpoint := "https://www.bing.com/search?q=" + url.QueryEscape(in.Query) + "&setlang=en&count=10"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", webUserAgent)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	client := w.Client
	if client == nil {
		client = http.DefaultClient
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

	results := parseBingResults(string(body))
	if len(results) == 0 {
		return nil, fmt.Errorf("no parseable results (endpoint layout may have changed)")
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
