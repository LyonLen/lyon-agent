package tools

import (
	"sort"
	"sync"
	"time"
)

// metricSamples is the ring capacity for percentile computation: the most
// recent N durations per tool feed p50/p95, keeping memory flat on long
// sessions while staying representative.
const metricSamples = 512

// ToolMetric is the aggregated view of one tool's executions.
type ToolMetric struct {
	Name    string
	Calls   int
	Errors  int
	Total   time.Duration
	Max     time.Duration
	P50     time.Duration
	P95     time.Duration
	LastErr string
}

// Avg returns the mean execution time, or zero when there are no calls.
func (m ToolMetric) Avg() time.Duration {
	if m.Calls == 0 {
		return 0
	}
	return m.Total / time.Duration(m.Calls)
}

type toolSample struct {
	calls, errors int
	total, max    time.Duration
	durs          []time.Duration // ring buffer, oldest first
	lastErr       string
}

func (s *toolSample) record(d time.Duration, err string) {
	s.calls++
	s.total += d
	if d > s.max {
		s.max = d
	}
	if err != "" {
		s.errors++
		s.lastErr = err
	}
	if len(s.durs) < metricSamples {
		s.durs = append(s.durs, d)
		return
	}
	// Ring: shift is O(n) but n is capped at 512 and writes are per tool
	// execution — negligible next to the execution itself.
	copy(s.durs, s.durs[1:])
	s.durs[len(s.durs)-1] = d
}

// Metrics is an Observer aggregating per-tool execution statistics. Safe for
// concurrent use (tools execute in parallel within a turn).
type Metrics struct {
	mu    sync.Mutex
	tools map[string]*toolSample
}

// NewMetrics creates an empty collector.
func NewMetrics() *Metrics {
	return &Metrics{tools: make(map[string]*toolSample)}
}

// OnToolStart is a no-op: all accounting happens at end, where Duration and
// Err are final. Present to satisfy Observer.
func (m *Metrics) OnToolStart(ExecEvent) {}

// OnToolEnd records one completed (or failed) execution.
func (m *Metrics) OnToolEnd(e ExecEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.tools[e.Name]
	if s == nil {
		s = &toolSample{}
		m.tools[e.Name] = s
	}
	s.record(e.Duration, e.Err)
}

// Snapshot returns one metric per tool, sorted by name.
func (m *Metrics) Snapshot() []ToolMetric {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ToolMetric, 0, len(m.tools))
	for name, s := range m.tools {
		tm := ToolMetric{
			Name:    name,
			Calls:   s.calls,
			Errors:  s.errors,
			Total:   s.total,
			Max:     s.max,
			LastErr: s.lastErr,
		}
		tm.P50 = percentile(s.durs, 0.50)
		tm.P95 = percentile(s.durs, 0.95)
		out = append(out, tm)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// percentile picks the nearest-rank percentile from an unsorted sample. The
// caller holds the lock, so sorting a copy is safe.
func percentile(durs []time.Duration, q float64) time.Duration {
	if len(durs) == 0 {
		return 0
	}
	cp := make([]time.Duration, len(durs))
	copy(cp, durs)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	idx := int(q*float64(len(cp))+0.999999) - 1 // nearest-rank, 1-based
	if idx < 0 {
		idx = 0
	}
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}
