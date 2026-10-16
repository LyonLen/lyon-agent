package main

// The startup home dashboard (#10). Four module groups render into the
// transcript right under the commands banner — machine stats, clock +
// IP location + weather, a personality-profiled news brief, and the
// custom group (ruanyifeng digest, leetcode daily, daily keyword,
// quotes, oscilloscope). The architecture keeps three promises:
//
//   - The TUI event loop never fetches. Render reads an in-memory cell
//     map; background goroutines (one per refreshable module, started
//     only from runTUI) write cells on each module's own interval, and
//     a 1s tea.Tick chain re-renders the block while it sits at the
//     transcript tail.
//   - Offline is a first-class state: a failed fetch keeps the
//     last-success cell with a stale badge; a module with no data ever
//     renders one dim placeholder row; a render panic is caught per
//     module. Nothing here can take the TUI down.
//   - Every kill path is an env var: LYON_DASHBOARD=off reverts to the
//     banner-only startup view with zero dashboard behavior — no seeds,
//     no ticks, no refreshers. Config lives at $LYON_HOME/dashboard.json,
//     the last-success cache at $LYON_HOME/dashboard-cache.json.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charmbracelet/x/ansi"
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
			"sysinfo": "5s", "geo": "1h", "weather": "30m", "brief": "30m",
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
	group    int // 1 machine · 2 time/place · 3 brief · 4 custom
	custom   bool
	defOn    bool
	interval time.Duration // 0 = render-driven (no fetcher)
	refresh  func(ctx context.Context) (any, error)
	decode   func([]byte) (any, error) // nil → not persisted to the cache
	render   func(m *tuiModel, cell *dashCell, has bool) []string
}

var (
	dashRegistryOnce sync.Once
	dashMods         []*dashMod
	dashModsByID     map[string]*dashMod
)

func dashRegistry() []*dashMod {
	dashRegistryOnce.Do(func() {
		dashMods = []*dashMod{
			{id: "sysinfo", group: 1, defOn: true, interval: 5 * time.Second,
				refresh: func(ctx context.Context) (any, error) {
					return sysProbe(dashWorkspacePath()), nil
				},
				render: renderSysinfo},
			{id: "clock", group: 2, defOn: true, render: renderClock},
			{id: "geo", group: 2, defOn: true, interval: time.Hour,
				refresh: fetchGeo,
				decode: func(b []byte) (any, error) {
					var v geoData
					err := json.Unmarshal(b, &v)
					return &v, err
				},
				render: renderGeo},
			{id: "weather", group: 2, defOn: true, interval: 30 * time.Minute,
				refresh: refreshWeather,
				decode: func(b []byte) (any, error) {
					var v weatherData
					err := json.Unmarshal(b, &v)
					return &v, err
				},
				render: renderWeather},
			{id: "brief", group: 3, defOn: true, interval: 30 * time.Minute,
				refresh: func(ctx context.Context) (any, error) {
					return fetchBrief(ctx, dashConfigNow().Feeds, dashWorkspacePath())
				},
				decode: func(b []byte) (any, error) {
					var v briefData
					err := json.Unmarshal(b, &v)
					return &v, err
				},
				render: renderBrief},
			{id: "ruanyifeng", group: 4, custom: true, defOn: true, interval: time.Hour,
				refresh: func(ctx context.Context) (any, error) {
					return fetchDigest(ctx, dashDigestURL)
				},
				decode: func(b []byte) (any, error) {
					var v digestData
					err := json.Unmarshal(b, &v)
					return &v, err
				},
				render: renderDigest},
			{id: "leetcode", group: 4, custom: true, defOn: true, interval: time.Hour,
				refresh: fetchLeetCode,
				decode: func(b []byte) (any, error) {
					var v leetcodeData
					err := json.Unmarshal(b, &v)
					return &v, err
				},
				render: renderLeetcode},
			{id: "keywords", group: 4, custom: true, render: renderKeywords},
			{id: "stocks", group: 4, custom: true, interval: 30 * time.Minute,
				refresh: func(ctx context.Context) (any, error) {
					return fetchStocks(ctx, dashConfigNow().Stocks)
				},
				decode: func(b []byte) (any, error) {
					var v stocksData
					err := json.Unmarshal(b, &v)
					return &v, err
				},
				render: renderStocks},
			{id: "scope", group: 4, custom: true, render: renderScope},
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

// dashRenderOrder: core groups in fixed order, then the custom group in
// the user's configured order (membership list — an id the user removed
// from "custom" stops rendering even when enabled).
func dashRenderOrder(cfg *dashConfig) []*dashMod {
	var out []*dashMod
	seen := map[string]bool{}
	for _, mod := range dashRegistry() {
		if mod.custom {
			continue
		}
		out = append(out, mod)
		seen[mod.id] = true
	}
	for _, id := range cfg.Custom {
		if seen[id] {
			continue
		}
		if mod, ok := dashModsByID[id]; ok && mod.custom {
			out = append(out, mod)
			seen[id] = true
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

// dashLabel pads a bilingual module label to a fixed 9-cell gutter.
func dashLabel(s string) string {
	const w = 9
	switch pad := w - ansi.StringWidth(s); {
	case pad > 0:
		return s + strings.Repeat(" ", pad)
	case pad < 0:
		return ansi.Truncate(s, w-1, "") + " "
	}
	return s
}

// dashFit truncates over-wide rows; the reset closes any style the cut
// would otherwise leave open (renderTranscript wraps anything longer,
// but pre-fitting keeps one logical row one physical row).
func dashFit(s string, w int) string {
	if w > 0 && ansi.StringWidth(s) > w {
		return ansi.Truncate(s, w, "…") + cReset
	}
	return s
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

// dashTerminalName best-effort identifies the host terminal from env —
// the same signals the rest of the ecosystem uses.
func dashTerminalName() string {
	if os.Getenv("WT_SESSION") != "" {
		return "windows-terminal"
	}
	if v := os.Getenv("TERM_PROGRAM"); v != "" {
		return strings.ToLower(strings.TrimSuffix(v, ".app"))
	}
	if os.Getenv("ConEmuANSI") == "ON" {
		return "conemu"
	}
	if os.Getenv("ANSICON") != "" {
		return "ansicon"
	}
	if v := os.Getenv("TERM"); v != "" {
		return v
	}
	if runtime.GOOS == "windows" {
		return "console"
	}
	return "tty"
}

var zhWeekdays = [...]string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}

func dashPlaceholder() string { return paint(cDim, tr("waiting for data", "等待数据")) }

// dashRows builds the whole dashboard block for the current width. Every
// module gets a recover guard: one broken renderer degrades to its own
// placeholder row, never the page.
func (m *tuiModel) dashRows() []string {
	w := m.tw
	if w <= 0 {
		w = 80
	}
	cfg := dashConfigNow()
	rows := []string{paint(cBold+cCyan, tr("lyon home", "lyon 首页"))}
	lastGroup := 0
	for _, mod := range dashRenderOrder(&cfg) {
		if !dashModEnabled(&cfg, mod.id) {
			continue
		}
		if lastGroup != 0 && mod.group != lastGroup {
			rows = append(rows, "")
		}
		lastGroup = mod.group
		cell, has := dashGetCell(mod.id)
		out := dashSafeRender(m, mod, cell, has)
		if has && cell != nil && (cell.stale || dashAgedStale(&cfg, mod, cell)) && len(out) > 0 {
			out[len(out)-1] += " " + paint(cYellow, tr("stale", "离线"))
		}
		for _, r := range out {
			rows = append(rows, dashFit(r, w))
		}
	}
	rows = append(rows, "") // the conversation below starts on a clean line
	return rows
}

func dashAgedStale(cfg *dashConfig, mod *dashMod, cell *dashCell) bool {
	iv := dashIntervalOf(cfg, mod)
	return iv > 0 && time.Since(cell.at) > 2*iv
}

func dashSafeRender(m *tuiModel, mod *dashMod, cell *dashCell, has bool) (rows []string) {
	defer func() {
		if r := recover(); r != nil {
			rows = []string{dashLabel(mod.id) + paint(cYellow, tr("render error", "渲染错误"))}
		}
	}()
	if mod.render == nil {
		return nil
	}
	return mod.render(m, cell, has)
}

func renderSysinfo(m *tuiModel, cell *dashCell, has bool) []string {
	label := dashLabel(tr("machine", "机器"))
	if !has {
		return []string{label + dashPlaceholder()}
	}
	s, ok := cell.data.(sysSnap)
	if !ok {
		return []string{label + dashPlaceholder()}
	}
	dim := func(s string) string { return paint(cDim, s) }
	var r1 []string
	if s.CPUPct >= 0 {
		r1 = append(r1, fmt.Sprintf("%s %.0f%%", tr("cpu", "处理器"), s.CPUPct))
	} else {
		r1 = append(r1, tr("cpu", "处理器")+" …")
	}
	switch {
	case s.MemTotal > 0 && s.MemUsed > 0:
		pct := s.MemPct
		if pct < 0 {
			pct = float64(s.MemUsed) / float64(s.MemTotal) * 100
		}
		r1 = append(r1, fmt.Sprintf("%s %s/%s %.0f%%", tr("mem", "内存"),
			dashBytes(s.MemUsed), dashBytes(s.MemTotal), pct))
	case s.MemTotal > 0:
		r1 = append(r1, fmt.Sprintf("%s %s", tr("mem", "内存"), dashBytes(s.MemTotal)))
	default:
		r1 = append(r1, tr("mem", "内存")+" …")
	}
	if s.DiskTotal > 0 {
		used := s.DiskTotal - s.DiskFree
		r1 = append(r1, fmt.Sprintf("%s %s/%s %.0f%%", tr("disk", "磁盘"),
			dashBytes(used), dashBytes(s.DiskTotal),
			float64(used)/float64(s.DiskTotal)*100))
	} else {
		r1 = append(r1, tr("disk", "磁盘")+" …")
	}
	var r2 []string
	if s.NetOK {
		r2 = append(r2, fmt.Sprintf("%s ↑%s/s ↓%s/s", tr("net", "网络"),
			dashBytes(uint64(s.NetUpBps)), dashBytes(uint64(s.NetDownBps))))
	} else {
		r2 = append(r2, tr("net", "网络")+" …")
	}
	if s.OS != "" {
		r2 = append(r2, "os "+s.OS)
	}
	r2 = append(r2, fmt.Sprintf("%s %d×%d", dashTerminalName(), m.width, m.height))
	return []string{
		label + strings.Join(r1, dim(" · ")),
		dashLabel("") + strings.Join(r2, dim(" · ")),
	}
}

func renderClock(m *tuiModel, cell *dashCell, has bool) []string {
	now := time.Now()
	week, _ := now.ISOWeek()
	en := now.Format("2006-01-02 Mon 15:04:05") + fmt.Sprintf(" · week %d", week)
	zh := now.Format("2006-01-02") + " " + zhWeekdays[int(now.Weekday())] + " " +
		now.Format("15:04:05") + fmt.Sprintf(" · 第%d周", week)
	return []string{dashLabel(tr("time", "时间")) + tr(en, zh)}
}

func renderGeo(m *tuiModel, cell *dashCell, has bool) []string {
	label := dashLabel(tr("location", "位置"))
	if !has {
		return []string{label + paint(cDim, tr("locating…", "定位中…"))}
	}
	g, ok := cell.data.(*geoData)
	if !ok {
		return []string{label + paint(cDim, tr("locating…", "定位中…"))}
	}
	var parts []string
	if g.City != "" {
		parts = append(parts, g.City)
	}
	if g.Region != "" && g.Region != g.City {
		parts = append(parts, g.Region)
	}
	if g.Country != "" && g.Country != g.City && g.Country != g.Region {
		parts = append(parts, g.Country)
	}
	line := strings.Join(parts, " · ")
	if line == "" {
		line = "—"
	}
	if g.IP != "" {
		line += "  " + paint(cDim, g.IP)
	}
	return []string{label + line}
}

func renderWeather(m *tuiModel, cell *dashCell, has bool) []string {
	label := dashLabel(tr("weather", "天气"))
	if !has {
		_, _, located := dashWeatherCoords()
		msg := tr("waiting for weather", "等待天气")
		if !located {
			msg = tr("waiting for location", "等待位置")
		}
		return []string{label + paint(cDim, msg)}
	}
	w, ok := cell.data.(*weatherData)
	if !ok {
		return []string{label + dashPlaceholder()}
	}
	en, zh := weatherDesc(w.Code)
	line := fmt.Sprintf("%.0f°C %s · %s %.0fkm/h · %.0f/%.0f°",
		w.Temp, tr(en, zh), tr("wind", "风"), w.Wind, w.Hi, w.Lo)
	return []string{label + line}
}

func renderBrief(m *tuiModel, cell *dashCell, has bool) []string {
	var b *briefData
	if has {
		b, _ = cell.data.(*briefData)
	}
	profile := "—"
	var industry string
	if b != nil && len(b.Profile) > 0 {
		profile = strings.Join(b.Profile, " · ")
		industry = classifyIndustry(profile)
	}
	row1 := dashLabel(tr("brief", "简报")) + paint(cDim, tr("profile ", "画像 ")) + profile
	newsLabel := dashLabel("")
	var news string
	if b != nil && len(b.Items) > 0 {
		var titles []string
		for i, it := range b.Items {
			if i >= 3 {
				break
			}
			titles = append(titles, ansi.Truncate(it.Title, 40, "…"))
		}
		news = strings.Join(titles, paint(cDim, " · "))
	} else {
		news = paint(cDim, tr("news offline — local tips below", "新闻离线 — 下为本地提示"))
	}
	row2 := newsLabel + news
	tipEn, tipZh := dashPickTip(industry, time.Now())
	row3 := dashLabel(tr("tip", "提示")) + tr(tipEn, tipZh)
	return []string{row1, row2, row3}
}

func renderDigest(m *tuiModel, cell *dashCell, has bool) []string {
	label := dashLabel(tr("digest", "摘要"))
	if !has {
		return []string{label + dashPlaceholder()}
	}
	d, ok := cell.data.(*digestData)
	if !ok {
		return []string{label + dashPlaceholder()}
	}
	line := d.Title
	if d.Date != "" {
		line = d.Date + " · " + d.Title
	}
	return []string{label + line}
}

func renderLeetcode(m *tuiModel, cell *dashCell, has bool) []string {
	label := dashLabel(tr("daily", "每日"))
	if !has {
		return []string{label + dashPlaceholder()}
	}
	d, ok := cell.data.(*leetcodeData)
	if !ok {
		return []string{label + dashPlaceholder()}
	}
	zhTitle := d.Translated
	if zhTitle == "" {
		zhTitle = d.Title
	}
	zhDiff := map[string]string{"Easy": "简单", "Medium": "中等", "Hard": "困难"}[d.Difficulty]
	if zhDiff == "" {
		zhDiff = d.Difficulty
	}
	line := fmt.Sprintf("#%s %s (%s)", d.FrontendID, tr(d.Title, zhTitle), tr(d.Difficulty, zhDiff))
	return []string{label + line}
}

// dashIndustryNow reads the industry bucket out of the brief cell so the
// keyword row follows the same profile as the news ranking.
func dashIndustryNow() string {
	if c, ok := dashGetCell("brief"); ok {
		if b, is := c.data.(*briefData); is && len(b.Profile) > 0 {
			return classifyIndustry(strings.Join(b.Profile, " "))
		}
	}
	return "generic"
}

func renderKeywords(m *tuiModel, cell *dashCell, has bool) []string {
	industry := dashIndustryNow()
	en, zh := dashPickKeyword(industry, time.Now())
	line := time.Now().Format("2006-01-02") + " · " + industry + " · " + tr(en, zh)
	return []string{dashLabel(tr("keyword", "关键词")) + line}
}

func renderStocks(m *tuiModel, cell *dashCell, has bool) []string {
	label := dashLabel(tr("stocks", "行情"))
	if !has {
		return []string{label + dashPlaceholder()}
	}
	s, ok := cell.data.(*stocksData)
	if !ok || len(s.Quotes) == 0 {
		return []string{label + dashPlaceholder()}
	}
	var parts []string
	for _, q := range s.Quotes {
		// CN market convention: red rises, green falls.
		var pct string
		switch {
		case q.Pct > 0:
			pct = paint(cRed, fmt.Sprintf("+%.2f%%", q.Pct))
		case q.Pct < 0:
			pct = paint(cGreen, fmt.Sprintf("%.2f%%", q.Pct))
		default:
			pct = paint(cDim, "0.00%")
		}
		parts = append(parts, fmt.Sprintf("%s %.2f %s", q.Name, q.Price, pct))
	}
	return []string{label + strings.Join(parts, paint(cDim, " · "))}
}

func renderScope(m *tuiModel, cell *dashCell, has bool) []string {
	w := m.tw
	if w <= 0 {
		w = 80
	}
	wave := dashWave(w-9, time.Now())
	return []string{dashLabel(tr("scope", "示波")) + paint(cCyan, wave)}
}

// ---------------------------------------------------------------------------
// Transcript block lifecycle: seed at startup, live-refresh at the tail
// ---------------------------------------------------------------------------

// dashWorkspace feeds the memory profile and the disk probe; set from
// the model at seed time and from runTUI at refresher start.
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

// dashBlocks maps a model to its live dashboard region so several models
// (tests build many) never scribble on each other.
type dashBlock struct {
	start, n int
	last     string
}

var (
	dashBlockMu sync.Mutex
	dashBlocks  = map[*tuiModel]*dashBlock{}
)

func dashPutBlock(m *tuiModel, start, n int, last string) {
	dashBlockMu.Lock()
	dashBlocks[m] = &dashBlock{start: start, n: n, last: last}
	dashBlockMu.Unlock()
}

func dashGetBlock(m *tuiModel) (*dashBlock, bool) {
	dashBlockMu.Lock()
	defer dashBlockMu.Unlock()
	b, ok := dashBlocks[m]
	return b, ok
}

func dashDelBlock(m *tuiModel) {
	dashBlockMu.Lock()
	delete(dashBlocks, m)
	dashBlockMu.Unlock()
}

// dashSeed appends the dashboard under the startup banner. Called from
// resetTranscript (new model, /new, /resume) and dashHome.
func (m *tuiModel) dashSeed() {
	if dashEnvOff() {
		return
	}
	cfg := dashConfigNow()
	if !dashGlobalOn(&cfg) {
		return
	}
	dashSetWorkspace(m.workspace)
	rows := m.dashRows()
	start := len(m.lines)
	for _, r := range rows {
		m.lines = append(m.lines, tline{kind: lineStyled, raw: r})
	}
	dashPutBlock(m, start, len(rows), strings.Join(rows, "\n"))
}

// dashRenderBlock re-renders the live block in place — but only while it
// still sits at the transcript tail. Once the conversation (or /clear)
// moves the tail, the seeded dashboard is history and freezes; /home or
// /new brings a fresh one.
func (m *tuiModel) dashRenderBlock() {
	b, ok := dashGetBlock(m)
	if !ok {
		return
	}
	if b.start+b.n != len(m.lines) {
		if len(m.lines) == 0 { // /clear wiped the view — the block is gone
			dashDelBlock(m)
		}
		return
	}
	if dashEnvOff() {
		return
	}
	cfg := dashConfigNow()
	if !dashGlobalOn(&cfg) {
		return
	}
	rows := m.dashRows()
	joined := strings.Join(rows, "\n")
	if joined == b.last {
		return // nothing changed since the last second — skip the churn
	}
	tlines := make([]tline, len(rows))
	for i, r := range rows {
		tlines[i] = tline{kind: lineStyled, raw: r}
	}
	m.lines = append(m.lines[:b.start], tlines...)
	dashPutBlock(m, b.start, len(rows), joined)
	m.refresh()
}

// dashStripBlock removes the live block (used by /dash off) without
// touching anything around it.
func (m *tuiModel) dashStripBlock() {
	if b, ok := dashGetBlock(m); ok && b.start+b.n <= len(m.lines) {
		m.lines = append(m.lines[:b.start], m.lines[b.start+b.n:]...)
	}
	dashDelBlock(m)
}

// dashHome returns the view to the dashboard: banner + fresh block,
// conversation history kept exactly like /clear keeps it, and the audit
// panel + plan preserved — this is view navigation, not a session reset.
func (m *tuiModel) dashHome() {
	if dashEnvOff() {
		return
	}
	steps, seq, sel, open, outOpen, todos :=
		m.steps, m.stepSeq, m.stepSel, m.stepOpen, m.stepOutOpen, m.todos
	m.resetTranscript() // reseeds banner + dashboard
	if _, ok := dashGetBlock(m); !ok {
		m.dashSeed() // safety net if the wiring is toggled off
	}
	m.steps, m.stepSeq, m.stepSel, m.stepOpen, m.stepOutOpen, m.todos =
		steps, seq, sel, open, outOpen, todos
	m.follow = true
	m.refresh()
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

func (m *tuiModel) dashTick() tea.Cmd {
	m.dashRenderBlock()
	return dashTickCmd()
}

// ---------------------------------------------------------------------------
// /dash and /home
// ---------------------------------------------------------------------------

func (m *tuiModel) dashCommand(args string) {
	args = strings.TrimSpace(args)
	switch {
	case dashEnvOff():
		m.pushCmd(paint(cYellow, tr("dashboard disabled via LYON_DASHBOARD=off",
			"仪表盘已被 LYON_DASHBOARD=off 关闭")))
	case args == "":
		// Bare /dash is both the config panel and the way back home.
		m.dashHome()
		m.pushCmd(dashPanelRows()...)
	case args == "on", args == "off":
		cfg := dashConfigNow()
		on := args == "on"
		cfg.Enabled = &on
		if err := writeDashConfig(cfg); err != nil {
			m.pushCmd(paint(cYellow, tr("config write failed: ", "配置写入失败: ")+sanitizeText(err.Error())))
			return
		}
		if on {
			m.dashHome()
			m.pushCmd(paint(cGreen, tr("dashboard on", "仪表盘已开启")))
		} else {
			m.dashStripBlock()
			m.pushCmd(paint(cDim, tr("dashboard off", "仪表盘已关闭")))
		}
	case strings.HasPrefix(args, "on "), strings.HasPrefix(args, "off "):
		on := strings.HasPrefix(args, "on ")
		rest := strings.TrimPrefix(args, "on ")
		if !on {
			rest = strings.TrimPrefix(args, "off ")
		}
		id := strings.TrimSpace(rest)
		mod := dashModLookup(id)
		if mod == nil {
			m.pushCmd(
				paint(cRed, tr("unknown module: "+id, "未知模块: "+id)),
				paint(cDim, tr("known: "+strings.Join(dashModuleIDs(), " "),
					"已知: "+strings.Join(dashModuleIDs(), " "))),
			)
			return
		}
		cfg := dashConfigNow()
		if cfg.Modules == nil {
			cfg.Modules = map[string]bool{}
		}
		cfg.Modules[id] = on
		if mod.custom && !dashContains(cfg.Custom, id) {
			cfg.Custom = append(cfg.Custom, id) // toggling a removed custom id rejoins the group
		}
		if err := writeDashConfig(cfg); err != nil {
			m.pushCmd(paint(cYellow, tr("config write failed: ", "配置写入失败: ")+sanitizeText(err.Error())))
			return
		}
		state := tr("off", "关")
		if on {
			state = tr("on", "开")
		}
		m.pushCmd(paint(cDim, id+" → "+state))
		m.dashRenderBlock() // re-lays the block when it is still at the tail
	default:
		m.dashUsage()
	}
}

func dashContains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func dashModuleIDs() []string {
	var ids []string
	for _, mod := range dashRegistry() {
		ids = append(ids, mod.id)
	}
	return ids
}

func (m *tuiModel) dashUsage() {
	m.pushCmd(
		paint(cYellow, tr("usage: /dash on|off [module] — /dash or /home returns to the dashboard",
			"用法: /dash on|off [模块] — /dash 或 /home 回到仪表盘")),
	)
}

// dashPanelRows is the /dash config panel: per-module state, refresh
// intervals, the file it persists to, and the env kill switch.
func dashPanelRows() []string {
	cfg := dashConfigNow()
	var on, off []string
	for _, mod := range dashRegistry() {
		if dashModEnabled(&cfg, mod.id) {
			on = append(on, mod.id)
		} else {
			off = append(off, mod.id)
		}
	}
	rows := []string{
		paint(cBold, tr("dashboard", "仪表盘")) + paint(cDim, "  "+dashConfigPath()),
	}
	if len(on) > 0 {
		rows = append(rows, paint(cGreen, tr("on ", "开 "))+
			strings.Join(on, paint(cDim, " ")))
	}
	if len(off) > 0 {
		rows = append(rows, paint(cDim, tr("off", "关")+" "+
			strings.Join(off, " ")))
	}
	var ivs []string
	for _, mod := range dashRegistry() {
		if mod.interval > 0 {
			ivs = append(ivs, fmt.Sprintf("%s %s", mod.id, dashIntervalOf(&cfg, mod)))
		}
	}
	rows = append(rows, paint(cDim, tr("refresh ", "刷新 "))+strings.Join(ivs, paint(cDim, " · ")))
	if !dashGlobalOn(&cfg) {
		rows = append(rows, paint(cYellow, tr("dashboard is off — /dash on re-enables it",
			"仪表盘当前关闭 — /dash on 重新开启")))
	}
	rows = append(rows, paint(cDim, tr("LYON_DASHBOARD=off disables everything · /dash on|off <module> toggles a row",
		"LYON_DASHBOARD=off 关闭全部 · /dash on|off <模块> 开关单项")))
	return rows
}

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
