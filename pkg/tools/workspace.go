package tools

import "context"

// WorkspaceFunc resolves the live workspace root of the executing session.
// A session's workspace can change mid-life (per-session workspace
// selection), so tools resolve the root per execution instead of freezing
// the engine root at registration time.
type WorkspaceFunc func() string

type workspaceFnKey struct{}

// WithWorkspaceFunc installs the resolver on the tool-execution context.
func WithWorkspaceFunc(ctx context.Context, f WorkspaceFunc) context.Context {
	return context.WithValue(ctx, workspaceFnKey{}, f)
}

// WorkspaceRoot returns the live workspace root for ctx, falling back to
// the configured root (engine-level callers, tests, executions without a
// session) when no resolver is installed or it answers empty.
func WorkspaceRoot(ctx context.Context, fallback string) string {
	if f, ok := ctx.Value(workspaceFnKey{}).(WorkspaceFunc); ok && f != nil {
		if ws := f(); ws != "" {
			return ws
		}
	}
	return fallback
}
