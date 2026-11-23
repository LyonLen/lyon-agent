package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWebFetchStripsHTMLToText(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt := `<html><head><title>t</title><style>body{}</style></head>
<body><h1>Release Notes</h1><p>Go 1.25 adds container-aware GOMAXPROCS.</p>
<script>stealCookies()</script><a href="/x">link text</a></body></html>`
		_, _ = w.Write([]byte(fmt))
	}))
	defer ts.Close()

	w := &WebFetch{Client: ts.Client()}
	in, _ := json.Marshal(webFetchInput{URL: ts.URL})
	out, err := w.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	text := out[0].OfText.Text
	for _, want := range []string{"Release Notes", "container-aware GOMAXPROCS", "link text"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %q", want, text)
		}
	}
	for _, bad := range []string{"stealCookies", "<script", "<h1>", "body{}"} {
		if strings.Contains(text, bad) {
			t.Errorf("leaked %q into text: %q", bad, text)
		}
	}
}

func TestWebFetchRejectsBadSchemes(t *testing.T) {
	w := &WebFetch{}
	for _, u := range []string{"javascript:alert(1)", "ftp://x/y", "file:///etc/passwd", ""} {
		in, _ := json.Marshal(webFetchInput{URL: u})
		if _, err := w.Execute(context.Background(), in); err == nil {
			t.Errorf("scheme not rejected: %q", u)
		}
	}
}

func TestWebFetchPlainTextAndErrorStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.Error(w, "nope", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("plain body here"))
	}))
	defer ts.Close()

	w := &WebFetch{Client: ts.Client()}
	in, _ := json.Marshal(webFetchInput{URL: ts.URL})
	out, err := w.Execute(context.Background(), in)
	if err != nil || out[0].OfText.Text != "plain body here" {
		t.Fatalf("plain fetch: %v %v", err, out)
	}

	in, _ = json.Marshal(webFetchInput{URL: ts.URL + "/missing"})
	if _, err := w.Execute(context.Background(), in); err == nil {
		t.Fatal("404 should error")
	}
}

// A 429 with a short Retry-After is waited out once inside the call and
// the retry succeeds — the tool never surfaces the blip to the model.
func TestWebFetch429RetriesOnce(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("recovered"))
	}))
	defer ts.Close()

	w := &WebFetch{Client: ts.Client()}
	in, _ := json.Marshal(webFetchInput{URL: ts.URL})
	out, err := w.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if out[0].OfText.Text != "recovered" {
		t.Fatalf("text = %q", out[0].OfText.Text)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("server hits = %d, want 2 (one retry)", n)
	}
}

// A retry that 429s again fences the host: later fetches fail in-process
// with a route-around hint and never reach the server.
func TestWebFetch429FencesHost(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer ts.Close()

	w := &WebFetch{Client: ts.Client()}
	in, _ := json.Marshal(webFetchInput{URL: ts.URL})
	if _, err := w.Execute(context.Background(), in); err == nil {
		t.Fatal("want error after retry also 429s")
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("server hits = %d, want 2", n)
	}

	_, err := w.Execute(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "cooldown") {
		t.Fatalf("fenced call err = %v, want cooldown hint", err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("fenced call reached the server (hits = %d)", n)
	}
}

// A Retry-After beyond the wait cap fences immediately — the tool must
// not block the turn for the server's chosen pause.
func TestWebFetchLongRetryAfterFencesFast(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer ts.Close()

	w := &WebFetch{Client: ts.Client()}
	in, _ := json.Marshal(webFetchInput{URL: ts.URL})
	start := time.Now()
	_, err := w.Execute(context.Background(), in)
	if err == nil {
		t.Fatal("want error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("waited %s for a 120s Retry-After", elapsed)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("server hits = %d, want 1 (no in-call retry)", n)
	}
	if !strings.Contains(err.Error(), "different source") {
		t.Fatalf("err lacks route-around hint: %v", err)
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		in    string
		want  time.Duration
		valid bool
	}{
		{"120", 120 * time.Second, true},
		{" 7 ", 7 * time.Second, true},
		{"0", 0, true},
		{"-3", 0, true}, // clamp: a negative value means "now", not "never"
		{"", 0, false},
		{"soonish", 0, false},
		// A past HTTP-date means "retry now".
		{"Wed, 21 Oct 2020 07:28:00 GMT", 0, true},
	}
	for _, c := range cases {
		got, ok := parseRetryAfter(c.in)
		if ok != c.valid || (c.valid && got != c.want) {
			t.Errorf("parseRetryAfter(%q) = (%v, %v), want (%v, %v)", c.in, got, ok, c.want, c.valid)
		}
	}
	// A future HTTP-date is accepted and positive.
	if got, ok := parseRetryAfter(time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)); !ok || got <= 0 {
		t.Errorf("future HTTP-date: (%v, %v)", got, ok)
	}
}

func TestParseBingResults(t *testing.T) {
	page := `<html><body>
<div class="b_algo"><h2><a target="_blank" href="https://go.dev/blog/go1.25">Go 1.25 <b>Release</b> Notes</a></h2><p>snippet</p></div>
<div class="b_algo"><h2><a href="https://example.com/other">Other Result</a></h2></div>
<div class="b_algo"><h2><a href="#anchor">skip me</a></h2></div>
<div class="b_algo"><h2><a href="https://go.dev/blog/go1.25">duplicate url</a></h2></div>
</body></html>`
	got := parseBingResults(page)
	if len(got) != 2 {
		t.Fatalf("got %d results: %+v", len(got), got)
	}
	if got[0].URL != "https://go.dev/blog/go1.25" || got[0].Title != "Go 1.25 Release Notes" {
		t.Fatalf("first result: %+v", got[0])
	}
	if got[0].Snippet != "snippet" {
		t.Fatalf("first result snippet = %q, want %q", got[0].Snippet, "snippet")
	}
	if got[1].Title != "Other Result" {
		t.Fatalf("second result: %+v", got[1])
	}
	if got[1].Snippet != "" {
		t.Fatalf("second result snippet = %q, want empty", got[1].Snippet)
	}
}

func TestWebSearchParsesEndpoint(t *testing.T) {
	// Serve a Bing-shaped page from a local server so the parse path runs
	// end to end without the network.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "q=go+1.25") {
			t.Errorf("query not encoded into URL: %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<div class="b_algo"><h2><a href="https://go.dev/doc/go1.25">Go 1.25 Release Notes</a></h2><p>The official release notes for Go 1.25.</p></div>`))
	}))
	defer ts.Close()

	// Point the tool at the local server by swapping the endpoint host via
	// a custom client that rewrites the request URL.
	s := &WebSearch{Client: &http.Client{Transport: rewriteTo(ts.URL)}}
	in, _ := json.Marshal(webSearchInput{Query: "go 1.25"})
	out, err := s.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	text := out[0].OfText.Text
	if !strings.Contains(text, "1. Go 1.25 Release Notes") || !strings.Contains(text, "https://go.dev/doc/go1.25") {
		t.Fatalf("unexpected search output: %q", text)
	}
	// 出处之外还要有原文: the result's own snippet rides the output.
	if !strings.Contains(text, "The official release notes for Go 1.25.") {
		t.Fatalf("snippet missing from search output: %q", text)
	}
}

func TestParseBingRSS(t *testing.T) {
	page := `<?xml version="1.0"?>
<rss><channel>
<item>
  <title>Go 1.25 &lt;b&gt;Release&lt;/b&gt; Notes</title>
  <link>https://go.dev/blog/go1.25</link>
  <description><![CDATA[<p>The official release notes.</p>]]></description>
</item>
<item>
  <title>duplicate</title>
  <link>https://go.dev/blog/go1.25</link>
  <description>ignored</description>
</item>
<item>
  <title>anchor skip</title>
  <link>#section</link>
  <description>nope</description>
</item>
<item>
  <title>Second &amp; Final</title>
  <link>https://example.com/x?a=1&amp;b=2</link>
  <description>plain snippet</description>
</item>
</channel></rss>`
	got := parseBingRSS(page)
	if len(got) != 2 {
		t.Fatalf("got %d results: %+v", len(got), got)
	}
	if got[0].Title != "Go 1.25 Release Notes" || got[0].URL != "https://go.dev/blog/go1.25" {
		t.Fatalf("first: %+v", got[0])
	}
	if got[0].Snippet != "The official release notes." {
		t.Fatalf("snippet = %q", got[0].Snippet)
	}
	if got[1].Title != "Second & Final" || got[1].URL != "https://example.com/x?a=1&b=2" {
		t.Fatalf("entity decode: %+v", got[1])
	}
	// A non-XML body (Bing ignored format=rss) must fall through, not panic.
	if r := parseBingRSS(`<html><body>nope</body></html>`); r != nil {
		t.Fatalf("html body should yield nil, got %+v", r)
	}
}

func TestSearchRSSURLOrLanguage(t *testing.T) {
	cn := searchRSSURL("国务院 2026 放假")
	if !strings.Contains(cn, "www.bing.com") || !strings.Contains(cn, "format=rss") {
		t.Fatalf("CJK query endpoint: %s", cn)
	}
	if strings.Contains(cn, "ensearch") {
		t.Fatalf("CJK query must keep the CN index: %s", cn)
	}
	en := searchRSSURL("golang context cancel pattern")
	if !strings.Contains(en, "cn.bing.com") || !strings.Contains(en, "ensearch=1") || !strings.Contains(en, "format=rss") {
		t.Fatalf("latin query endpoint: %s", en)
	}
	if !hasCJK("千询办公") || hasCJK("go 1.25") || hasCJK("") {
		t.Fatal("hasCJK split wrong")
	}
}

// The RSS path runs end to end against a local feed: numbered output,
// snippet included — the model-facing format stays byte-identical to the
// HTML path's.
func TestWebSearchRSSParsesEndpoint(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "q=go+1.25") {
			t.Errorf("query not encoded into URL: %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(`<?xml version="1.0"?>
<rss><channel>
<item><title>Go 1.25 Release Notes</title><link>https://go.dev/doc/go1.25</link>
<description>The official release notes for Go 1.25.</description></item>
</channel></rss>`))
	}))
	defer ts.Close()

	s := &WebSearch{Client: &http.Client{Transport: rewriteTo(ts.URL)}}
	in, _ := json.Marshal(webSearchInput{Query: "go 1.25"})
	out, err := s.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	text := out[0].OfText.Text
	for _, want := range []string{"1. Go 1.25 Release Notes", "https://go.dev/doc/go1.25", "The official release notes for Go 1.25."} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %q", want, text)
		}
	}
}

func TestWebSearchEmptyQuery(t *testing.T) {
	s := &WebSearch{}
	in, _ := json.Marshal(webSearchInput{Query: "  "})
	if _, err := s.Execute(context.Background(), in); err == nil {
		t.Fatal("empty query should error")
	}
}

func TestParseBaiduResults(t *testing.T) {
	page := `<html><body>
<div class="result c-container"><h3 class="t"><a href="/link?url=abc123" target="_blank">生物信息学<em>分析</em>工具软件采购公告</a></h3>
<span class="content-right_8Zs40">采购方式:竞争性磋商 预算金额:45万元</span></div>
<div class="result c-container"><h3><a href="https://example.com/direct">直接链接结果</a></h3>
<div class="c-abstract">摘要文本在这里。</div></div>
<div class="result c-container"><h3><a href="#anchor">跳过我</a></h3></div>
<div class="result c-container"><h3><a href="/link?url=abc123">重复链接</a></h3></div>
</body></html>`
	got := parseBaiduResults(page)
	if len(got) != 2 {
		t.Fatalf("got %d results: %+v", len(got), got)
	}
	if !strings.HasPrefix(got[0].URL, "https://www.baidu.com/link?url=abc123") {
		t.Fatalf("relative link not absolutized: %+v", got[0])
	}
	if got[0].Title != "生物信息学分析工具软件采购公告" {
		t.Fatalf("title (em stripped) = %q", got[0].Title)
	}
	if !strings.Contains(got[0].Snippet, "竞争性磋商") {
		t.Fatalf("snippet = %q", got[0].Snippet)
	}
	if got[1].URL != "https://example.com/direct" || got[1].Snippet != "摘要文本在这里。" {
		t.Fatalf("second: %+v", got[1])
	}
}

func TestParseBaiduBlocked(t *testing.T) {
	// The seccaptcha stub Baidu serves when requests burst — must read
	// as blocked (error path), not as an empty-but-ok parse.
	stub := `<a href="https://bioc-static.cdn.bcebos.com/pages/ds_svcp.html?biocOrigin=https://seccaptcha-gs.baidu.com">Found</a>.`
	if !baiduBlocked(stub) {
		t.Fatal("seccaptcha stub not detected")
	}
	if baiduBlocked(`<html><title>x_百度搜索</title><h3><a href="/link?url=1">r</a></h3></html>`) {
		t.Fatal("normal SERP flagged as blocked")
	}
}

// The gate is the promise that unrelated results never reach the model:
// every observed Bing decoy mode must fail it, real results pass.
func TestResultsRelevant(t *testing.T) {
	tender := "生物信息学软件 政府采购 招标公告 单一来源 license"
	cases := []struct {
		name    string
		results []SearchResult
		query   string
		want    bool
	}{
		{
			"tender query vs decoy news",
			[]SearchResult{{Title: "盘点2026年9月去世的10位明星", Snippet: "每一位都让人十分惋惜"}},
			tender, false,
		},
		{
			"tender query vs dictionary fallback",
			[]SearchResult{
				{Title: "生物（拥有生命功能的生命体）_百度百科", Snippet: "生物学是研究生命现象的学科"},
				{Title: "高中生物知识点总结 - 知乎", Snippet: "高中生物知识点"},
			},
			tender, false,
		},
		{
			"obituary query vs first-char dictionary",
			[]SearchResult{
				{Title: "洪（汉语文字）_百度百科", Snippet: "洪是一个汉字"},
				{Title: "洪 hóng - 汉典", Snippet: "洪的读音意思"},
			},
			"洪健华去世", false,
		},
		{
			"tender query vs real Baidu hits",
			[]SearchResult{
				{Title: "生物信息学分析工具软件采购项目竞争性磋商公告", Snippet: "采购方式:竞争性磋商"},
				{Title: "内蒙古大学招标采购中心", Snippet: "生物信息学服务器采购更正公告"},
			},
			tender, true,
		},
		{
			"short query passes on one unit",
			[]SearchResult{{Title: "北京天气预报,7天天气预报", Snippet: "北京天气查询"}},
			"北京天气", true,
		},
		{
			"latin query",
			[]SearchResult{{Title: "Go 1.25 Release Notes", Snippet: "The official release notes for Go 1.25."}},
			"go 1.25", true,
		},
	}
	for _, c := range cases {
		if got := resultsRelevant(c.results, c.query); got != c.want {
			t.Errorf("%s: resultsRelevant = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSearchOrder(t *testing.T) {
	cjk := searchOrder("国务院放假通知")
	if cjk[0].name != "baidu" || cjk[1].name != "bing" {
		t.Fatalf("CJK order = %s,%s, want baidu,bing", cjk[0].name, cjk[1].name)
	}
	lat := searchOrder("golang context cancel pattern")
	if lat[0].name != "bing" || lat[1].name != "baidu" {
		t.Fatalf("latin order = %s,%s, want bing,baidu", lat[0].name, lat[1].name)
	}
}

// CJK queries take the Baidu leg end to end: a Baidu-shaped SERP from
// the local server must come back as the numbered model-facing list.
func TestWebSearchBaiduEndpoint(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "wd=") {
			t.Errorf("query not in wd param: %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><body><div class="result c-container">
<h3><a href="/link?url=tender1">国务院办公厅关于2026年放假安排的通知</a></h3>
<span class="content-right_x">放假安排：春节、国庆各休息。</span>
</div></body></html>`))
	}))
	defer ts.Close()

	s := &WebSearch{Client: &http.Client{Transport: rewriteTo(ts.URL)}}
	in, _ := json.Marshal(webSearchInput{Query: "国务院放假通知"})
	out, err := s.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	text := out[0].OfText.Text
	for _, want := range []string{"1. 国务院办公厅关于2026年放假安排的通知", "https://www.baidu.com/link?url=tender1", "放假安排"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %q", want, text)
		}
	}
}

// Opt-in live probe: runs the real engine ladder against the real
// endpoints. LYON_LIVE_SEARCH=1 go test ./pkg/tools -run TestLiveSearch -v
// LYON_SEARCH_QUERY overrides the probe query.
func TestLiveSearch(t *testing.T) {
	if os.Getenv("LYON_LIVE_SEARCH") == "" {
		t.Skip("set LYON_LIVE_SEARCH=1 to hit the real search endpoints")
	}
	q := os.Getenv("LYON_SEARCH_QUERY")
	if q == "" {
		// The report that started the ladder: a tender query that came
		// back with air-fryer recipes / obituaries from Bing.
		q = "生物信息学软件 政府采购 招标公告 单一来源 license"
	}
	s := &WebSearch{}
	in, _ := json.Marshal(webSearchInput{Query: q})
	out, err := s.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("search %q: %v", q, err)
	}
	t.Logf("query=%q\n%s", q, out[0].OfText.Text)
}

// rewriteTo redirects every request to the test server, preserving the
// original query string.
func rewriteTo(base string) http.RoundTripper {
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		u := *req.URL
		target := strings.TrimRight(base, "/") + "/?" + u.RawQuery
		req2, err := http.NewRequestWithContext(req.Context(), req.Method, target, nil)
		if err != nil {
			return nil, err
		}
		req2.Header = req.Header.Clone()
		return http.DefaultTransport.RoundTrip(req2)
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
