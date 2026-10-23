package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/toolrunner"

	"lyon-agent/pkg/flow"
	"lyon-agent/pkg/tools"
)

// flowInput is the typed input of the flow tool; its jsonschema tags are the
// schema the model validates against. Action selects the operation so one
// tool covers list (discover what is frozen), run (execute a sequence), and
// freeze (fix the sequence just proven green). Args are named to match the
// frozen script's declared placeholders.
type flowInput struct {
	Action string            `json:"action" jsonschema:"one of list | run | freeze"`
	Slug   string            `json:"slug,omitempty" jsonschema:"the workflow slug (required for run and freeze)"`
	Args   map[string]string `json:"args,omitempty" jsonschema:"named args for run, keyed to the script's declared placeholders"`
	Steps  []string          `json:"steps,omitempty" jsonschema:"ordered shell steps to freeze (freeze only)"`
	Desc   string            `json:"description,omitempty" jsonschema:"one-line description recorded with a freeze"`
}

// flowTool is the composite unit over frozen workflow scripts (pkg/flow):
// the model runs an already-fixed command sequence with ONE call instead of
// re-deriving every step, and fixes a freshly-successful sequence so it stops
// costing tokens. ParallelSafe stays false — a flow drives shell steps, the
// same work run_shell does, and the session serializes it.
type flowTool struct {
	inner anthropic.BetaTool
}

// newFlowTool builds the flow tool. Schema generation failure means the
// struct tags are malformed — a developer error, so it panics loudly.
func newFlowTool() tools.Tool {
	desc := "Run or manage frozen workflow scripts — command sequences fixed after repeated success, " +
		"so they cost no re-derivation. `list` shows frozen slugs (also listed in the system prompt's " +
		"frozen-workflows section). `run` executes a slug's steps in order with named args. `freeze` " +
		"saves a proven command sequence under a slug so it can be re-run with one call later."
	inner, err := toolrunner.NewBetaToolFromJSONSchema[flowInput](
		"flow", desc,
		func(_ context.Context, _ flowInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			// Unreachable: Execute below handles the raw input itself. The
			// runner factory requires a handler; the adapter never calls it.
			return anthropic.BetaToolResultBlockParamContentUnion{}, fmt.Errorf("flow: use Execute")
		},
	)
	if err != nil {
		panic(fmt.Sprintf("agent: flow schema: %v", err))
	}
	return &flowTool{inner: inner}
}

// Name implements tools.Tool.
func (t *flowTool) Name() string { return t.inner.Name() }

// Description implements tools.Tool.
func (t *flowTool) Description() string { return t.inner.Description() }

// InputSchema implements tools.Tool.
func (t *flowTool) InputSchema() anthropic.BetaToolInputSchemaParam { return t.inner.InputSchema() }

// ParallelSafe is false: a flow runs shell steps, the same work run_shell
// does, so it takes the session's serial mutex.
func (t *flowTool) ParallelSafe() bool { return false }

// Execute implements tools.Tool. It unmarshals the action and dispatches; a
// frozen script runs with the session's workspace as cwd, so it is confined
// to the same tree run_shell would be.
func (t *flowTool) Execute(ctx context.Context, input json.RawMessage) ([]anthropic.BetaToolResultBlockParamContentUnion, error) {
	var in flowInput
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, fmt.Errorf("invalid input: %w", err)
	}
	ws := ""
	if sess := SessionFromContext(ctx); sess != nil {
		ws = sess.eng.cfg.Workspace
	}
	var out string
	var err error
	switch strings.TrimSpace(strings.ToLower(in.Action)) {
	case "list":
		out = listFlows()
	case "run":
		out, err = runFlow(ctx, ws, in)
	case "freeze":
		out, err = freezeFlow(ws, in)
	case "":
		return nil, fmt.Errorf("flow: action is required (list | run | freeze)")
	default:
		return nil, fmt.Errorf("flow: unknown action %q (want list | run | freeze)", in.Action)
	}
	if err != nil {
		return nil, err
	}
	return tools.TextResult(out), nil
}

// listFlows renders the frozen slugs, or a hint when none are frozen yet.
func listFlows() string {
	scripts := flow.ListScripts()
	if len(scripts) == 0 {
		return "No frozen workflows yet. Freeze a proven command sequence with action=freeze."
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Frozen workflows (%d):\n", len(scripts)))
	for _, s := range scripts {
		desc := s.Description
		if desc == "" {
			desc = fmt.Sprintf("%d step(s)", len(s.Steps))
		}
		fmt.Fprintf(&b, "- %s: %s (args: %s)\n", s.Slug, desc, strings.Join(s.Args, ", "))
	}
	return b.String()
}

// runFlow executes a frozen slug's steps in order under the session workspace.
func runFlow(ctx context.Context, ws string, in flowInput) (string, error) {
	slug := strings.TrimSpace(in.Slug)
	if slug == "" {
		return "", fmt.Errorf("flow: run needs a slug")
	}
	res, err := flow.RunScript(ctx, slug, in.Args, ws)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	verdict := "FAILED"
	if res.OK {
		verdict = "OK"
	}
	fmt.Fprintf(&b, "flow %s: %s\n", slug, verdict)
	for _, st := range res.Steps {
		fmt.Fprintf(&b, "[%d] $ %s\n    exit=%d\n", st.Index, st.Step, st.Exit)
		if strings.TrimSpace(st.Output) != "" {
			fmt.Fprintf(&b, "    %s\n", st.Output)
		}
		if st.Err != "" {
			fmt.Fprintf(&b, "    err: %s\n", st.Err)
		}
	}
	if !res.OK {
		fmt.Fprintf(&b, "Stopped at the first failing step. Fix it, or re-freeze the corrected sequence with action=freeze.")
	}
	return b.String(), nil
}

// freezeFlow saves a proven step sequence under a slug so it can be re-run
// with one call. It refuses an empty step list — a freeze with nothing in it
// would shadow a real script under the same slug.
func freezeFlow(ws string, in flowInput) (string, error) {
	slug := strings.TrimSpace(in.Slug)
	if slug == "" {
		return "", fmt.Errorf("flow: freeze needs a slug")
	}
	if len(in.Steps) == 0 {
		return "", fmt.Errorf("flow: freeze needs a non-empty steps list")
	}
	for _, s := range in.Steps {
		if strings.TrimSpace(s) == "" {
			return "", fmt.Errorf("flow: freeze rejects an empty step")
		}
	}
	script := flow.Script{
		Slug:        slug,
		Description: strings.TrimSpace(in.Desc),
		Steps:       in.Steps,
		Workspace:   ws,
		Runs:        1,
	}
	if err := flow.FreezeScript(script); err != nil {
		return "", err
	}
	return fmt.Sprintf("Froze workflow %q (%d step(s)). Run it with action=run, slug=%s.", slug, len(in.Steps), slug), nil
}
