package main

// Dashboard data-layer tests (#10; the chat-area display is retired —
// header widgets own presentation). Rules of the house: every network
// fetch runs against httptest stubs (the suite never touches the real
// network), and each test points $LYON_HOME at its own temp dir so
// config/cache state cannot leak between cases.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// dashTestHome isolates config + cache under a fresh $LYON_HOME and
// forces the dashboard on; home-keyed memos reload transparently, and
// the explicit invalidation covers same-home raw-file writes.
func dashTestHome(t *testing.T) string {
	t.Helper()
	home := tempDir(t)
	t.Setenv("LYON_HOME", home)
	t.Setenv("LYON_DASHBOARD", "")
	dashInvalidateConfig()
	dashCellsReset()
	setLang("en")
	return home
}

// dashStub points one endpoint at a test server for this case only.
func dashStub(t *testing.T, field, url string) {
	t.Helper()
	old := dashEndpoint(field)
	dashSetEndpoint(field, url)
	t.Cleanup(func() { dashSetEndpoint(field, old) })
}

// dashModel builds a fresh model in an isolated home.
func dashModel(t *testing.T) *tuiModel {
	t.Helper()
	dashTestHome(t)
	return newTestModel(t)
}

func allTranscript(m *tuiModel) []string {
	out := make([]string, 0, len(m.lines))
	for _, l := range m.lines {
		out = append(out, l.raw)
	}
	return out
}

// TestDashNoTranscriptBlock: the startup transcript carries the banner
// only — no dashboard block ever seeds, neither at model construction
// nor through the resetTranscript path (/new, /resume).
func TestDashNoTranscriptBlock(t *testing.T) {
	m := dashModel(t)
	if len(m.lines) != 2 {
		t.Fatalf("startup transcript has %d lines, want 2 (banner only)", len(m.lines))
	}
	joined := strings.Join(allTranscript(m), "\n")
	if !strings.Contains(joined, "commands: /stats") {
		t.Error("banner missing from startup transcript")
	}
	if strings.Contains(joined, "lyon home") {
		t.Error("dashboard block seeded into the transcript")
	}
	// A conversation grows, then the /new //resume reset runs: still no
	// dashboard block in either state.
	m.lines = append(m.lines, tline{kind: lineStyled, raw: "some chat"})
	m.resetTranscript()
	if got := strings.Join(allTranscript(m), "\n"); strings.Contains(got, "lyon home") {
		t.Error("resetTranscript re-seeded a dashboard block")
	}
	if len(m.lines) != 2 {
		t.Errorf("reset transcript has %d lines, want 2", len(m.lines))
	}
}

// TestDashIntervalDefaults: sysinfo refreshes every 10s by default —
// both the built-in registry interval and the default config string —
// while an explicit user interval still wins over the default.
func TestDashIntervalDefaults(t *testing.T) {
	dashTestHome(t)
	mod := dashModLookup("sysinfo")
	if mod == nil || mod.interval != 10*time.Second {
		t.Fatalf("sysinfo registry interval = %v, want 10s", mod.interval)
	}
	cfg := defaultDashConfig()
	if got := cfg.Intervals["sysinfo"]; got != "10s" {
		t.Errorf("default config interval = %q, want 10s", got)
	}
	if got := dashIntervalOf(&cfg, mod); got != 10*time.Second {
		t.Errorf("dashIntervalOf = %v, want 10s", got)
	}
	// A stored dashboard.json with an explicit value keeps its meaning.
	cfg.Intervals["sysinfo"] = "5s"
	if got := dashIntervalOf(&cfg, mod); got != 5*time.Second {
		t.Errorf("explicit interval ignored: got %v", got)
	}
	cfg.Intervals["sysinfo"] = "nonsense"
	if got := dashIntervalOf(&cfg, mod); got != 10*time.Second {
		t.Errorf("garbage interval must fall back to 10s, got %v", got)
	}
}

// TestDashConfig: partial files merge onto defaults, unknown module ids
// roundtrip, broken JSON degrades to defaults, enabled=false is honored.
func TestDashConfig(t *testing.T) {
	home := dashTestHome(t)
	path := dashConfigPath()
	if !strings.HasPrefix(path, home) {
		t.Fatalf("config path %q not under home %q", path, home)
	}
	partial := `{"modules":{"stocks":true,"weather":false,"moon":true},` +
		`"custom":["stocks","ruanyifeng"],"intervals":{"geo":"10m"},"enabled":false}`
	if err := os.WriteFile(path, []byte(partial), 0o644); err != nil {
		t.Fatal(err)
	}
	dashInvalidateConfig()
	cfg := dashConfigNow()
	if dashGlobalOn(&cfg) {
		t.Error("enabled:false not honored")
	}
	if !dashModEnabled(&cfg, "stocks") || dashModEnabled(&cfg, "weather") {
		t.Errorf("module overrides wrong: stocks=%v weather=%v",
			dashModEnabled(&cfg, "stocks"), dashModEnabled(&cfg, "weather"))
	}
	if !dashModEnabled(&cfg, "sysinfo") {
		t.Error("default module lost by partial file")
	}
	if _, ok := cfg.Modules["moon"]; !ok {
		t.Error("unknown module id dropped")
	}
	if len(cfg.Custom) != 2 || cfg.Custom[0] != "stocks" {
		t.Errorf("custom list wrong: %v", cfg.Custom)
	}
	mod := dashModLookup("geo")
	if d := dashIntervalOf(&cfg, mod); d != 10*time.Minute {
		t.Errorf("interval override: got %v", d)
	}
	cfg.Intervals["geo"] = "nonsense"
	if d := dashIntervalOf(&cfg, mod); d != time.Hour {
		t.Errorf("bad interval must fall back to default, got %v", d)
	}

	// Roundtrip keeps the unknown id and every override.
	cfg.Enabled = nil
	if err := writeDashConfig(cfg); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"moon"`) {
		t.Error("unknown module id lost in roundtrip")
	}
	dashInvalidateConfig()
	again := readDashConfig(path)
	if !dashModEnabled(&again, "moon") || dashGlobalOn(&again) == false {
		t.Error("roundtrip readback mismatch")
	}

	// Broken JSON: pure defaults, never an error state.
	if err := os.WriteFile(path, []byte("{nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	dashInvalidateConfig()
	fresh := readDashConfig(path)
	if !dashGlobalOn(&fresh) || !dashModEnabled(&fresh, "sysinfo") {
		t.Error("garbage file must yield defaults")
	}
}

// TestDashFetchGeo: primary and fallback paths against local stubs.
func TestDashFetchGeo(t *testing.T) {
	dashTestHome(t)
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"success":true,"ip":"203.0.113.9","city":"Shanghai",`+
			`"region":"Shanghai","country":"China","latitude":31.23,"longitude":121.47}`)
	}))
	defer okSrv.Close()
	dashStub(t, "geo", okSrv.URL)
	got, err := fetchGeo(context.Background())
	if err != nil {
		t.Fatalf("primary geo: %v", err)
	}
	g := got.(*geoData)
	if g.City != "Shanghai" || g.Lat != 31.23 || g.IP != "203.0.113.9" {
		t.Errorf("geo payload wrong: %+v", g)
	}

	// Primary refuses, fallback answers — location still resolves.
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"success":false}`)
	}))
	defer badSrv.Close()
	fbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "fields=") {
			t.Errorf("fallback query missing fields: %s", r.URL.RawQuery)
		}
		io.WriteString(w, `{"status":"success","query":"8.8.8.8","city":"Mountain View",`+
			`"regionName":"California","country":"US","lat":37.4,"lon":-122.1}`)
	}))
	defer fbSrv.Close()
	dashStub(t, "geo", badSrv.URL)
	dashStub(t, "geoFallback", fbSrv.URL)
	got, err = fetchGeo(context.Background())
	if err != nil {
		t.Fatalf("fallback geo: %v", err)
	}
	g = got.(*geoData)
	if g.City != "Mountain View" || g.Lon != -122.1 {
		t.Errorf("fallback payload wrong: %+v", g)
	}
}

// TestDashFetchWeather: open-meteo shape against a stub.
func TestDashFetchWeather(t *testing.T) {
	dashTestHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "forecast_days") {
			t.Errorf("weather query incomplete: %s", r.URL.RawQuery)
		}
		io.WriteString(w, `{"current":{"temperature_2m":21.7,"weather_code":0,`+
			`"wind_speed_10m":12.5},"daily":{"temperature_2m_max":[26.1],`+
			`"temperature_2m_min":[17.9]}}`)
	}))
	defer srv.Close()
	dashStub(t, "weather", srv.URL)
	got, err := fetchWeather(context.Background(), 31.2, 121.5)
	if err != nil {
		t.Fatal(err)
	}
	w := got.(*weatherData)
	if w.Temp != 21.7 || w.Code != 0 || w.Hi != 26.1 || w.Lo != 17.9 || w.Lat != 31.2 {
		t.Errorf("weather payload wrong: %+v", w)
	}
	en, zh := weatherDesc(w.Code)
	if en == "" || zh == "" {
		t.Errorf("weatherDesc empty for code 0: %q/%q", en, zh)
	}
}

// TestDashFetchBrief: concurrent feed fan-out, one dead host degrades to
// the survivor, titles come back entity-unescaped.
func TestDashFetchBrief(t *testing.T) {
	dashTestHome(t)
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `<?xml version="1.0"?><rss version="2.0"><channel><title>t</title>`+
			`<item><title>A &amp; B ship today</title><link>http://x/1</link>`+
			`<pubDate>Mon, 13 Oct 2026 08:00:00 +0000</pubDate></item>`+
			`<item><title>Older story</title><link>http://x/2</link>`+
			`<pubDate>Sun, 12 Oct 2026 08:00:00 +0000</pubDate></item></channel></rss>`)
	}))
	defer good.Close()
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer dead.Close()
	got, err := fetchBrief(context.Background(), []string{good.URL, dead.URL}, t.TempDir())
	if err != nil {
		t.Fatalf("one-of-two feeds must succeed: %v", err)
	}
	b := got.(*briefData)
	if b.FeedsOK != 1 {
		t.Errorf("FeedsOK = %d, want 1", b.FeedsOK)
	}
	if len(b.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(b.Items))
	}
	if b.Items[0].Title != "A & B ship today" {
		t.Errorf("entity/title wrong: %q", b.Items[0].Title)
	}
	if !b.Items[0].Pub.After(b.Items[1].Pub) {
		t.Error("items not sorted by recency")
	}

	// All feeds down → error, which the refresher turns into a stale cell.
	if _, e := fetchBrief(context.Background(), []string{dead.URL}, t.TempDir()); e == nil {
		t.Error("all-feeds-dead must report an error for the stale path")
	}
}

// TestDashFetchDigest: atom.xml shape via direct URL (the registry passes
// the real ruanyifeng URL; here the stub is the URL).
func TestDashFetchDigest(t *testing.T) {
	dashTestHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ua := r.Header.Get("User-Agent"); !strings.Contains(ua, "Mozilla") {
			t.Errorf("digest fetch missing browser UA: %q", ua)
		}
		io.WriteString(w, `<?xml version="1.0" encoding="utf-8"?>`+
			`<feed xmlns="http://www.w3.org/2005/Atom"><title>Blog</title>`+
			`<entry><title>Week 2026-41</title><link href="https://example.com/w41"/>`+
			`<published>2026-10-13T10:00:00+08:00</published></entry>`+
			`<entry><title>Week 2026-40</title><link href="https://example.com/w40"/>`+
			`<published>2026-10-06T10:00:00+08:00</published></entry></feed>`)
	}))
	defer srv.Close()
	got, err := fetchDigest(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	d := got.(*digestData)
	if d.Title != "Week 2026-41" || d.Date != "2026-10-13" {
		t.Errorf("digest wrong: %+v", d)
	}
}

// TestDashFetchLeetCode: GraphQL POST shape against a stub.
func TestDashFetchLeetCode(t *testing.T) {
	dashTestHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("leetcode must POST, got %s", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "questionOfTodayV2") {
			t.Errorf("payload missing operation: %.80s", body)
		}
		io.WriteString(w, `{"data":{"todayRecord":[{"date":"2026-10-15",`+
			`"question":{"titleSlug":"two-sum","title":"Two Sum",`+
			`"translatedTitle":"两数之和","questionFrontendId":"1",`+
			`"difficulty":"Easy"}}]}}`)
	}))
	defer srv.Close()
	dashStub(t, "leetcode", srv.URL)
	got, err := fetchLeetCode(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	d := got.(*leetcodeData)
	if d.FrontendID != "1" || d.Translated != "两数之和" || d.Difficulty != "Easy" {
		t.Errorf("leetcode wrong: %+v", d)
	}
}

// TestDashFetchStocks: one stub answering both secids — order preserved,
// and a missing f170 derives the percent from the previous close.
func TestDashFetchStocks(t *testing.T) {
	dashTestHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("secid") {
		case "1.000001":
			io.WriteString(w, `{"data":{"f43":105.5,"f57":"600519","f58":"GZMT",`+
				`"f60":100.0,"f170":5.5}}`)
		case "0.399001":
			io.WriteString(w, `{"data":{"f43":2010.0,"f57":"399001","f58":"SZCZ",`+
				`"f60":2000.0}}`)
		default:
			io.WriteString(w, `{"data":{}}`)
		}
	}))
	defer srv.Close()
	dashStub(t, "stock", srv.URL)
	got, err := fetchStocks(context.Background(), []string{"1.000001", "0.399001"})
	if err != nil {
		t.Fatal(err)
	}
	s := got.(*stocksData)
	if len(s.Quotes) != 2 {
		t.Fatalf("quotes = %d, want 2", len(s.Quotes))
	}
	if s.Quotes[0].Name != "GZMT" || s.Quotes[0].Pct != 5.5 {
		t.Errorf("quote[0] wrong: %+v", s.Quotes[0])
	}
	if s.Quotes[1].Name != "SZCZ" {
		t.Errorf("quote order not preserved: %+v", s.Quotes[1])
	}
	if d := s.Quotes[1].Pct; d < 0.49 || d > 0.51 {
		t.Errorf("derived pct = %v, want ~0.5", d)
	}
}

// TestDashCellStale: the failure badge lives on the cell itself — a
// failed refresh keeps the last success and flips stale.
func TestDashCellStale(t *testing.T) {
	dashTestHome(t)
	dashPutCell("geo", &geoData{City: "Fresh"}, time.Now(), false)
	c, ok := dashGetCell("geo")
	if !ok || c.stale || c.data.(*geoData).City != "Fresh" {
		t.Fatalf("fresh cell wrong: ok=%v cell=%+v", ok, c)
	}
	dashMarkFail("geo")
	c, _ = dashGetCell("geo")
	if !c.stale || c.data.(*geoData).City != "Fresh" {
		t.Errorf("markFail must keep data and flag stale: %+v", c)
	}
}

// TestDashCache: persistable cells roundtrip through dashboard-cache.json
// and an aged entry comes back pre-marked stale.
func TestDashCache(t *testing.T) {
	dashTestHome(t)
	dashPutCell("geo", &geoData{City: "Saved"}, time.Now(), false)
	dashPutCell("sysinfo", sysSnap{CPUPct: 7}, time.Now(), false) // no decode → not persisted
	dashSaveCache()
	if _, err := os.Stat(dashCachePath()); err != nil {
		t.Fatalf("cache file missing: %v", err)
	}
	dashCellsReset()
	if _, ok := dashGetCell("geo"); ok {
		t.Fatal("cells not cleared by reset")
	}
	dashLoadCacheNow()
	c, ok := dashGetCell("geo")
	if !ok {
		t.Fatal("geo lost in cache roundtrip")
	}
	if g := c.data.(*geoData); g.City != "Saved" {
		t.Errorf("cached city = %q", g.City)
	}
	if c.stale {
		t.Error("fresh cached cell marked stale")
	}
	if _, ok := dashGetCell("sysinfo"); ok {
		t.Error("non-persistable sysinfo leaked into cache")
	}

	// Hand-written aged cache: loads with data, stale from the start.
	dashCellsReset()
	file := dashCacheFile{Saved: time.Now(), Cells: map[string]dashCacheEnt{}}
	raw, _ := json.Marshal(&geoData{City: "OldTown"})
	file.Cells["geo"] = dashCacheEnt{At: time.Now().Add(-48 * time.Hour), Data: raw}
	data, _ := json.Marshal(file)
	if err := os.WriteFile(dashCachePath(), data, 0o644); err != nil {
		t.Fatal(err)
	}
	dashLoadCacheNow()
	c, ok = dashGetCell("geo")
	if !ok || !c.stale || c.data.(*geoData).City != "OldTown" {
		t.Fatalf("aged cache load wrong: ok=%v cell=%+v", ok, c)
	}
}

// TestDashEnvOff: the kill switch — no tick chain, no dashboard in the
// transcript.
func TestDashEnvOff(t *testing.T) {
	dashTestHome(t)
	t.Setenv("LYON_DASHBOARD", "off")
	if cmd := dashTickCmd(); cmd != nil {
		t.Error("env-off tick must be nil")
	}
	m := newTestModel(t)
	if strings.Contains(strings.Join(allTranscript(m), "\n"), "lyon home") {
		t.Error("env-off startup contains a dashboard")
	}
}

// TestDashRefreshOnce: success writes the cell; a failed attempt keeps
// the last success and marks it stale; disabled modules never fetch.
func TestDashRefreshOnce(t *testing.T) {
	dashTestHome(t)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		io.WriteString(w, `{"success":true,"ip":"203.0.113.9","city":"Shanghai",`+
			`"region":"Shanghai","country":"China","latitude":31.2,"longitude":121.5}`)
	}))
	dashStub(t, "geo", srv.URL)
	// The fallback must be dead too — otherwise a closed primary escapes
	// to the real ip-api.com over the network.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	dashStub(t, "geoFallback", deadURL)

	ctx := context.Background()
	dashRefreshOnce(ctx, "geo")
	c, ok := dashGetCell("geo")
	if !ok || c.stale || c.data.(*geoData).City != "Shanghai" {
		t.Fatalf("refresh did not populate the cell: ok=%v cell=%+v", ok, c)
	}

	// Kill the endpoint → the old cell survives, badged stale.
	srv.Close()
	dashRefreshOnce(ctx, "geo")
	c, _ = dashGetCell("geo")
	if c.data.(*geoData).City != "Shanghai" || !c.stale {
		t.Errorf("failure must keep last success and mark stale: %+v", c)
	}

	// Disabled module: zero fetch attempts.
	before := atomic.LoadInt32(&hits)
	cfg := defaultDashConfig()
	cfg.Modules["geo"] = false
	if err := writeDashConfig(cfg); err != nil {
		t.Fatal(err)
	}
	dashRefreshOnce(ctx, "geo")
	if got := atomic.LoadInt32(&hits); got != before {
		t.Errorf("disabled module fetched (%d -> %d)", before, got)
	}
}

// TestDashSysProbe: the local probe runs on this OS without panicking,
// values stay inside their domains, and a cold process warms its rate
// baselines so the very first probe already reports real CPU/net
// numbers (the root cause of the user's "cpu … net …" first paint).
func TestDashSysProbe(t *testing.T) {
	dashTestHome(t)
	sysResetBaseline()
	s := sysProbe(t.TempDir())
	if runtime.GOOS != "darwin" {
		if s.CPUPct < 0 {
			t.Errorf("first probe CPU still warmup (-1): baseline priming failed")
		}
		if !s.NetOK {
			t.Errorf("first probe net still warmup (false): baseline priming failed")
		}
	}
	if s.CPUPct < -1 || s.CPUPct > 100 {
		t.Errorf("CPUPct out of domain: %v", s.CPUPct)
	}
	if s.MemPct < -1 || s.MemPct > 100 {
		t.Errorf("MemPct out of domain: %v", s.MemPct)
	}
	if s.DiskTotal > 0 && s.DiskFree > s.DiskTotal {
		t.Errorf("disk free > total: %d > %d", s.DiskFree, s.DiskTotal)
	}
	if s.NetUpBps < 0 || s.NetDownBps < 0 {
		t.Errorf("negative net rate: up=%v down=%v", s.NetUpBps, s.NetDownBps)
	}
	// Second probe: no re-warm sleep path, still in domain.
	s2 := sysProbe(t.TempDir())
	if s2.CPUPct < -1 || s2.CPUPct > 100 {
		t.Errorf("second probe CPUPct out of domain: %v", s2.CPUPct)
	}
}
