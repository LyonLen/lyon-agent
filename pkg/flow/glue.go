package flow

import (
	"context"
	"fmt"
	"strings"

	"lyon-agent/pkg/eval"
)

// Glue between the mechanical runner (§3) and the evaluation package
// (§1.3 / §2): adapters that turn eval's one-shot call shapes into the
// Deps hooks. The wiring layer (TUI/CLI) closes these over Session.Ask —
// a cheap subagent-tier completion — and stays free of prompt plumbing.

// NewGate runs the §2 closed-book knowledge quiz as the flow's
// pre-flight: generate the checklist, answer it blind, score it. Below
// eval.CoverageThreshold the returned inject lines are the gap items
// for定向注入.
func NewGate(gen eval.ChecklistGen, book eval.Book) func(ctx context.Context, task string) (bool, []string, error) {
	return func(ctx context.Context, task string) (bool, []string, error) {
		cov, inject, err := eval.RunCoverage(ctx, gen, book, task)
		if err != nil {
			return false, nil, err
		}
		return cov.GatePass(), inject, nil
	}
}

// NewDiagnose adapts eval's failure post-mortem into the runner's
// retry hook (§1.3 feeding §3): trajectory in (node prompt + attempt
// output + verdict), constraint block out. An unparsable reply yields
// "" — the retry still happens, just without fresh constraints.
func NewDiagnose(ask eval.Asker) func(ctx context.Context, prompt, output, fail string) (string, error) {
	return func(ctx context.Context, prompt, output, fail string) (string, error) {
		var b strings.Builder
		fmt.Fprintf(&b, "Node prompt:\n%s\n\nAttempt output:\n%s\n\nFailure:\n%s",
			prompt, output, fail)
		d, err := eval.SummarizeFailure(ctx, ask, b.String())
		if err != nil {
			return "", err
		}
		return d.Constraints(), nil
	}
}
