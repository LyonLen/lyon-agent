// Package flow is the mechanical runner from docs/design/agent-evolution.md
// §3: a workflow's control flow is data, not code, and the state machine —
// not the LLM — decides what happens next. Each tick builds one node's
// prompt (the model freely fills the slots inside), judges the result
// against that node's exit criteria (command exit code, output marker, or
// a narrow judge call), and moves on: LLM organizes the words, the state
// machine owns the sequence.
package flow

import (
	"context"
	"fmt"
	"strings"
)

// Exit kinds — the deterministic verdicts of §3.2. Empty reads as
// "always": a node whose product is the statement itself.
const (
	ExitAlways   = "always"   // no criterion — presence of the run is the pass
	ExitCommand  = "command"  // shell probe exits 0
	ExitContains = "contains" // run output carries the marker substring
	ExitJudge    = "judge"    // one narrow裁判 model call on this output only
)

// Fallback policies when a node's attempts are exhausted (the
// "fallback" of the schema). "goto:<name>" jumps elsewhere instead of
// following the sequence — control flow as data.
const (
	FallbackRetry    = "retry"    // diagnose the failure, retry (bounded), then abort
	FallbackAbort    = "abort"    // stop the flow here
	FallbackContinue = "continue" // record the miss, walk on
	GotoPrefix       = "goto:"
)

const (
	// MaxAttempts bounds a retry-fallback node: each miss feeds a
	// diagnosis back in as constraints before the next try (§1.3's
	// 错因归纳 reused as the runner's retry memory).
	MaxAttempts = 3
	// tickBudget caps total ticks so a goto cycle can never spin forever.
	tickBudgetBase = 8
)

// Exit is a node's pass criterion (the exit_criteria of the schema).
type Exit struct {
	Kind     string `json:"kind"`               // always | command | contains | judge
	Command  string `json:"command,omitempty"`  // shell probe: exit 0 passes
	Contains string `json:"contains,omitempty"` // required substring of the run output
	Judge    string `json:"judge,omitempty"`    // the judge's question
}

// Node is one step of the frozen procedure. {{input}} in Prompt is the
// only template slot the state machine fills; everything the model
// produces lives inside the node, never in the control flow.
type Node struct {
	Name     string `json:"name"`
	Tool     string `json:"tool,omitempty"`     // tool this node leans on (hint, schema fidelity)
	Prompt   string `json:"prompt"`             // instruction template; {{input}} = the flow's task
	Exit     Exit   `json:"exit"`               // pass criterion
	Fallback string `json:"fallback,omitempty"` // retry (default) | abort | continue | goto:<name>
	OnPass   string `json:"on_pass,omitempty"`  // jump target after pass; empty = next in sequence
}

// Workflow is one frozen procedure: an ordered node list plus the
// trigger keywords Match scores tasks against.
type Workflow struct {
	Slug    string   `json:"slug"`
	Title   string   `json:"title"`
	Trigger []string `json:"trigger"`
	Nodes   []Node   `json:"nodes"`
}

// Deps are the runner's contact with the outside world — injected so
// the state machine is fully testable offline. Production wiring (the
// CLI flag, /flow run) supplies a real Session drive for Run, the
// workspace shell for Shell, and a cheap model for Judge/Diagnose.
type Deps struct {
	// Run executes one node's prompt (the LLM filling its slots).
	Run func(ctx context.Context, prompt string) (string, error)
	// Shell probes a command for an exit-code verdict.
	Shell func(ctx context.Context, command string) (output string, code int, err error)
	// Judge answers one narrow question about a node's output.
	Judge func(ctx context.Context, question, output string) (bool, error)
	// Diagnose post-mortems a failed attempt and returns the constraint
	// block to append before the retry ("" when nothing actionable).
	Diagnose func(ctx context.Context, prompt, output, fail string) (string, error)
	// Gate is the §2 knowledge-coverage pre-flight (may be nil): run once
	// before the first tick against the task. ok = passed the threshold;
	// inject = the gap lines to fold into the task when it missed. A gate
	// error never blocks the run — a broken quiz degrades to no quiz.
	Gate func(ctx context.Context, task string) (ok bool, inject []string, err error)
	// Step reports progress (each attempt's verdict) — the TUI's panel
	// rows and the plain REPL's status lines hang off it. May be nil.
	Step func(node string, attempt, max int, ok bool, note string)
}

// Result is one flow run, truthful about partial completion.
type Result struct {
	Slug       string            `json:"slug"`
	OK         bool              `json:"ok"` // every node passed
	FailedNode string            `json:"failed_node,omitempty"`
	Failed     []string          `json:"failed,omitempty"` // nodes missed under continue/give-up
	Attempts   int               `json:"attempts"`         // total node runs
	Outputs    map[string]string `json:"outputs,omitempty"`
	Err        string            `json:"error,omitempty"` // transport / context failure
	// Gated marks that the coverage pre-flight ran; Injected carries the
	// gap lines folded into {{input}} when it missed (design §2.2 —
	// 定向注入). GateNote records a skipped/broken gate, never fatal.
	Gated    bool     `json:"gated,omitempty"`
	Injected []string `json:"injected,omitempty"`
	GateNote string   `json:"gate_note,omitempty"`
}

// indexOf finds a node by name (-1 when absent).
func (w *Workflow) indexOf(name string) int {
	for i, n := range w.Nodes {
		if n.Name == name {
			return i
		}
	}
	return -1
}

// Execute walks the workflow: tick by tick, the state machine picks the
// node, the model fills it, the exit criteria judge it. The model never
// chooses the next node — that is the whole point of §3.2.
func Execute(ctx context.Context, w *Workflow, input string, deps Deps) Result {
	res := Result{Slug: w.Slug, Outputs: map[string]string{}}
	if len(w.Nodes) == 0 {
		res.OK = true
		return res
	}
	// Knowledge-coverage pre-flight (§2): a miss injects the gap lines
	// into the task, so every node's {{input}} carries them — the
	// mechanical twin of PushSystemNote injection at the session layer.
	if deps.Gate != nil {
		ok, inject, err := deps.Gate(ctx, input)
		switch {
		case err != nil:
			res.GateNote = "gate skipped: " + err.Error()
		default:
			res.Gated = true
			if !ok {
				res.Injected = inject
				if len(inject) > 0 {
					input += "\n\nKnowledge gaps from the pre-flight coverage quiz — answer every node with these in mind:\n- " +
						strings.Join(inject, "\n- ")
					res.GateNote = "coverage below threshold — gaps injected"
				} else {
					res.GateNote = "coverage below threshold — nothing to inject"
				}
			}
		}
	}
	budget := len(w.Nodes)*(MaxAttempts+1) + tickBudgetBase

	report := func(node string, attempt, max int, ok bool, note string) {
		if deps.Step != nil {
			deps.Step(node, attempt, max, ok, note)
		}
	}

	idx := 0
	for tick := 0; tick < budget; {
		if err := ctx.Err(); err != nil {
			res.Err = err.Error()
			return res
		}
		tick++
		node := w.Nodes[idx]

		maxAtt := 1
		fb := node.Fallback
		if fb == "" {
			fb = FallbackRetry
		}
		if fb == FallbackRetry {
			maxAtt = MaxAttempts
		}

		prompt := strings.ReplaceAll(node.Prompt, "{{input}}", input)
		var constraints []string
		out, fail, passed := "", "", false

		for attempt := 1; attempt <= maxAtt; attempt++ {
			if err := ctx.Err(); err != nil {
				res.Err = err.Error()
				return res
			}
			p := prompt
			if len(constraints) > 0 {
				p += "\n\nConstraints from previous failed attempts:\n- " +
					strings.Join(constraints, "\n- ")
			}
			runOut, err := deps.Run(ctx, p)
			out = runOut
			res.Attempts++
			res.Outputs[node.Name] = runOut
			if err != nil {
				fail = "run failed: " + err.Error()
			} else {
				passed, fail = judge(ctx, node.Exit, runOut, deps)
			}
			report(node.Name, attempt, maxAtt, passed, fail)
			if passed {
				break
			}
			if attempt == maxAtt || fb != FallbackRetry {
				break
			}
			// Bounded diagnose-retry: feed the post-mortem back in.
			if deps.Diagnose != nil {
				if patch, derr := deps.Diagnose(ctx, prompt, out, fail); derr == nil && patch != "" {
					constraints = append(constraints, patch)
				}
			}
		}

		if passed {
			if node.OnPass != "" {
				next := w.indexOf(node.OnPass)
				if next < 0 {
					res.FailedNode = node.Name
					res.Failed = append(res.Failed, node.Name)
					res.Err = fmt.Sprintf("on_pass target %q not found", node.OnPass)
					return res
				}
				idx = next
			} else {
				idx++
			}
			if idx >= len(w.Nodes) {
				res.OK = len(res.Failed) == 0
				return res
			}
			continue
		}

		// Node failed for good — the fallback decides control flow.
		report(node.Name, 0, 0, false, "giving up: "+fail)
		res.Failed = append(res.Failed, node.Name)
		switch {
		case fb == FallbackContinue || strings.HasPrefix(fb, GotoPrefix):
			if strings.HasPrefix(fb, GotoPrefix) {
				next := w.indexOf(strings.TrimPrefix(fb, GotoPrefix))
				if next < 0 {
					res.FailedNode = node.Name
					res.Err = fmt.Sprintf("goto target %q not found", strings.TrimPrefix(fb, GotoPrefix))
					return res
				}
				idx = next
			} else {
				idx++
			}
			if idx >= len(w.Nodes) {
				res.OK = len(res.Failed) == 0
				return res
			}
		default: // abort — and a retry fallback that exhausted its budget
			res.FailedNode = node.Name
			return res
		}
	}
	res.FailedNode = w.Nodes[min(idx, len(w.Nodes)-1)].Name
	res.Err = "tick budget exhausted (goto cycle?)"
	return res
}

// judge applies one node's exit criterion. fail is "" on pass, a
// human-readable verdict on miss (what the panel row and the diagnoser
// see).
func judge(ctx context.Context, ex Exit, out string, deps Deps) (bool, string) {
	kind := ex.Kind
	if kind == "" {
		kind = ExitAlways
	}
	switch kind {
	case ExitAlways:
		return true, ""
	case ExitCommand:
		if ex.Command == "" {
			return true, "" // probe-less command node: nothing to ask
		}
		probeOut, code, err := deps.Shell(ctx, ex.Command)
		switch {
		case err != nil:
			return false, fmt.Sprintf("probe %q failed: %v", ex.Command, err)
		case code != 0:
			return false, fmt.Sprintf("probe %q exit %d: %s", ex.Command, code, clip(probeOut))
		}
		return true, ""
	case ExitContains:
		if ex.Contains == "" {
			return true, ""
		}
		if !strings.Contains(out, ex.Contains) {
			return false, fmt.Sprintf("output missing marker %q", ex.Contains)
		}
		return true, ""
	case ExitJudge:
		if ex.Judge == "" {
			return true, ""
		}
		ok, err := deps.Judge(ctx, ex.Judge, out)
		if err != nil {
			return false, "judge failed: " + err.Error()
		}
		if !ok {
			return false, "judge said no: " + ex.Judge
		}
		return true, ""
	default:
		return false, fmt.Sprintf("unknown exit kind %q", kind)
	}
}

// clip keeps probe output inside a verdict line.
func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 160 {
		return string(r[:160]) + "…"
	}
	return s
}

// Match picks the workflow whose trigger keywords cover the text best —
// distinct trigger words present ÷ len(trigger), at least one hit
// required (the coarse scorer of §3.1; the cheap-model confirmation
// layer hangs off this later). Ties keep the earlier workflow.
func Match(wfs []Workflow, text string) (Workflow, bool) {
	lower := strings.ToLower(text)
	best, bestScore := Workflow{}, 0.0
	for _, wf := range wfs {
		if len(wf.Trigger) == 0 {
			continue
		}
		hits := 0
		for _, t := range wf.Trigger {
			if t = strings.ToLower(strings.TrimSpace(t)); t != "" &&
				strings.Contains(lower, t) {
				hits++
			}
		}
		if hits == 0 {
			continue
		}
		if score := float64(hits) / float64(len(wf.Trigger)); score > bestScore {
			best, bestScore = wf, score
		}
	}
	return best, bestScore > 0
}
