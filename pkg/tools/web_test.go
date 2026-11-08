package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func TestWebSearchEmptyQuery(t *testing.T) {
	s := &WebSearch{}
	in, _ := json.Marshal(webSearchInput{Query: "  "})
	if _, err := s.Execute(context.Background(), in); err == nil {
		t.Fatal("empty query should error")
	}
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
