package server

import (
	"encoding/json"
	"testing"

	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/tools"
)

func TestMetricsEndpoint(t *testing.T) {
	eng := agent.NewEngine(agent.DefaultConfig(), nil)
	st := NewStore(0, 0)
	s := New(eng, st)
	s.SetObserver(tools.NewMetrics())
	srv := newHTTPTestServer(t, s)

	code, body := srv.get("/metrics")
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}

	var doc struct {
		UptimeSeconds int     `json:"uptime_seconds"`
		Sessions      int     `json:"sessions"`
		Model         string  `json:"model"`
		CacheShare    float64 `json:"cache_share"`
		Goroutines    int     `json:"goroutines"`
		Turns         struct {
			Turns         int   `json:"turns"`
			Input         int64 `json:"input"`
			CacheRead     int64 `json:"cache_read"`
			CacheCreation int64 `json:"cache_creation"`
		} `json:"turns"`
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}
	if doc.Model == "" || doc.Goroutines <= 0 {
		t.Fatalf("missing process fields: %s", body)
	}
	if doc.Sessions != 0 || doc.Turns.Turns != 0 {
		t.Fatalf("fresh server should be empty: %s", body)
	}
	// tools key present (empty array) when the observer is a Metrics.
	if doc.Tools == nil {
		t.Errorf("tools array missing (observer wired?): %s", body)
	}
}

func TestMetricsEndpointObservesTools(t *testing.T) {
	eng := agent.NewEngine(agent.DefaultConfig(), nil)
	st := NewStore(0, 0)
	s := New(eng, st)
	m := tools.NewMetrics()
	s.SetObserver(m)

	// Feed a synthetic execution through the shared observer.
	m.OnToolEnd(tools.ExecEvent{Name: "run_shell", Duration: 40 * 1e6})
	sess := eng.NewSession(m)
	defer sess.Close()
	id, err := st.Create(sess)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_ = id

	c := newHTTPTestServer(t, s)
	code, body := c.get("/metrics")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	var doc struct {
		Tools []struct {
			Name  string  `json:"name"`
			Calls int     `json:"calls"`
			P95MS float64 `json:"p95_ms"`
		} `json:"tools"`
		Sessions int `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc.Sessions != 1 {
		t.Errorf("sessions = %d, want 1", doc.Sessions)
	}
	found := false
	for _, tool := range doc.Tools {
		if tool.Name == "run_shell" && tool.Calls == 1 && tool.P95MS == 40 {
			found = true
		}
	}
	if !found {
		t.Errorf("tool metrics missing from /metrics: %s", body)
	}
}
