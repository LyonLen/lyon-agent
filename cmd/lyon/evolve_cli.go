package main

// The CLI half of the evolution wiring: -exam runs the §1.4 closed-book
// test, -coverage runs the §2 pre-flight and injects gaps, and -flow
// drives a frozen workflow through the mechanical runner — each attempt
// scored into the leaderboard and the routing table so command-line runs
// feed the same evidence the TUI does.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/eval"
	"lyon-agent/pkg/flow"
	"lyon-agent/pkg/routing"
	"lyon-agent/pkg/tools"
)

// drainText runs one prompt like drain, but also collects the assistant
// text — the flow runner's node fill. Events still stream to stdout so
// a flow run shows its tools working.
func drainText(sess *agent.Session) (string, error) {
	var b strings.Builder
	for ev := range sess.Events() {
		if ev.Type == agent.EventPromptEnd {
			render(ev)
			if ev.Err != "" {
				return b.String(), errors.New(ev.Err)
			}
			return b.String(), nil
		}
		render(ev)
		if ev.Type == agent.EventTextDelta {
			b.WriteString(ev.Text)
		}
	}
	return b.String(), errors.New("session closed before prompt_end")
}

// runExamCLI takes the §1.4 closed-book exam for a class and files the
// score (leaderboard clock + routing evidence), exactly like the TUI's
// /exam. Returns the process code: 0 pass, 1 fail, 2 setup error.
func runExamCLI(sess *agent.Session, model, class string) int {
	ex, err := eval.LoadOrSeedExam(class)
	if err != nil {
		fmt.Fprintln(os.Stderr, "exam:", err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	res := eval.RunExam(ctx, ex, func(ctx context.Context, q string) (string, error) {
		return sess.Ask(ctx, eval.ClosedBookSystem, q, examMaxTokens)
	})
	fmt.Println(paint(cCyan, "✎ exam"), res.Summary())
	for _, a := range res.Answers {
		if !a.OK {
			fmt.Println("  "+paint(cRed, "✗"), oneLine(a.Q))
		}
	}
	lb := eval.OpenLeaderboard()
	demoted := lb.SubmitExam(class, eval.Scorecard{
		Agent: model, Total: res.Pct, Passed: res.Passed, Source: "exam",
	})
	routing.Open().Record(class, model, model, res.Pct)
	if demoted {
		fmt.Println(paint(cRed, "  demoted after two failures — the seat is open"))
	}
	if res.Passed {
		return 0
	}
	return 1
}

// gatePromptCLI runs the §2 coverage pre-flight over a plain prompt and
// returns the prompt to actually send: gaps folded in when the blind
// answers missed (§2.2 定向注入). A broken gate prints a note and sends
// the prompt untouched — never fatal.
func gatePromptCLI(sess *agent.Session, prompt string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	gen := func(ctx context.Context, task string) (string, error) {
		return sess.Ask(ctx, eval.GenSystem, task, flowSaveMaxTokens)
	}
	book := func(ctx context.Context, qs []string) (string, error) {
		return sess.Ask(ctx, eval.BookSystem,
			"- "+strings.Join(qs, "\n- "), flowSaveMaxTokens)
	}
	cov, inject, err := eval.RunCoverage(ctx, gen, book, prompt)
	if err != nil {
		fmt.Println(paint(cYellow, "coverage: gate skipped — "+err.Error()))
		return prompt
	}
	fmt.Printf("%s %.0f%% (%d/%d items)\n",
		paint(cCyan, "coverage"), cov.Score*100, cov.Hits, cov.Items)
	if cov.GatePass() {
		return prompt
	}
	if len(inject) == 0 {
		fmt.Println(paint(cYellow, "  below threshold — nothing to inject"))
		return prompt
	}
	fmt.Printf("  %d gap(s) injected:\n", len(inject))
	for _, g := range inject {
		fmt.Println("    - " + g)
	}
	return prompt + "\n\nKnowledge gaps from the pre-flight coverage quiz — answer with these in mind:\n- " +
		strings.Join(inject, "\n- ")
}

// runFlowCLI drives one frozen workflow: the state machine walks the
// nodes (design §3), every attempt prints a Step line, and the result
// scores into the leaderboard + routing table as a "flow" production
// record. Returns 0 when every node passed.
func runFlowCLI(sess *agent.Session, cfg agent.Config, slug, prompt string) int {
	wfs, err := flow.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "flow:", err)
		return 2
	}
	w, ok := findFlow(wfs, slug)
	if !ok {
		fmt.Fprintf(os.Stderr, "flow: no workflow %q\n", slug)
		var names []string
		for _, x := range wfs {
			names = append(names, x.Slug)
		}
		fmt.Fprintln(os.Stderr, "  available:", strings.Join(names, ", "))
		return 2
	}
	fmt.Printf("%s %s — %d nodes\n",
		paint(cBold, "▶ flow"), w.Slug, len(w.Nodes))

	sh := &tools.Shell{Root: cfg.Workspace, Allow: cfg.ShellAllow}
	ask := func(ctx context.Context, system, user string) (string, error) {
		return sess.Ask(ctx, system, user, 1000)
	}
	deps := flow.Deps{
		Run: func(ctx context.Context, nodePrompt string) (string, error) {
			if err := sess.Submit(nodePrompt); err != nil {
				return "", err
			}
			return drainText(sess)
		},
		Shell: sh.Run,
		Judge: func(ctx context.Context, question, out string) (bool, error) {
			reply, err := sess.Ask(ctx,
				"You are a strict judge. Answer exactly yes or no.",
				question+"\n\nOutput:\n"+truncateRunes(out, 4000), 64)
			if err != nil {
				return false, err
			}
			return strings.HasPrefix(strings.ToLower(strings.TrimSpace(reply)), "yes"), nil
		},
		Diagnose: flow.NewDiagnose(ask),
		Gate: flow.NewGate(
			func(ctx context.Context, task string) (string, error) {
				return sess.Ask(ctx, eval.GenSystem, task, flowSaveMaxTokens)
			},
			func(ctx context.Context, qs []string) (string, error) {
				return sess.Ask(ctx, eval.BookSystem,
					"- "+strings.Join(qs, "\n- "), flowSaveMaxTokens)
			},
		),
		Step: func(node string, attempt, max int, ok bool, note string) {
			mark := paint(cGreen, "✓")
			if !ok {
				mark = paint(cYellow, "✗")
			}
			suffix := ""
			if note != "" {
				suffix = "  " + paint(cDim, truncateRunes(note, 90))
			}
			fmt.Printf("  %s %-14s attempt %d/%d%s\n",
				mark, node, attempt, max, suffix)
		},
	}
	if strings.EqualFold(os.Getenv("LYON_FLOW_GATE"), "off") {
		deps.Gate = nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	res := flow.Execute(ctx, &w, prompt, deps)

	// Result summary.
	switch {
	case res.Err != "":
		fmt.Println(paint(cRed, "✗ flow error: "+res.Err))
	case res.OK:
		fmt.Println(paint(cGreen, fmt.Sprintf("✓ flow done — %d attempts", res.Attempts)))
	default:
		fmt.Println(paint(cRed, fmt.Sprintf("✗ flow failed at node %q (%d attempts)",
			res.FailedNode, res.Attempts)))
	}
	if res.Gated {
		fmt.Println("  " + paint(cDim, "gate: "+res.GateNote))
	}

	// Score it: correctness = share of nodes that ended green.
	// Efficiency compares attempts to the node count — retries cost.
	total := len(w.Nodes)
	passed := total - len(res.Failed)
	ratio := 0.0
	if total > 0 && res.Err == "" {
		ratio = float64(passed) / float64(total)
	}
	class := string(routing.Classify(prompt))
	sc := eval.ScoreWith(cfg.Model, class, "flow", eval.RunMetrics{
		Correctness:   ratio,
		Steps:         res.Attempts,
		BaselineSteps: total,
	}, eval.DefaultThresholds)
	eval.OpenLeaderboard().Record(class, sc)
	routing.Open().Record(class, cfg.Model, cfg.Model, sc.Total)
	fmt.Printf("  score %.2f (correctness %.0f%%, %d/%d nodes) → leaderboard [%s]\n",
		sc.Total, ratio*100, passed, total, class)

	// The round clock ticks for CLI runs too; a due exam prints a hint
	// instead of firing a silent network call — /exam or -exam runs it.
	lb := eval.OpenLeaderboard()
	if rounds, due := lb.CountRound(class); due {
		fmt.Printf("  exam due (%d/%d rounds) — lyon run -exam %s …\n",
			rounds, eval.ExamInterval, class)
	}

	if res.OK {
		return 0
	}
	return 1
}
