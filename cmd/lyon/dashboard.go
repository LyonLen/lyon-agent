package main

// The dashboard data layer (#10; display retired — the header widget
// cells own presentation now). Ten modules — machine stats, clock,
// IP location + weather, the personality-profiled news brief, and the
// custom group (ruanyifeng digest, leetcode daily, daily keyword,
// quotes, oscilloscope) — refresh into an in-memory cell map that the
// header reads. The architecture keeps three promises:
//
//   - The TUI event loop never fetches: background goroutines (one per
//     refreshable module, started only from runTUI) write cells on each
//     module's own interval; a 1s tea.Tick chain keeps the header clock
//     stepping.
//   - Offline is a first-class state: a failed fetch keeps the
//     last-success cell marked stale instead of dropping it.
//   - Every kill path is an env var: LYON_DASHBOARD=off starts no
//     refreshers and no tick. Config lives at $LYON_HOME/dashboard.json,
//     the last-success cache at $LYON_HOME/dashboard-cache.json.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// dashEnvOff is the hard kill switch, same idiom as LYON_EXAM/LYON_SCORE.
func dashEnvOff() bool { return strings.EqualFold(os.Getenv("LYON_DASHBOARD"), "off") }

// dashHomeDir mirrors the routing/market idiom: $LYON_HOME when set,
// else ~/.lyon.
func dashHomeDir() string {
	if h := os.Getenv("LYON_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".lyon"
	}
	return filepath.Join(home, ".lyon")
}

func dashConfigPath() string { return filepath.Join(dashHomeDir(), "dashboard.json") }
func dashCachePath() string  { return filepath.Join(dashHomeDir(), "dashboard-cache.json") }

// ---------------------------------------------------------------------------
// Configuration: $LYON_HOME/dashboard.json
// ---------------------------------------------------------------------------

// dashConfig is the whole file schema. Every field is optional on disk —
// missing pieces inherit the built-in defaults (all four core groups on;
// the custom group defaults to ruanyifeng + leetcode). Unknown module
// ids survive a roundtrip untouched and are simply never rendered.
type dashConfig struct {
	Enabled   *bool             `json:"enabled,omitempty"`
	Modules   map[string]bool   `json:"modules,omitempty"`
	Custom    []string          `json:"custom,omitempty"`
	Intervals map[string]string `json:"intervals,omitempty"`
	Feeds     []string          `json:"feeds,omitempty"`
	Stocks    []string          `json:"stocks,omitempty"`
}

func defaultDashFeeds() []string {
	return []string{
		// Verified keyless and CN-reachable; ithome's canonical feed is
		// www.ithome.com/rss/ (the rss.ithome.com host answers nothing).
		"https://www.ithome.com/rss/",
		"https://www.chinanews.com.cn/rss/world.xml",
		"https://www.solidot.org/index.rss",
	}
}

func defaultDashConfig() dashConfig {
	on := true
	return dashConfig{
		Enabled: &on,
		Modules: map[string]bool{
			"sysinfo": true, "clock": true, "geo": true, "weather": true, "brief": true,
			"ruanyifeng": true, "leetcode": true,
			"keywords": false, "stocks": false, "scope": false,
		},
		Custom: []string{"ruanyifeng", "leetcode", "keywords", "stocks", "scope"},
		Intervals: map[string]string{
			"sysinfo": "10s", "geo": "1h", "weather": "30m", "brief": "30m",
			"ruanyifeng": "1h", "leetcode": "1h", "stocks": "30m",
		},
		Feeds:  defaultDashFeeds(),
		Stocks: []string{"1.000001", "0.399001"},
	}
}

// dashGlobalOn: absent enabled key means on.
func dashGlobalOn(cfg *dashConfig) bool { return cfg.Enabled == nil || *cfg.Enabled }

// readDashConfig overlays the file onto the defaults. Unmarshalling into
// the default-filled struct merges maps key-by-key (so a partial
// "modules" object keeps the other defaults) and replaces slices only
// when the key is present; an unreadable file yields pure defaults —
// the dashboard must never refuse to start over a typo.
func readDashConfig(path string) dashConfig {
	cfg := defaultDashConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return defaultDashConfig()
	}
	if cfg.Modules == nil {
		cfg.Modules = defaultDashConfig().Modules
	}
	if cfg.Custom == nil {
		cfg.Custom = defaultDashConfig().Custom
	}
	if cfg.Intervals == nil {
		cfg.Intervals = defaultDashConfig().Intervals
	}
	return cfg
}

// writeDashConfig persists atomically (CreateTemp beside the target +
// rename — a crash never leaves a torn file) and drops the memo so the
// next read comes back from disk.
func writeDashConfig(cfg dashConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := dashWriteFile(dashConfigPath(), data); err != nil {
		return err
	}
	dashStateMu.Lock()
	dashCfgCache = nil
	dashStateMu.Unlock()
	return nil
}

// dashWriteFile stages next to the target and renames over it; the
// staged file is removed on any failure path.
func dashWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".dash-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o644); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// The config memo is keyed by home dir: a test (or user) that points
// $LYON_HOME elsewhere transparently reloads.
var (
	dashStateMu    sync.Mutex
	dashCfgHome    string
	dashCfgCache   *dashConfig
	dashCfgLoading bool // re-entrancy guard: a loader below reads config again
)

func dashConfigNow() dashConfig {
	dashStateMu.Lock()
	// Re-entrant path (cache load under the cell lock reads intervals):
	// skip the memo and read straight from disk.
	if dashCfgLoading {
		dashStateMu.Unlock()
		return readDashConfig(dashConfigPath())
	}
	h := dashHomeDir()
	if dashCfgCache == nil || dashCfgHome != h {
		dashCfgLoading = true
		c := readDashConfig(filepath.Join(h, "dashboard.json"))
		dashCfgLoading = false
		dashCfgCache = &c
		dashCfgHome = h
	}
	c := *dashCfgCache
	dashStateMu.Unlock()
	return c
}

// dashInvalidateConfig drops the memo — tests that write dashboard.json
// by hand call it before re-reading.
func dashInvalidateConfig() {
	dashStateMu.Lock()
	dashCfgCache = nil
	dashStateMu.Unlock()
}

// ---------------------------------------------------------------------------
// Cells: the in-memory last-success store render reads from
// ---------------------------------------------------------------------------

type dashCell struct {
	at    time.Time
	data  any
	stale bool // the most recent refresh attempt failed
}

var (
	dashCellMu      sync.Mutex
	dashCellHome    string
	dashCellMap     map[string]*dashCell
	dashFetchActive = map[string]bool{}
)

// dashCellStoreLocked returns the map for the current home, reloading
// the on-disk cache when the home changed.
func dashCellStoreLocked() map[string]*dashCell {
	h := dashHomeDir()
	if dashCellMap != nil && dashCellHome == h {
		return dashCellMap
	}
	dashCellMap = map[string]*dashCell{}
	dashCellHome = h
	dashLoadCacheLocked(h)
	return dashCellMap
}

func dashGetCell(id string) (*dashCell, bool) {
	dashCellMu.Lock()
	defer dashCellMu.Unlock()
	c, ok := dashCellStoreLocked()[id]
	return c, ok
}

func dashPutCell(id string, data any, at time.Time, stale bool) {
	dashCellMu.Lock()
	defer dashCellMu.Unlock()
	dashCellStoreLocked()[id] = &dashCell{at: at, data: data, stale: stale}
}

func dashMarkFail(id string) {
	dashCellMu.Lock()
	defer dashCellMu.Unlock()
	if c, ok := dashCellStoreLocked()[id]; ok && c != nil {
		c.stale = true
	}
}

// dashCellsReset clears all cells (test hook: offline/placeholder cases
// need a known-empty store).
func dashCellsReset() {
	dashCellMu.Lock()
	defer dashCellMu.Unlock()
	dashCellMap = map[string]*dashCell{}
	dashCellHome = dashHomeDir()
}

// dashLoadCacheNow reloads cells from $LYON_HOME/dashboard-cache.json
// for the current home (test hook and the refreshers' cold start).
func dashLoadCacheNow() {
	dashCellMu.Lock()
	defer dashCellMu.Unlock()
	dashCellMap = map[string]*dashCell{}
	dashCellHome = dashHomeDir()
	dashLoadCacheLocked(dashCellHome)
}

// ---------------------------------------------------------------------------
// Cache file: $LYON_HOME/dashboard-cache.json
// ---------------------------------------------------------------------------

type dashCacheFile struct {
	Saved time.Time               `json:"saved"`
	Cells map[string]dashCacheEnt `json:"cells"`
}

type dashCacheEnt struct {
	At   time.Time       `json:"at"`
	Data json.RawMessage `json:"data"`
}

// dashLoadCacheLocked decodes cached entries into cells, pre-marking
// anything older than twice its refresh interval as stale. Called with
// dashCellMu held.
func dashLoadCacheLocked(home string) {
	data, err := os.ReadFile(filepath.Join(home, "dashboard-cache.json"))
	if err != nil {
		return
	}
	var file dashCacheFile
	if err := json.Unmarshal(data, &file); err != nil {
		return
	}
	cfg := defaultDashConfig() // intervals only; reads config below when available
	if h := os.Getenv("LYON_HOME"); h == home {
		cfg = dashConfigNow()
	}
	for id, ent := range file.Cells {
		mod := dashModLookup(id)
		if mod == nil || mod.decode == nil {
			continue
		}
		v, err := mod.decode(ent.Data)
		if err != nil {
			continue
		}
		stale := false
		if iv := dashIntervalOf(&cfg, mod); iv > 0 && time.Since(ent.At) > 2*iv {
			stale = true
		}
		dashCellStoreLocked()[id] = &dashCell{at: ent.At, data: v, stale: stale}
	}
}

// dashSaveCache snapshots the persistable cells (those with a decode
// side) to disk. Called after each successful network refresh.
func dashSaveCache() {
	dashCellMu.Lock()
	file := dashCacheFile{Saved: time.Now(), Cells: map[string]dashCacheEnt{}}
	for _, mod := range dashRegistry() {
		if mod.decode == nil {
			continue
		}
		c, ok := dashCellStoreLocked()[mod.id]
		if !ok || c == nil || c.data == nil {
			continue
		}
		raw, err := json.Marshal(c.data)
		if err != nil {
			continue
		}
		file.Cells[mod.id] = dashCacheEnt{At: c.at, Data: raw}
	}
	dashCellMu.Unlock()
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return
	}
	_ = dashWriteFile(dashCachePath(), data)
}

// ---------------------------------------------------------------------------
// The module registry
// ---------------------------------------------------------------------------

// sysSnap is the local machine sample every platform file produces.
// Negative percentages and false OK flags mean "unavailable on this
// OS / not yet measured" — render shows an ellipsis, never a lie.
type sysSnap struct {
	CPUPct     float64
	MemPct     float64
	MemUsed    uint64
	MemTotal   uint64
	DiskFree   uint64
	DiskTotal  uint64
	NetUpBps   float64
	NetDownBps float64
	NetOK      bool
	OS         string
}

type dashMod struct {
	id       string
	defOn    bool
	interval time.Duration // 0 = no fetcher (pure local)
	refresh  func(ctx context.Context) (any, error)
	decode   func([]byte) (any, error) // nil → not persisted to the cache
}

var (
	dashRegistryOnce sync.Once
	dashMods         []*dashMod
	dashModsByID     map[string]*dashMod
)

func dashRegistry() []*dashMod {
	dashRegistryOnce.Do(func() {
		dashMods = []*dashMod{
			{id: "sysinfo", defOn: true, interval: 10 * time.Second,
				refresh: func(ctx context.Context) (any, error) {
					return sysProbe(dashWorkspacePath()), nil
				}},
			{id: "clock", defOn: true},
			{id: "geo", defOn: true, interval: time.Hour,
				refresh: fetchGeo,
				decode: func(b []byte) (any, error) {
					var v geoData
					err := json.Unmarshal(b, &v)
					return &v, err
				}},
			{id: "weather", defOn: true, interval: 30 * time.Minute,
				refresh: refreshWeather,
				decode: func(b []byte) (any, error) {
					var v weatherData
					err := json.Unmarshal(b, &v)
					return &v, err
				}},
			{id: "brief", defOn: true, interval: 30 * time.Minute,
				refresh: func(ctx context.Context) (any, error) {
					return fetchBrief(ctx, dashConfigNow().Feeds, dashWorkspacePath())
				},
				decode: func(b []byte) (any, error) {
					var v briefData
					err := json.Unmarshal(b, &v)
					return &v, err
				}},
			{id: "ruanyifeng", defOn: true, interval: time.Hour,
				refresh: func(ctx context.Context) (any, error) {
					return fetchDigest(ctx, dashDigestURL)
				},
				decode: func(b []byte) (any, error) {
					var v digestData
					err := json.Unmarshal(b, &v)
					return &v, err
				}},
			{id: "leetcode", defOn: true, interval: time.Hour,
				refresh: fetchLeetCode,
				decode: func(b []byte) (any, error) {
					var v leetcodeData
					err := json.Unmarshal(b, &v)
					return &v, err
				}},
			{id: "keywords"},
			{id: "stocks", interval: 30 * time.Minute,
				refresh: func(ctx context.Context) (any, error) {
					return fetchStocks(ctx, dashConfigNow().Stocks)
				},
				decode: func(b []byte) (any, error) {
					var v stocksData
					err := json.Unmarshal(b, &v)
					return &v, err
				}},
			{id: "scope"},
		}
		dashModsByID = map[string]*dashMod{}
		for _, mod := range dashMods {
			dashModsByID[mod.id] = mod
		}
	})
	return dashMods
}

func dashModLookup(id string) *dashMod {
	dashRegistry()
	return dashModsByID[id]
}

// dashDigestURL is 阮一峰的网络日志 atom.xml — Cloudflare-fronted, so
// fetchers carry the browser UA.
const dashDigestURL = "https://www.ruanyifeng.com/blog/atom.xml"

// refreshWeather runs only when a coordinate source exists: the live geo
// cell first, else the latitude/longitude remembered inside the last
// weather cell (so an IP outage does not kill an otherwise-working
// weather row).
func refreshWeather(ctx context.Context) (any, error) {
	lat, lon, ok := dashWeatherCoords()
	if !ok {
		return nil, fmt.Errorf("no location yet")
	}
	return fetchWeather(ctx, lat, lon)
}

func dashWeatherCoords() (lat, lon float64, ok bool) {
	if c, has := dashGetCell("geo"); has {
		if g, is := c.data.(*geoData); is {
			return g.Lat, g.Lon, true
		}
	}
	if c, has := dashGetCell("weather"); has {
		if w, is := c.data.(*weatherData); is && (w.Lat != 0 || w.Lon != 0) {
			return w.Lat, w.Lon, true
		}
	}
	return 0, 0, false
}

// dashModEnabled: the modules map wins; a missing key falls back to the
// module's built-in default.
func dashModEnabled(cfg *dashConfig, id string) bool {
	if v, ok := cfg.Modules[id]; ok {
		return v
	}
	if mod := dashModLookup(id); mod != nil {
		return mod.defOn
	}
	return false
}

// dashIntervalOf: the intervals map overrides the built-in default;
// garbage or a non-positive value falls back to the built-in.
func dashIntervalOf(cfg *dashConfig, mod *dashMod) time.Duration {
	if mod.interval <= 0 {
		return 0
	}
	if s, ok := cfg.Intervals[mod.id]; ok && s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return d
		}
	}
	return mod.interval
}

func dashBytes(v uint64) string {
	if v < 1024 {
		return fmt.Sprintf("%dB", v)
	}
	units := []string{"K", "M", "G", "T", "P"}
	div := uint64(1024)
	i := 0
	for v/div >= 1024 && i < len(units)-1 {
		div *= 1024
		i++
	}
	val := float64(v) / float64(div)
	if val < 10 {
		return fmt.Sprintf("%.1f%s", val, units[i])
	}
	return fmt.Sprintf("%.0f%s", val, units[i])
}

var zhWeekdays = [...]string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}

// dashWorkspace feeds the memory profile and the disk probe; set from
// runTUI at refresher start.
var (
	dashWSMu      sync.Mutex
	dashWorkspace string
)

func dashSetWorkspace(ws string) {
	if ws == "" {
		return
	}
	dashWSMu.Lock()
	dashWorkspace = ws
	dashWSMu.Unlock()
}

func dashWorkspacePath() string {
	dashWSMu.Lock()
	defer dashWSMu.Unlock()
	return dashWorkspace
}

// ---------------------------------------------------------------------------
// The 1s render tick (tea.Tick chain — self-contained, no goroutines)
// ---------------------------------------------------------------------------

type dashTickMsg struct{}

func dashTickCmd() tea.Cmd {
	if dashEnvOff() {
		return nil
	}
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return dashTickMsg{} })
}

// dashTick just rearms the chain: the transcript no longer carries a
// dashboard block — the tick's per-second Update is what repaints the
// header clock.
func (m *tuiModel) dashTick() tea.Cmd { return dashTickCmd() }

// ---------------------------------------------------------------------------
// Background refreshers: one goroutine per module, all independent
// ---------------------------------------------------------------------------

var (
	dashLoopsOnce    sync.Once
	dashLoopsRunning bool
)

// dashStartRefreshers is called once from runTUI — and only there: tests
// build models directly, so the suite never starts fetchers and never
// touches the real network. Every refreshable module gets its own
// goroutine; the first wave therefore runs fully concurrent (the
// product's hard concurrency disposition).
func dashStartRefreshers(workspace string) {
	if dashEnvOff() {
		return
	}
	dashSetWorkspace(workspace)
	dashLoadCacheNow() // cold start paints from last-success, marked stale by age
	dashLoopsOnce.Do(func() {
		dashLoopsRunning = true
		for _, mod := range dashRegistry() {
			if mod.refresh == nil {
				continue
			}
			go dashRefresherLoop(mod)
		}
	})
}

func dashRefresherLoop(mod *dashMod) {
	for {
		cfg := dashConfigNow()
		if dashGlobalOn(&cfg) && dashModEnabled(&cfg, mod.id) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			dashRefreshOnce(ctx, mod.id)
			cancel()
		}
		// Re-read the interval each cycle so a /dash interval edit lands
		// without a restart.
		d := dashIntervalOf(&cfg, mod)
		if d <= 0 {
			d = time.Minute
		}
		timer := time.NewTimer(d)
		<-timer.C
	}
}

// dashTryFetch coalesces refreshes per module: if one fetch is already
// in flight (the geo kick racing the weather loop, say), the newcomer
// drops instead of stacking.
func dashTryFetch(id string) bool {
	dashCellMu.Lock()
	defer dashCellMu.Unlock()
	if dashFetchActive[id] {
		return false
	}
	dashFetchActive[id] = true
	return true
}

func dashReleaseFetch(id string) {
	dashCellMu.Lock()
	delete(dashFetchActive, id)
	dashCellMu.Unlock()
}

// dashRefreshOnce is one full refresh cycle for one module: fetch into a
// cell on success, badge the old cell stale on failure. Tests call this
// directly against httptest stubs.
func dashRefreshOnce(ctx context.Context, id string) {
	mod := dashModLookup(id)
	if mod == nil || mod.refresh == nil {
		return
	}
	cfg := dashConfigNow()
	if !dashGlobalOn(&cfg) || !dashModEnabled(&cfg, id) {
		return
	}
	if !dashTryFetch(id) {
		return
	}
	defer dashReleaseFetch(id)
	data, err := mod.refresh(ctx)
	if err != nil {
		dashMarkFail(id)
		return
	}
	dashPutCell(id, data, time.Now(), false)
	if mod.decode != nil {
		dashSaveCache() // persistable modules refresh the cache file
	}
	// A fresh location makes the weather row worth one immediate cycle —
	// but only inside the production loops, so tests never inherit a
	// surprise goroutine.
	if id == "geo" && dashLoopsRunning {
		go func() {
			wctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			dashRefreshOnce(wctx, "weather")
		}()
	}
}
