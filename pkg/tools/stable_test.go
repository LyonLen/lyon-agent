package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// TestAdaptedSchemaMarshalDeterministic pins the prompt-cache fix: the SDK
// iterates schema ExtraFields as a Go map (random order per marshal), so
// without canonicalization the tools array bytes drift every request and the
// cacheable prefix never matches. Marshal one adapted tool definition many
// times and require byte-identical output with the schema content intact.
func TestAdaptedSchemaMarshalDeterministic(t *testing.T) {
	sh := &Shell{}
	at := Adapt(sh, AdapterConfig{})
	def := anthropic.BetaToolUnionParam{
		OfTool: &anthropic.BetaToolParam{
			Name:        "run_shell",
			Description: anthropic.String(sh.Description()),
			InputSchema: at.InputSchema(),
		},
	}

	first, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for i := 1; i < 100; i++ {
		b, err := json.Marshal(def)
		if err != nil {
			t.Fatalf("marshal %d: %v", i, err)
		}
		if string(b) != string(first) {
			t.Fatalf("schema bytes drifted at iteration %d:\n%s\nvs\n%s", i, first, b)
		}
	}

	// Canonicalization must not lose schema content.
	for _, key := range []string{`"$schema"`, `"additionalProperties":false`, `"required":["command"]`, `"command"`} {
		if !strings.Contains(string(first), key) {
			t.Errorf("canonical schema missing %s: %s", key, first)
		}
	}
}

// TestStableSchemaIdempotent: canonicalizing the canonical form is a no-op,
// so tools that pass through Adapt twice (subagent wrapping) stay stable.
func TestStableSchemaIdempotent(t *testing.T) {
	s := schemaFor[shellInput]()
	once := stableSchema(s)
	twice := stableSchema(once)

	a, err := json.Marshal(once)
	if err != nil {
		t.Fatalf("marshal once: %v", err)
	}
	b, err := json.Marshal(twice)
	if err != nil {
		t.Fatalf("marshal twice: %v", err)
	}
	if string(a) != string(b) {
		t.Fatalf("not idempotent:\n%s\nvs\n%s", a, b)
	}
}

// TestStableSchemaWithoutExtras covers the plain path: a schema with no
// extras still round-trips with content preserved and stable bytes.
func TestStableSchemaWithoutExtras(t *testing.T) {
	plain := anthropic.BetaToolInputSchemaParam{
		Properties: map[string]any{"a": map[string]any{"type": "string"}},
		Required:   []string{"a"},
	}
	st := stableSchema(plain)
	first, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for i := 1; i < 20; i++ {
		b, err := json.Marshal(st)
		if err != nil {
			t.Fatalf("marshal %d: %v", i, err)
		}
		if string(b) != string(first) {
			t.Fatalf("drift at %d: %s vs %s", i, first, b)
		}
	}
	if !strings.Contains(string(first), `"a"`) || !strings.Contains(string(first), `"required"`) {
		t.Errorf("content lost: %s", first)
	}
}
