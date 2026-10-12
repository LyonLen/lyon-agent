package eval

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestParseChecklistToleratesFencesAndProse(t *testing.T) {
	reply := "Sure! Here you go:\n```json\n[{\"q\":\"build cmd?\",\"expected\":\"go build ./...\"}]\n```\nHappy coding."
	items, err := ParseChecklist(reply)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Expected != "go build ./..." {
		t.Fatalf("items = %+v", items)
	}
	if _, err := ParseChecklist("no json here"); err == nil {
		t.Fatal("expected an error for a reply with no array")
	}
}

func TestParseBookToleratesFences(t *testing.T) {
	m, err := ParseBook("```json\n{\"build cmd?\":\"go build ./...\"}\n```")
	if err != nil {
		t.Fatal(err)
	}
	if m["build cmd?"] != "go build ./..." {
		t.Fatalf("book = %#v", m)
	}
}

func TestRunCoverageGradesAndInjectsMisses(t *testing.T) {
	gen := func(context.Context, string) (string, error) {
		return `[{"q":"build?","expected":"go build ./..."},{"q":"test?","expected":"go test ./..."},{"q":"port?","expected":"8080"}]`, nil
	}
	book := func(_ context.Context, qs []string) (string, error) {
		if len(qs) != 3 {
			t.Fatalf("book got %d questions", len(qs))
		}
		return `{"build?":"go build ./...","test?":"go run tests","port?":"8080"}`, nil
	}
	c, inject, err := RunCoverage(context.Background(), gen, book, "ship it")
	if err != nil {
		t.Fatal(err)
	}
	if c.Items != 3 || c.Hits != 2 {
		t.Fatalf("coverage = %+v", c)
	}
	if c.Score != 2.0/3.0 {
		t.Fatalf("score = %v", c.Score)
	}
	if len(inject) != 1 || !strings.Contains(inject[0], "test?") {
		t.Fatalf("inject = %v", inject)
	}
}

func TestRunCoverageGateOutcome(t *testing.T) {
	// Two of three hit → 0.667 passes the 0.6 gate.
	gen := func(context.Context, string) (string, error) {
		return `[{"q":"a","expected":"1"},{"q":"b","expected":"2"},{"q":"c","expected":"3"}]`, nil
	}
	book := func(context.Context, []string) (string, error) {
		return `{"a":"1","b":"2","c":"nope"}`, nil
	}
	c, _, err := RunCoverage(context.Background(), gen, book, "t")
	if err != nil {
		t.Fatal(err)
	}
	if !c.GatePass() || !c.Pass(CoverageThreshold) {
		t.Fatalf("2/3 must pass 0.6: %+v", c)
	}

	// One of three → 0.333 blocks.
	book = func(context.Context, []string) (string, error) {
		return `{"a":"1","b":"nope","c":"nope"}`, nil
	}
	c, inject, err := RunCoverage(context.Background(), gen, book, "t")
	if err != nil {
		t.Fatal(err)
	}
	if c.GatePass() {
		t.Fatalf("1/3 must fail the gate: %+v", c)
	}
	if len(inject) != 2 {
		t.Fatalf("two misses must be injected: %v", inject)
	}
}

func TestRunCoverageEmptyChecklistNeverBlocks(t *testing.T) {
	c, inject, err := RunCoverage(context.Background(),
		func(context.Context, string) (string, error) { return `[]`, nil },
		func(context.Context, []string) (string, error) {
			t.Fatal("book must not run for an empty checklist")
			return "", nil
		}, "t")
	if err != nil {
		t.Fatal(err)
	}
	if c.Score != 1 || len(inject) != 0 || !c.GatePass() {
		t.Fatalf("empty checklist: %+v", c)
	}
}

func TestRunCoveragePropagatesGeneratorError(t *testing.T) {
	_, _, err := RunCoverage(context.Background(),
		func(context.Context, string) (string, error) { return "", fmt.Errorf("boom") },
		nil, "t")
	if err == nil || !strings.Contains(err.Error(), "checklist") {
		t.Fatalf("err = %v", err)
	}
}
