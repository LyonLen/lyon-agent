package tools

import (
	"context"
	"encoding/json"
	"testing"
)

func TestRegistryRegisterAndAdaptSorted(t *testing.T) {
	r := NewRegistry()
	for _, name := range []string{"zeta", "alpha", "mid"} {
		if err := r.Register(&funcTool{name: name, parallel: true}); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	if err := r.Register(&funcTool{name: "alpha"}); err == nil {
		t.Fatal("duplicate registration should fail")
	}

	names := r.Names()
	want := []string{"alpha", "mid", "zeta"}
	if len(names) != 3 || names[0] != want[0] || names[1] != want[1] || names[2] != want[2] {
		t.Fatalf("names = %v, want %v", names, want)
	}

	adapted := r.Adapt(AdapterConfig{})
	if len(adapted) != 3 {
		t.Fatalf("adapted = %d tools", len(adapted))
	}
	for i, tool := range adapted {
		if tool.Name() != want[i] {
			t.Fatalf("adapted[%d] = %s, want %s", i, tool.Name(), want[i])
		}
		raw, err := json.Marshal(tool.InputSchema())
		if err != nil || string(raw) == "{}" {
			t.Fatalf("tool %s has empty schema: %s (err=%v)", tool.Name(), raw, err)
		}
	}
}

func TestBuiltinSchemasReflectTags(t *testing.T) {
	sh := &Shell{}
	schema := sh.InputSchema()
	raw, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(raw) || string(raw) == "{}" {
		t.Fatalf("shell schema not generated: %s", raw)
	}

	// The required marker from the jsonschema tag must survive.
	var decoded struct {
		Required   []string       `json:"required"`
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Required) == 0 || decoded.Required[0] != "command" {
		t.Fatalf("required = %v, want [command]", decoded.Required)
	}
	if _, ok := decoded.Properties["command"]; !ok {
		t.Fatalf("properties missing command: %v", decoded.Properties)
	}
}

func TestAdaptObserverReceivesEvents(t *testing.T) {
	var started, finished int
	obs := ObserverFuncs{
		Start: func(e ExecEvent) { started++ },
		End:   func(e ExecEvent) { finished++ },
	}
	a := Adapt(&funcTool{name: "probe", parallel: true, result: "hi"}, AdapterConfig{Observer: obs})
	if _, err := a.Execute(context.Background(), json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if started != 1 || finished != 1 {
		t.Fatalf("started=%d finished=%d, want 1/1", started, finished)
	}
}
