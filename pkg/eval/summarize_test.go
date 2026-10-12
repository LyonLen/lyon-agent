package eval

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestParseDiagnosisTolerant(t *testing.T) {
	reply := "```json\n{\"failed_conditions\":[\"run tests before claiming done\",\"  \"],\"patch\":\"verify with go test ./...\"}\n```"
	d := ParseDiagnosis(reply)
	if len(d.FailedConditions) != 1 || d.FailedConditions[0] != "run tests before claiming done" {
		t.Fatalf("conditions = %#v", d.FailedConditions)
	}
	if d.Patch != "verify with go test ./..." {
		t.Fatalf("patch = %q", d.Patch)
	}
	// Garbage → empty diagnosis, never a panic.
	if d := ParseDiagnosis("I could not analyze this."); len(d.FailedConditions) != 0 || d.Patch != "" {
		t.Fatalf("garbage = %+v", d)
	}
	if d := ParseDiagnosis("{not json"); len(d.FailedConditions) != 0 {
		t.Fatalf("broken json = %+v", d)
	}
}

func TestSummarizeFailureCallsAndClips(t *testing.T) {
	var gotSystem string
	var gotUser string
	ask := func(_ context.Context, system, user string) (string, error) {
		gotSystem, gotUser = system, user
		return `{"failed_conditions":["missed config dir"],"patch":"always grep config/"}`, nil
	}
	long := strings.Repeat("x", TrajectoryCap+500)
	d, err := SummarizeFailure(context.Background(), ask, long)
	if err != nil {
		t.Fatal(err)
	}
	if gotSystem != DiagnoseSystem {
		t.Fatalf("system prompt = %q", gotSystem)
	}
	if len([]rune(gotUser)) > TrajectoryCap+1 { // clipped with ellipsis
		t.Fatalf("trajectory not clipped: %d runes", len([]rune(gotUser)))
	}
	if len(d.FailedConditions) != 1 {
		t.Fatalf("diagnosis = %+v", d)
	}
	// Asker error propagates (caller decides whether to retry).
	_, err = SummarizeFailure(context.Background(), func(context.Context, string, string) (string, error) {
		return "", fmt.Errorf("down")
	}, "t")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestDiagnosisConstraintsRender(t *testing.T) {
	d := Diagnosis{FailedConditions: []string{"verify before finish"}, Patch: "run go test ./..."}
	block := d.Constraints()
	if !strings.Contains(block, "- verify before finish") ||
		!strings.Contains(block, "- run go test ./...") ||
		!strings.Contains(block, "failed attempt") {
		t.Fatalf("constraints block = %q", block)
	}
	if (Diagnosis{}).Constraints() != "" {
		t.Fatal("empty diagnosis must render nothing")
	}
}
