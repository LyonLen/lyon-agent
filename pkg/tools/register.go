package tools

import (
	"context"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/toolrunner"
)

// RegisterFunc adds a plain Go function to the registry as a typed tool.
// Struct T's jsonschema tags define the input schema and the SDK validates
// incoming tool input against it before fn runs, so malformed calls surface
// as is_error tool results instead of panics. Handler errors propagate the
// same way (the runner formats them as "Error: ...").
//
// parallelSafe should be true for read-only tools: they then run
// concurrently with other safe tools in a turn instead of taking the
// session's serial mutex.
func RegisterFunc[T any](
	r *Registry,
	name, description string,
	parallelSafe bool,
	fn func(ctx context.Context, input T) (string, error),
) error {
	if fn == nil {
		return fmt.Errorf("tools: register %q: nil handler", name)
	}
	inner, err := toolrunner.NewBetaToolFromJSONSchema[T](
		name,
		description,
		func(ctx context.Context, in T) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			out, err := fn(ctx, in)
			if err != nil {
				return anthropic.BetaToolResultBlockParamContentUnion{}, err
			}
			return TextResult(out)[0], nil
		},
	)
	if err != nil {
		return fmt.Errorf("tools: register %q: %w", name, err)
	}
	return r.Register(&fnTool{BetaTool: inner, parallel: parallelSafe})
}

// fnTool adapts an SDK BetaTool (which knows nothing about parallelism) to
// the tools.Tool interface.
type fnTool struct {
	anthropic.BetaTool
	parallel bool
}

// ParallelSafe implements Tool.
func (f *fnTool) ParallelSafe() bool { return f.parallel }
