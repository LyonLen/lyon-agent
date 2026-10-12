// Package eval grades agent runs and keeps the elimination ledger from
// docs/design/agent-evolution.md: a weighted Scorecard per attempt, a
// file-backed leaderboard that puts the best condition-passing candidate
// on duty, held-out exams every ten rounds, coverage checklists and
// failure diagnosis. Everything is deterministic and file-backed so a
// scoring decision can be replayed offline.
package eval

import (
	"math"
	"time"
)

// Score weights — one dominant correctness term plus efficiency, cost and
// compliance, so a fast cheap run that is wrong never outranks a correct
// one (design §1.2).
const (
	WeightCorrectness = 0.50
	WeightEfficiency  = 0.20
	WeightCost        = 0.15
	WeightCompliance  = 0.15
)

// violationPenalty is how much Compliance loses per observed workflow or
// authorization violation (hooks audit): three strikes zero the dim.
const violationPenalty = 0.25

// RunMetrics are the raw facts of one attempt, collected by the caller
// (steps count from the audit trail, cost from the usage gauge, violations
// from hooks). Zero-valued references (baseline, budget) disable their
// dimension instead of guessing.
type RunMetrics struct {
	Correctness   float64 // 0..1 — graded against ground truth or a blind judge
	Steps         int     // effective actions the run took
	BaselineSteps int     // reference cost for this task class; 0 = no baseline yet
	CostUSD       float64 // actual spend
	BudgetUSD     float64 // intended spend; 0 = no budget yet
	Violations    int     // workflow / authorization violations observed
}

// Dims are the four graded dimensions, each clamped to 0..1.
type Dims struct {
	Correctness float64 `json:"correctness"`
	Efficiency  float64 `json:"efficiency"`
	Cost        float64 `json:"cost"`
	Compliance  float64 `json:"compliance"`
}

// Thresholds are the condition lines a run must clear per dimension to
// count as "qualified" — passing is all-or-nothing across them.
type Thresholds struct {
	Correctness float64 `json:"correctness"`
	Efficiency  float64 `json:"efficiency"`
	Cost        float64 `json:"cost"`
	Compliance  float64 `json:"compliance"`
}

// DefaultThresholds: near-certain correctness, a third of the baseline
// effort, staying inside a fifth of the budget headroom, and clean audit.
var DefaultThresholds = Thresholds{
	Correctness: 0.80,
	Efficiency:  0.30,
	Cost:        0.20,
	Compliance:  0.90,
}

// Scorecard is one graded attempt. Source tags where the grade came from:
// "production" (task run), "exam" (held-out test) or "flow" (workflow run).
type Scorecard struct {
	Agent  string    `json:"agent"`
	Class  string    `json:"class"`
	Dims   Dims      `json:"dims"`
	Total  float64   `json:"total"`
	Passed bool      `json:"passed"`
	Source string    `json:"source,omitempty"`
	At     time.Time `json:"at"`
}

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}

// dimsOf derives the graded dimensions from raw metrics:
//   - efficiency = baseline/steps when over baseline (a run twice as long
//     as the reference scores 0.5), full marks at or under it;
//   - cost = budget/actual when over budget, full marks inside it;
//   - compliance loses violationPenalty per violation.
func dimsOf(m RunMetrics) Dims {
	eff := 1.0
	if m.BaselineSteps > 0 && m.Steps > m.BaselineSteps {
		eff = float64(m.BaselineSteps) / float64(m.Steps)
	}
	cost := 1.0
	if m.BudgetUSD > 0 && m.CostUSD > m.BudgetUSD {
		cost = m.BudgetUSD / m.CostUSD
	}
	comp := 1 - violationPenalty*float64(m.Violations)
	return Dims{
		Correctness: clamp01(m.Correctness),
		Efficiency:  clamp01(eff),
		Cost:        clamp01(cost),
		Compliance:  clamp01(comp),
	}
}

// round4 keeps stored totals comparable across reloads.
func round4(x float64) float64 { return math.Round(x*10000) / 10000 }

// Score grades a production run with the default weights and thresholds.
func Score(agent, class string, m RunMetrics) Scorecard {
	return ScoreWith(agent, class, "production", m, DefaultThresholds)
}

// ScoreWith grades an attempt with explicit source and thresholds (exams
// and flows pass their own source tag; tests pin custom thresholds).
func ScoreWith(agent, class, source string, m RunMetrics, th Thresholds) Scorecard {
	d := dimsOf(m)
	total := WeightCorrectness*d.Correctness +
		WeightEfficiency*d.Efficiency +
		WeightCost*d.Cost +
		WeightCompliance*d.Compliance
	passed := d.Correctness >= th.Correctness &&
		d.Efficiency >= th.Efficiency &&
		d.Cost >= th.Cost &&
		d.Compliance >= th.Compliance
	return Scorecard{
		Agent:  agent,
		Class:  class,
		Dims:   d,
		Total:  round4(total),
		Passed: passed,
		Source: source,
		At:     time.Now().UTC(),
	}
}
