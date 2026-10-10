package tools

import (
	"bytes"
	"encoding/json"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

// stableSchema rewrites a tool input schema into a byte-stable form.
//
// The SDK marshals a param's ExtraFields ($schema, additionalProperties, ...)
// by iterating a Go map, whose order is randomized on every marshal. A
// schema carrying extras therefore produces different bytes on each request —
// and because the tools array is the very first thing in the request body,
// any change there invalidates the whole prompt-cache prefix. Tools with
// extras were silently paying full input price on every turn.
//
// Canonicalizing once here fixes it: marshal (order irrelevant), parse into
// a map, re-marshal with encoding/json's sorted keys, and hand the result to
// the SDK as a param override. The override branch bypasses ExtraFields
// entirely, so every later marshal emits exactly these canonical bytes.
// Content is preserved verbatim — including $schema and additionalProperties.
func stableSchema(s anthropic.BetaToolInputSchemaParam) anthropic.BetaToolInputSchemaParam {
	raw, err := json.Marshal(s)
	if err != nil {
		return s // unreachable for schemas we build; keep the original
	}
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // keep numeric keywords verbatim instead of float64 round-tripping
	if err := dec.Decode(&m); err != nil {
		return s
	}
	canon, err := json.Marshal(m) // sorts map keys: one canonical byte sequence
	if err != nil {
		return s
	}
	return param.Override[anthropic.BetaToolInputSchemaParam](json.RawMessage(canon))
}
