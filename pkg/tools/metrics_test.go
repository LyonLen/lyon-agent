package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestMetricsAggregatesCallsErrorsPercentiles(t *testing.T) {
	m := NewMetrics()
	m.OnToolEnd(ExecEvent{Name: "probe", Duration: 10 * time.Millisecond})
	m.OnToolEnd(ExecEvent{Name: "probe", Duration: 20 * time.Millisecond})
	m.OnToolEnd(ExecEvent{Name: "probe", Duration: 30 * time.Millisecond, Err: "boom"})
	m.OnToolEnd(ExecEvent{Name: "other", Duration: 5 * time.Millisecond})

	snap := m.Snapshot()
	if len(snap) != 2 || snap[0].Name != "other" || snap[1].Name != "probe" {
		t.Fatalf("snapshot not sorted by name: %+v", snap)
	}
	p := snap[1]
	if p.Calls != 3 || p.Errors != 1 {
		t.Fatalf("calls=%d errors=%d, want 3/1", p.Calls, p.Errors)
	}
	if p.Total != 60*time.Millisecond || p.Max != 30*time.Millisecond {
		t.Fatalf("total=%v max=%v", p.Total, p.Max)
	}
	if p.P50 != 20*time.Millisecond || p.P95 != 30*time.Millisecond {
		t.Fatalf("p50=%v p95=%v, want 20ms/30ms", p.P50, p.P95)
	}
	if p.Avg() != 20*time.Millisecond {
		t.Fatalf("avg=%v, want 20ms", p.Avg())
	}
	if p.LastErr != "boom" {
		t.Fatalf("lastErr=%q", p.LastErr)
	}
}

func TestMetricsWiredThroughAdapt(t *testing.T) {
	m := NewMetrics()
	a := Adapt(&funcTool{name: "wire_probe", parallel: true, result: "ok"},
		AdapterConfig{Observer: m})
	if _, err := a.Execute(context.Background(), json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	// A failure too.
	b := Adapt(&funcTool{name: "wire_probe", parallel: true, panicMsg: "x"},
		AdapterConfig{Observer: m})
	_, _ = b.Execute(context.Background(), json.RawMessage(`{}`))

	snap := m.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("want one tool, got %d", len(snap))
	}
	got := snap[0]
	if got.Calls != 2 || got.Errors != 1 {
		t.Fatalf("calls=%d errors=%d, want 2/1", got.Calls, got.Errors)
	}
}

func TestMetricsRingKeepsMemoryFlat(t *testing.T) {
	m := NewMetrics()
	for i := 0; i < metricSamples*3; i++ {
		m.OnToolEnd(ExecEvent{Name: "ring", Duration: time.Duration(i) * time.Millisecond})
	}
	m.mu.Lock()
	n := len(m.tools["ring"].durs)
	m.mu.Unlock()
	if n > metricSamples {
		t.Fatalf("ring grew to %d, cap %d", n, metricSamples)
	}
	if snap := m.Snapshot(); snap[0].Calls != metricSamples*3 {
		t.Fatalf("calls=%d, want %d", snap[0].Calls, metricSamples*3)
	}
}
