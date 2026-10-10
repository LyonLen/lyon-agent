package tools

import (
	"context"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/toolrunner"
)

// schemaFor derives a tool's input schema from struct T using the SDK's own
// jsonschema reflection (same path as toolrunner.NewBetaToolFromJSONSchema),
// so schema semantics never drift from the runner's. The handler is never
// invoked — only the schema is read. Failure means the struct tags are
// malformed, which is a developer error and panics loudly.
func schemaFor[T any]() anthropic.BetaToolInputSchemaParam {
	t, err := toolrunner.NewBetaToolFromJSONSchema[T](
		"_schema_probe",
		"",
		func(context.Context, T) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			return anthropic.BetaToolResultBlockParamContentUnion{}, nil
		},
	)
	if err != nil {
		panic(fmt.Sprintf("tools: schema generation failed for %T: %v", *new(T), err))
	}
	return t.InputSchema()
}
