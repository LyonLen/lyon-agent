package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
)

// approvedTool wraps funcTool so it satisfies Approvable.
type approvedTool struct{ *funcTool }

func (a *approvedTool) NeedsApproval() bool { return true }

func TestApprovalDenyBecomesDeclined(t *testing.T) {
	var ran atomic.Bool
	inner := &funcTool{
		name:     "mutating_probe",
		parallel: false,
		fn:       func(context.Context) error { ran.Store(true); return nil },
	}
	at := &approvedTool{inner}

	var starts, ends atomic.Int32
	var endErr atomic.Value
	a := Adapt(at, AdapterConfig{
		Approve: func(context.Context, ExecEvent) bool { return false },
		Observer: ObserverFuncs{
			Start: func(ExecEvent) { starts.Add(1) },
			End:   func(e ExecEvent) { ends.Add(1); endErr.Store(e.Err) },
		},
	})

	_, err := a.Execute(context.Background(), json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "user declined") {
		t.Fatalf("denied execution should error with 'user declined', got %v", err)
	}
	if ran.Load() {
		t.Fatal("tool executed despite denial")
	}
	if starts.Load() != 1 || ends.Load() != 1 {
		t.Fatalf("observer events: start=%d end=%d, want 1/1", starts.Load(), ends.Load())
	}
	if msg, _ := endErr.Load().(string); !strings.Contains(msg, "user declined") {
		t.Fatalf("OnToolEnd err = %q, want decline", msg)
	}
}

func TestApprovalAllowRuns(t *testing.T) {
	var ran atomic.Bool
	inner := &funcTool{name: "probe", fn: func(context.Context) error { ran.Store(true); return nil }}
	at := &approvedTool{inner}

	a := Adapt(at, AdapterConfig{
		Approve: func(context.Context, ExecEvent) bool { return true },
	})
	if _, err := a.Execute(context.Background(), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("approved execution failed: %v", err)
	}
	if !ran.Load() {
		t.Fatal("approved tool did not run")
	}
}

func TestApprovalIgnoresNonApprovableTools(t *testing.T) {
	// funcTool does not implement Approvable: a deny-everything hook must
	// not gate it (read-only tools like read_file stay frictionless).
	var ran atomic.Bool
	plain := &funcTool{name: "read_only", parallel: true, fn: func(context.Context) error { ran.Store(true); return nil }}
	a := Adapt(plain, AdapterConfig{
		Approve: func(context.Context, ExecEvent) bool { return false },
	})
	if _, err := a.Execute(context.Background(), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("plain tool must bypass approval: %v", err)
	}
	if !ran.Load() {
		t.Fatal("plain tool did not run")
	}
}

func TestSharedExecIDsAcrossTools(t *testing.T) {
	var seq atomic.Uint64
	var ids []uint64
	obs := ObserverFuncs{
		End: func(e ExecEvent) { ids = append(ids, e.ExecID) },
	}
	a := Adapt(&funcTool{name: "t1", parallel: true}, AdapterConfig{Seq: &seq, Observer: obs})
	b := Adapt(&funcTool{name: "t2", parallel: true}, AdapterConfig{Seq: &seq, Observer: obs})
	_, _ = a.Execute(context.Background(), json.RawMessage(`{}`))
	_, _ = b.Execute(context.Background(), json.RawMessage(`{}`))
	if len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
		t.Fatalf("exec ids = %v, want [1 2] (session-global counter)", ids)
	}
}
