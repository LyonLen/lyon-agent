package eval

import "testing"

func TestScoreDimsClampAndWeight(t *testing.T) {
	sc := Score("a1", "coding", RunMetrics{
		Correctness: 1.5, // over-range grades down to 1
		Steps:       5, BaselineSteps: 10,
		CostUSD: 0.5, BudgetUSD: 1,
		Violations: 0,
	})
	if sc.Dims.Correctness != 1 || sc.Dims.Efficiency != 1 || sc.Dims.Cost != 1 || sc.Dims.Compliance != 1 {
		t.Fatalf("all-green run must score 1s, got %+v", sc.Dims)
	}
	if sc.Total != 1 || !sc.Passed {
		t.Fatalf("all-green total = %v passed=%v", sc.Total, sc.Passed)
	}
}

func TestScoreEfficiencyAndCostRatios(t *testing.T) {
	sc := Score("a1", "coding", RunMetrics{
		Correctness: 1,
		Steps:       20, BaselineSteps: 10, // twice the baseline → 0.5
		CostUSD: 2, BudgetUSD: 1, // twice the budget → 0.5
	})
	if sc.Dims.Efficiency != 0.5 {
		t.Fatalf("efficiency = %v, want 0.5", sc.Dims.Efficiency)
	}
	if sc.Dims.Cost != 0.5 {
		t.Fatalf("cost = %v, want 0.5", sc.Dims.Cost)
	}
	want := round4(WeightCorrectness*1 + WeightEfficiency*0.5 + WeightCost*0.5 + WeightCompliance*1)
	if sc.Total != want {
		t.Fatalf("total = %v, want %v", sc.Total, want)
	}
	// All dims still above default thresholds → passes.
	if !sc.Passed {
		t.Fatalf("expected pass, dims %+v", sc.Dims)
	}
}

func TestScoreNoBaselineOrBudgetIsFullMarks(t *testing.T) {
	sc := Score("a1", "coding", RunMetrics{Correctness: 1, Steps: 999, CostUSD: 99})
	if sc.Dims.Efficiency != 1 || sc.Dims.Cost != 1 {
		t.Fatalf("missing references must not punish: %+v", sc.Dims)
	}
}

func TestScoreViolationsAndThresholds(t *testing.T) {
	sc := Score("a1", "coding", RunMetrics{Correctness: 1, Violations: 1})
	if sc.Dims.Compliance != 0.75 {
		t.Fatalf("compliance = %v, want 0.75", sc.Dims.Compliance)
	}
	if sc.Passed {
		t.Fatal("one violation must fail the 0.90 compliance line")
	}
	// Four violations zero the dim; very low correctness fails too.
	sc = Score("a1", "coding", RunMetrics{Correctness: 0.5, Violations: 4})
	if sc.Dims.Compliance != 0 || sc.Passed {
		t.Fatalf("dims %+v passed=%v", sc.Dims, sc.Passed)
	}
}

func TestScoreWithSourceAndCustomThresholds(t *testing.T) {
	th := Thresholds{Correctness: 0.5, Efficiency: 0.1, Cost: 0.1, Compliance: 0.1}
	sc := ScoreWith("a1", "exam", "exam", RunMetrics{Correctness: 0.6}, th)
	if !sc.Passed || sc.Source != "exam" || sc.Class != "exam" {
		t.Fatalf("sc = %+v", sc)
	}
}
