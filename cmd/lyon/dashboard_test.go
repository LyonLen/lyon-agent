package main

// Dashboard tests (#10). Rules of the house: every network fetch runs
// against httptest stubs (the real endpoints never get touched from the
// suite), language is pinned to English for deterministic labels, and
// each test points $LYON_HOME at its own temp dir so config/cache state
// cannot leak between cases.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charmbracelet/x/ansi"
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

// dashModel builds a fresh model in an isolated home with its dashboard
// block in place. The guard keeps it single-seed both before and after
// the resetTranscript wiring lands.
func dashModel(t *testing.T) *tuiModel {
	t.Helper()
	dashTestHome(t)
	m := newTestModel(t)
	if _, ok := dashGetBlock(m); !ok {
		m.dashSeed()
	}
	t.Cleanup(func() { dashDelBlock(m) })
	return m
}

func dashBlockRows(m *tuiModel) []string {
	b, ok := dashGetBlock(m)
	if !ok || b.start < 0 || b.start+b.n > len(m.lines) {
		return nil
	}
	rows := make([]string, 0, b.n)
	for _, l := range m.lines[b.start : b.start+b.n] {
		rows = append(rows, l.raw)
	}
	return rows
}

// TestDashDefaultRender: the four core groups render at startup, every
// row fits the transcript width, and the row count never depends on
// width (narrow windows cut columns, never rows).
func TestDashDefaultRender(t *testing.T) {
	m := dashModel(t)
	rows := dashBlockRows(m)
	if len(rows) == 0 {
		t.Fatal("no dashboard block after seed")
	}
	joined := strings.Join(rows, "\n")
	for _, want := range []string{
		"lyon home",
		"machine",                     // group 1
		"time", "location", "weather", // group 2
		"brief",           // group 3
		"digest", "daily", // group 4 defaults: ruanyifeng + leetcode
		"commands: /stats", // the banner above the block survives
	} {
		if !strings.Contains(joined, want) && !strings.Contains(
			strings.Join(allTranscript(m), "\n"), want) {
			t.Errorf("dashboard misses %q", want)
		}
	}
	fullN := len(rows)
	for _, r := range rows {
		if w := ansi.StringWidth(r); w > m.tw {
			t.Errorf("row wider than transcript (%d>%d): %q", w, m.tw, r)
		}
	}
	// The invariant vocabulary other suites pin must never appear here.
	low := strings.ToLower(joined)
	for _, banned := range []string{"restored", "second line", "usage table",
		"replay plan beta", "budget reached"} {
		if strings.Contains(low, banned) {
			t.Errorf("dashboard text contains banned phrase %q", banned)
		}
	}
	// Group separators: blanks both between groups and before the tail.
	empties := 0
	for _, r := range rows {
		if r == "" {
			empties++
		}
	}
	if empties < 3 {
		t.Errorf("want >=3 separator/blank rows, got %d", empties)
	}

	// Narrow window: same row count, narrower rows.
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	m.dashRenderBlock()
	narrow := dashBlockRows(m)
	if len(narrow) != fullN {
		t.Fatalf("row count changed with width: %d -> %d", fullN, len(narrow))
	}
	for _, r := range narrow {
		if w := ansi.StringWidth(r); w > m.tw {
			t.Errorf("narrow row wider than transcript (%d>%d): %q", w, m.tw, r)
		}
	}
}

// allTranscript returns every transcript raw row (banner + block).
func allTranscript(m *tuiModel) []string {
	out := make([]string, 0, len(m.lines))
	for _, l := range m.lines {
		out = append(out, l.raw)
	}
	return out
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

// TestDashRenderOrder: core groups keep their fixed order; the custom
// group follows the config order and membership.
func TestDashRenderOrder(t *testing.T) {
	dashTestHome(t)
	cfg := defaultDashConfig()
	var ids []string
	for _, mod := range dashRenderOrder(&cfg) {
		ids = append(ids, mod.id)
	}
	want := "sysinfo clock geo weather brief ruanyifeng leetcode keywords stocks scope"
	if got := strings.Join(ids, " "); got != want {
		t.Errorf("default order:\n got %q\nwant %q", got, want)
	}
	cfg.Custom = []string{"stocks", "ruanyifeng"}
	ids = nil
	for _, mod := range dashRenderOrder(&cfg) {
		ids = append(ids, mod.id)
	}
	want = "sysinfo clock geo weather brief stocks ruanyifeng"
	if got := strings.Join(ids, " "); got != want {
		t.Errorf("custom order:\n got %q\nwant %q", got, want)
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

	// All feeds down → error, which the refresher turns into a stale badge.
	err = func() error {
		_, e := fetchBrief(context.Background(), []string{dead.URL}, t.TempDir())
		return e
	}()
	if err == nil {
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

// TestDashOfflineRender: with every cell empty the page still renders —
// placeholders per module, the local tip always present, no panic.
func TestDashOfflineRender(t *testing.T) {
	m := dashModel(t)
	joined := strings.Join(dashBlockRows(m), "\n")
	for _, want := range []string{
		"waiting for data",     // digest / daily placeholders
		"locating…",            // geo placeholder
		"waiting for location", // weather waits on geo
		"news offline",         // brief degrades to the tip
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("offline render misses %q", want)
		}
	}
	// The tip row (3rd brief line) must carry content even with no cell.
	if !strings.Contains(joined, "profile") {
		t.Error("brief profile row missing")
	}
}

// TestDashStaleBadge: age beyond 2x the interval and a failed refresh
// both mark the last-success cell as stale in the render.
func TestDashStaleBadge(t *testing.T) {
	m := dashModel(t)
	dashPutCell("geo", &geoData{City: "OldTown"}, time.Now().Add(-3*time.Hour), false)
	dashPutCell("weather", &weatherData{Temp: 20, Code: 0}, time.Now(), false)
	dashMarkFail("weather")
	m.dashRenderBlock()
	joined := strings.Join(dashBlockRows(m), "\n")
	if !strings.Contains(joined, "OldTown") {
		t.Fatal("cached cell not rendered")
	}
	if !strings.Contains(joined, "stale") {
		t.Error("no stale badge for aged/failed cells")
	}
	// Fresh geo (just put) must render without the badge on its own row.
	dashPutCell("geo", &geoData{City: "Freshville"}, time.Now(), false)
	m.dashRenderBlock()
	for _, r := range dashBlockRows(m) {
		if strings.Contains(r, "Freshville") && strings.Contains(r, "stale") {
			t.Errorf("fresh cell wrongly badged: %q", r)
		}
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

// TestDashEnvOff: the kill switch — no block, banner-only lines, the
// command reports the switch instead of acting.
func TestDashEnvOff(t *testing.T) {
	dashTestHome(t)
	t.Setenv("LYON_DASHBOARD", "off")
	m := newTestModel(t)
	if _, ok := dashGetBlock(m); ok {
		t.Error("env-off model seeded a block")
	}
	if len(m.lines) != 2 {
		t.Errorf("env-off startup has %d lines, want 2 (banner only)", len(m.lines))
	}
	if cmd := dashTickCmd(); cmd != nil {
		t.Error("env-off tick must be nil")
	}
	m.dashCommand("")
	if !strings.Contains(m.cmdOutText(), "LYON_DASHBOARD=off") {
		t.Errorf("off-switch report missing: %q", m.cmdOutText())
	}
	m.dashHome()
	if len(m.lines) != 2 {
		t.Errorf("/home with env off changed lines: %d", len(m.lines))
	}
	// Local probe still works under the switch (pure syscall, no state).
	if s := sysProbe(t.TempDir()); s.CPUPct < -1 || (s.CPUPct > 100) {
		t.Errorf("cpu pct out of range: %v", s.CPUPct)
	}
}

// TestDashCommand: panel + per-module toggles persist to dashboard.json
// and re-render the live block.
func TestDashCommand(t *testing.T) {
	m := dashModel(t)

	m.dashCommand("on stocks")
	if !strings.Contains(m.cmdOutText(), "stocks") {
		t.Errorf("status missing: %q", m.cmdOutText())
	}
	cfg := readDashConfig(dashConfigPath())
	if !dashModEnabled(&cfg, "stocks") {
		t.Error("stocks toggle not persisted")
	}
	if !strings.Contains(strings.Join(dashBlockRows(m), "\n"), "stocks") {
		t.Error("block not re-rendered after toggle")
	}

	m.cmdOut = nil
	m.dashCommand("off weather")
	cfg = readDashConfig(dashConfigPath())
	if dashModEnabled(&cfg, "weather") {
		t.Error("weather off not persisted")
	}
	if strings.Contains(strings.Join(dashBlockRows(m), "\n"), "weather  ") {
		t.Error("weather row still rendered after off")
	}

	m.cmdOut = nil
	m.dashCommand("bogus")
	if !strings.Contains(m.cmdOutText(), "usage") {
		t.Errorf("usage line missing: %q", m.cmdOutText())
	}
	m.cmdOut = nil
	m.dashCommand("on bogus")
	if !strings.Contains(m.cmdOutText(), "unknown module") {
		t.Errorf("unknown module report missing: %q", m.cmdOutText())
	}

	// Bare /dash: back home + config panel.
	m.cmdOut = nil
	m.dashCommand("")
	if !strings.Contains(m.cmdOutText(), "dashboard.json") {
		t.Errorf("panel path missing: %q", m.cmdOutText())
	}
	b, ok := dashGetBlock(m)
	if !ok || b.start+b.n != len(m.lines) {
		t.Error("/dash did not leave the block at the tail")
	}
}

// TestDashHome: /home rebuilds banner+dashboard while the audit panel,
// todos and step selection ride through untouched.
func TestDashHome(t *testing.T) {
	m := dashModel(t)
	m.steps = append(m.steps, execStep{})
	m.stepSeq = 3
	m.stepSel = 1
	m.stepOpen = true
	m.todos = append(m.todos, todoItem{})
	m.lines = append(m.lines, tline{kind: lineStyled, raw: "chat before home"})

	m.dashHome()
	if len(m.steps) != 1 || m.stepSeq != 3 || m.stepSel != 1 || !m.stepOpen {
		t.Errorf("steps not preserved: seq=%d sel=%d", m.stepSeq, m.stepSel)
	}
	if len(m.todos) != 1 {
		t.Error("todos not preserved")
	}
	if strings.Contains(strings.Join(allTranscript(m), "\n"), "chat before home") {
		t.Error("old transcript survived /home")
	}
	b, ok := dashGetBlock(m)
	if !ok || b.start+b.n != len(m.lines) {
		t.Error("block not at tail after /home")
	}
	if c := strings.Count(strings.Join(allTranscript(m), "\n"), "commands: /stats"); c != 1 {
		t.Errorf("banner count after /home = %d", c)
	}
}

// TestDashTick: live rewrite while at the tail; freeze the moment the
// conversation grows; deletion after the view empties.
func TestDashTick(t *testing.T) {
	m := dashModel(t)
	dashPutCell("geo", &geoData{City: "TickCity"}, time.Now(), false)
	cmd := m.dashTick()
	if cmd == nil {
		t.Fatal("tick must re-arm while enabled")
	}
	if !strings.Contains(strings.Join(dashBlockRows(m), "\n"), "TickCity") {
		t.Error("tick did not rewrite the block")
	}

	// Conversation after the block → the seeded dashboard freezes.
	m.lines = append(m.lines, tline{kind: lineStyled, raw: "user said something"})
	dashPutCell("geo", &geoData{City: "FrozenCity"}, time.Now(), false)
	m.dashTick()
	joined := strings.Join(allTranscript(m), "\n")
	if !strings.Contains(joined, "user said something") {
		t.Fatal("tick ate a conversation line")
	}
	if strings.Contains(joined, "FrozenCity") {
		t.Error("frozen block rewrote after conversation grew")
	}

	// View emptied (the /clear semantics) → the block is reaped.
	m.lines = nil
	m.dashTick()
	if _, ok := dashGetBlock(m); ok {
		t.Error("block entry leaked after the view emptied")
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

// TestDashSysProbe: the local probe runs on this OS without panicking
// and only reports values inside their domains.
func TestDashSysProbe(t *testing.T) {
	dashTestHome(t)
	s := sysProbe(t.TempDir())
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
	// Render path consumes the value type the registry stores.
	rows := renderSysinfo(mustModelForProbe(t),
		&dashCell{data: sysSnap{CPUPct: -1, MemPct: -1, OS: "x"}}, true)
	if len(rows) != 2 {
		t.Errorf("sysinfo render rows = %d, want 2", len(rows))
	}
}

// mustModelForProbe reuses dashModel but skips the block assertions —
// renderSysinfo only needs width fields, which the seed already fixed.
func mustModelForProbe(t *testing.T) *tuiModel { return dashModel(t) }
