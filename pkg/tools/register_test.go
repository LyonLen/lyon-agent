package tools

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
)

type addInput struct {
	A int `json:"a" jsonschema:"first operand"`
	B int `json:"b" jsonschema:"second operand"`
}

func TestRegisterFuncRoundTrip(t *testing.T) {
	reg := NewRegistry()
	err := RegisterFunc(reg, "add", "Add two numbers.", true,
		func(ctx context.Context, in addInput) (string, error) {
			return strconv.Itoa(in.A + in.B), nil
		})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	tool, ok := reg.Get("add")
	if !ok {
		t.Fatal("add not registered")
	}
	if !tool.ParallelSafe() {
		t.Error("parallelSafe flag lost")
	}
	props, ok := tool.InputSchema().Properties.(map[string]any)
	if !ok || len(props) != 2 {
		t.Fatalf("schema properties = %#v, want a and b", tool.InputSchema().Properties)
	}

	out, err := tool.Execute(context.Background(), []byte(`{"a":2,"b":40}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(out) != 1 || out[0].OfText == nil || out[0].OfText.Text != "42" {
		t.Fatalf("result: %+v", out)
	}
}

func TestRegisterFuncWrongInputType(t *testing.T) {
	reg := NewRegistry()
	if err := RegisterFunc(reg, "add", "", false,
		func(ctx context.Context, in addInput) (string, error) { return "", nil }); err != nil {
		t.Fatalf("register: %v", err)
	}
	tool, _ := reg.Get("add")
	// b as a string violates the schema → the SDK's typed handler rejects it.
	if _, err := tool.Execute(context.Background(), []byte(`{"a":1,"b":"x"}`)); err == nil {
		t.Fatal("schema violation must become an error")
	}
}

func TestRegisterFuncHandlerErrorPropagates(t *testing.T) {
	reg := NewRegistry()
	_ = RegisterFunc(reg, "flaky", "", true,
		func(ctx context.Context, in addInput) (string, error) {
			return "", errors.New("business rule violated")
		})
	tool, _ := reg.Get("flaky")
	_, err := tool.Execute(context.Background(), []byte(`{"a":1,"b":2}`))
	if err == nil || !strings.Contains(err.Error(), "business rule violated") {
		t.Fatalf("handler error lost: %v", err)
	}
}

func TestRegisterFuncDuplicateRejected(t *testing.T) {
	reg := NewRegistry()
	fn := func(ctx context.Context, in addInput) (string, error) { return "", nil }
	if err := RegisterFunc(reg, "dup", "", true, fn); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := RegisterFunc(reg, "dup", "", true, fn); err == nil {
		t.Fatal("duplicate name must be rejected")
	}
}
