package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/toolrunner"

	"lyon-agent/pkg/tools"
)

// maxDispatchTasks bounds one fan-out: enough for a real decomposition,
// few enough that a single tool call cannot open an unbounded storm of
// API loops (the turn semaphore still bounds in-flight work within it).
const maxDispatchTasks = 6

// mergeMinBytes gates the consolidation pass: below this the concat is
// already small enough to read as-is, and an extra cheap-tier call would
// cost more than the context it saves.
const mergeMinBytes = 8 << 10

// mergeMaxTokens bounds the consolidated reply — a tight digest of the
// findings, not a second copy of them.
const mergeMaxTokens = 4000

// mergeBudget bounds the consolidation call end to end; a var (not const)
// so tests can shrink it onto the fail-open path.
var mergeBudget = 30 * time.Second

// mergeSystemPrompt instructs the consolidation pass: keep every fact,
// cite the source section, never hide a failure.
const mergeSystemPrompt = `You consolidate concurrent subagent reports into one answer for the driving agent.

Input: sections headed "## i/N <task>", each holding that subtask's report (or a "failed: …" line).

Write the consolidated result:
- Preserve every factual finding VERBATIM — numbers, names, quotes, paths, code. Do not paraphrase detail away.
- Attribute claims with their source section number in brackets, e.g. [1], [2].
- Keep every "failed:" line visible — failures are data the caller needs.
- Where sections conflict, present both readings with their [i] citations; do not silently pick one.
- No preamble about merging or summarizing; start directly with the findings.`

// dispatchTask is one subtask of a leader's decomposition.
type dispatchTask struct {
	Task    string `json:"task" jsonschema:"self-contained subtask one agent can finish alone"`
	Context string `json:"context,omitempty" jsonschema:"optional background or data this subtask needs"`
}

// dispatchInput is the typed input of run_subagents; its jsonschema tags
// are the schema the model validates against.
type dispatchInput struct {
	Tasks []dispatchTask `json:"tasks" jsonschema:"2-6 independent subtasks — every entry runs concurrently"`
}

// dispatchTool is run_subagents: the leader's decomposition fan-out (#53,
// and the 并发倾向 requirement — Go-style maximal concurrency is the
// product stance; a serial dispatch would be the defect). One call spawns
// every subtask as its own nested loop in parallel, each on the model
// its task class has evidence for.
type dispatchTool struct {
	inner anthropic.BetaTool
}

// newDispatchTool builds run_subagents. Schema generation failure means
// the struct tags are malformed — a developer error, so it panics loudly.
func newDispatchTool() tools.Tool {
	desc := "Decompose the work into 2-6 INDEPENDENT subtasks and run them ALL CONCURRENTLY in one call, " +
		"each handled by its own read-only research subagent on the best model for that subtask's class. " +
		"Use for parallel fan-out (independent lookups, comparisons, multi-file surveys) — the results come " +
		"back merged in task order with per-subtask usage. Only subtasks that do not depend on each other's " +
		"output; for dependent work use run_subagent. The subagents have file reads, web search/fetch and " +
		"other safe read-only tools."
	inner, err := toolrunner.NewBetaToolFromJSONSchema[dispatchInput](
		"run_subagents", desc,
		func(ctx context.Context, _ dispatchInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			// Unreachable: Execute below handles the raw input itself so it
			// can reach the driving session. The runner factory requires a
			// handler; the adapter calls this only if Execute were replaced.
			return anthropic.BetaToolResultBlockParamContentUnion{}, fmt.Errorf("run_subagents: use Execute")
		},
	)
	if err != nil {
		panic(fmt.Sprintf("agent: run_subagents schema: %v", err))
	}
	return &dispatchTool{inner: inner}
}

// Name implements tools.Tool.
func (t *dispatchTool) Name() string { return t.inner.Name() }

// Description implements tools.Tool.
func (t *dispatchTool) Description() string { return t.inner.Description() }

// InputSchema implements tools.Tool.
func (t *dispatchTool) InputSchema() anthropic.BetaToolInputSchemaParam { return t.inner.InputSchema() }

// ParallelSafe is false: the fan-out already opens its own concurrency —
// the session's serial tools must not overlap it.
func (t *dispatchTool) ParallelSafe() bool { return false }

// Execute implements tools.Tool.
func (t *dispatchTool) Execute(ctx context.Context, input json.RawMessage) ([]anthropic.BetaToolResultBlockParamContentUnion, error) {
	sess, ok := ctx.Value(sessionCtxKey{}).(*Session)
	if !ok || sess == nil {
		return nil, fmt.Errorf("run_subagents: no driving session in context")
	}
	return sess.dispatch(ctx, input)
}

// dispatch runs every subtask concurrently — one goroutine per subtask,
// bounded by maxDispatchTasks and the engine's turn semaphore — and
// merges the answers in task order. Each subtask rides the routing
// table's model when its class has evidence (Config.RouteFor), else the
// subagent tier: the leader decides WHAT to split, evidence decides WHO
// answers (#53 leader 拆解派活 + 平均分定任务分配).
func (s *Session) dispatch(ctx context.Context, input json.RawMessage) ([]anthropic.BetaToolResultBlockParamContentUnion, error) {
	var args dispatchInput
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, fmt.Errorf("run_subagents: invalid input: %w", err)
	}
	if len(args.Tasks) == 0 {
		return nil, fmt.Errorf("run_subagents: at least one task is required")
	}
	if len(args.Tasks) > maxDispatchTasks {
		return nil, fmt.Errorf("run_subagents: %d tasks exceeds the fan-out cap of %d",
			len(args.Tasks), maxDispatchTasks)
	}
	tasks := make([]string, len(args.Tasks))
	for i, t := range args.Tasks {
		tasks[i] = strings.TrimSpace(t.Task)
		if tasks[i] == "" {
			return nil, fmt.Errorf("run_subagents: task %d is empty", i+1)
		}
	}

	runID := s.disSeq.Add(1)
	type subResult struct {
		text  string
		usage Usage
		iters int
		err   error
	}
	results := make([]subResult, len(args.Tasks))

	var wg sync.WaitGroup
	for i := range args.Tasks {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			t := args.Tasks[i]
			task := tasks[i]
			if c := strings.TrimSpace(t.Context); c != "" {
				task += "\n\nContext:\n" + c
			}
			model := s.eng.subagentModel()
			if s.eng.cfg.RouteFor != nil {
				if m := strings.TrimSpace(s.eng.cfg.RouteFor(t.Task)); m != "" {
					model = m
				}
			}
			// Token: unique per task of this run — events, step nodes and
			// inner executions all pair on it (#53 attribution).
			subID := fmt.Sprintf("d%d.%d", runID, i)
			preview := clipRunes(strings.Join(strings.Fields(t.Task), " "), 56)
			text, usage, iters, err := s.runNested(ctx, model, task, subID, preview)
			results[i] = subResult{text: text, usage: usage, iters: iters, err: err}
		}(i)
	}
	wg.Wait()

	fails := 0
	okResults := 0
	seenBody := make(map[string]int, len(results)) // body → first task (1-based)
	var b strings.Builder
	for i, r := range results {
		t := args.Tasks[i]
		preview := clipRunes(strings.Join(strings.Fields(t.Task), " "), 56)
		fmt.Fprintf(&b, "## %d/%d %s\n", i+1, len(results), preview)
		switch {
		case r.err != nil:
			fails++
			fmt.Fprintf(&b, "failed: %v\n", r.err)
		default:
			okResults++
			// Rules-layer dedupe: subtasks handed the same context often
			// answer with byte-identical text — the second copy only
			// burns context, so it becomes a pointer back to the first.
			body := strings.TrimRight(r.text, "\n")
			switch {
			case body == "":
				b.WriteString("(no output)")
			default:
				if first, dup := seenBody[body]; dup {
					fmt.Fprintf(&b, "(identical to task %d)", first)
				} else {
					seenBody[body] = i + 1
					b.WriteString(body)
				}
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	if fails == len(results) {
		// "all N failed" with no cause is undiagnosable — fold each task's
		// error in so the model (and the user) sees WHY the fan-out died.
		causes := make([]string, 0, fails)
		for _, r := range results {
			if r.err != nil {
				causes = append(causes, r.err.Error())
			}
		}
		return nil, fmt.Errorf("run_subagents: all %d subtasks failed: %s",
			len(results), strings.Join(causes, " | "))
	}
	raw := strings.TrimRight(b.String(), "\n")
	// Consolidation pass (⑧): a wide fan-out of verbose reports reads
	// better as one cited digest — gated, bounded, fail-open. Any miss
	// (too few ok results, small concat, API error, useless reply) keeps
	// the raw concat, which was the status quo anyway.
	if merged := s.mergeSubresults(ctx, raw, okResults); merged != "" {
		return tools.TextResult(merged), nil
	}
	return tools.TextResult(raw), nil
}

// mergeSubresults is the consolidation pass: one cheap-tier Ask over the
// raw concat. Fail-open at every gate — the caller can never be blocked
// or broken by a merge that does not clearly beat the concat.
func (s *Session) mergeSubresults(ctx context.Context, raw string, okResults int) string {
	if okResults < 3 || len(raw) < mergeMinBytes {
		return "" // the concat is already small enough to read as-is
	}
	mctx, cancel := context.WithTimeout(ctx, mergeBudget)
	defer cancel()
	merged, err := s.Ask(mctx, mergeSystemPrompt, raw, mergeMaxTokens)
	merged = strings.TrimSpace(merged)
	// Only a reply that actually beats the concat wins: an error, an
	// empty reply, or a "digest" not shorter than the original all keep
	// the original — paying for a copy that saves nothing is the defect.
	if err != nil || merged == "" || len(merged) >= len(raw) {
		return ""
	}
	return merged
}
