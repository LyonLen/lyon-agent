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
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// webBodyLimit caps how much of a fetched page we will read.
const webBodyLimit = 512 * 1024

// webUserAgent avoids default-curl rejection on search endpoints.
const webUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36"

// WebSearch performs a keyless web search by scraping Bing's HTML endpoint.
// It is the client-side twin of Anthropic's server-side web_search, used when
// the API endpoint (e.g. a gateway) rewrites server tools into client tool
// calls. Parallel-safe: read-only outbound requests.
type WebSearch struct {
	Client *http.Client
}

func (w *WebSearch) Name() string { return "web_search" }

func (w *WebSearch) Description() string {
	return "Search the web. Returns the top results as numbered title + URL pairs. Use this before web_fetch to find pages to read."
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
	}
	return TextResult(b.String()), nil
}

// SearchResult is one organic result from the scraped page.
type SearchResult struct {
	Title string
	URL   string
}

var (
	// Each organic result anchors on an <h2><a href="..."> link inside a
	// b_algo block; capturing the anchor covers title and destination.
	reBingResult = regexp.MustCompile(`(?s)<h2><a[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	reTag        = regexp.MustCompile(`<[^>]+>`)
)

// parseBingResults extracts title/URL pairs from Bing HTML, de-duplicated,
// capped at 8.
func parseBingResults(page string) []SearchResult {
	var out []SearchResult
	seen := make(map[string]bool)
	for _, m := range reBingResult.FindAllStringSubmatch(page, -1) {
		u := strings.TrimSpace(html.UnescapeString(m[1]))
		title := strings.TrimSpace(reTag.ReplaceAllString(m[2], ""))
		title = strings.Join(strings.Fields(title), " ")
		if u == "" || title == "" || seen[u] {
			continue
		}
		// Skip in-page anchors and bing's own chrome.
		if strings.HasPrefix(u, "#") || strings.HasPrefix(u, "/") {
			continue
		}
		seen[u] = true
		out = append(out, SearchResult{Title: title, URL: u})
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

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", webUserAgent)
	req.Header.Set("Accept", "text/html,text/plain;q=0.9,*/*;q=0.5")

	client := w.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
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
