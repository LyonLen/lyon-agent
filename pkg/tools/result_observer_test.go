package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// resObs implements Observer + ResultObserver, recording every callback —
// it embeds bareObs (plain Observer) to prove the extension composes.
type resObs struct {
	bareObs
	results []string
}

func (o *resObs) OnToolResult(e ExecEvent, result string) { o.results = append(o.results, result) }

// TestResultObserverSeesCappedResult: the callback carries the capped
// text — post capResults, byte-identical to what the model receives.
// Failures and empty results never call back: errors and blanks are not
// fixed-point evidence (WORKORDER-runaway-guard §1.7).
func TestResultObserverSeesCappedResult(t *testing.T) {
	long := strings.Repeat("x", 60) + "END" // 63 bytes, cap 10
	o := &resObs{}
	a := Adapt(&funcTool{name: "cap", parallel: true, result: long},
		AdapterConfig{Observer: o, ResultCap: 10})
	out, err := a.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(o.results) != 1 {
		t.Fatalf("OnToolResult calls = %d, want 1", len(o.results))
	}
	got := o.results[0]
	if !strings.HasPrefix(got, strings.Repeat("x", 10)) || !strings.Contains(got, "[truncated") {
		t.Fatalf("observer saw uncapped result: %q", got)
	}
	if model := contentText(out); model != got {
		t.Fatalf("observer text != model text:\n obs   %q\n model %q", got, model)
	}

	// err != nil → no callback.
	o2 := &resObs{}
	a2 := Adapt(&funcTool{name: "boom", parallel: true, fn: func(context.Context) error {
		return errors.New("boom")
	}}, AdapterConfig{Observer: o2, ResultCap: 10})
	if _, err := a2.Execute(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("want tool error")
	}
	if len(o2.results) != 0 {
		t.Fatalf("failed execution called back: %q", o2.results)
	}

	// Whitespace-only result → no callback either.
	o3 := &resObs{}
	a3 := Adapt(&funcTool{name: "blank", parallel: true, result: "   "}, AdapterConfig{Observer: o3})
	if _, err := a3.Execute(context.Background(), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(o3.results) != 0 {
		t.Fatalf("empty result called back: %q", o3.results)
	}
}
