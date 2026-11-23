package server

import (
	"os"
	"strings"
	"testing"
	"time"

	"lyon-agent/pkg/memory"
)

// freshMemStore opens a store over a throwaway workspace (process-wide
// cache keys on the abs path — unique per test). Cleanup retries
// RemoveAll: TempDir races the antivirus scanner on this machine (the
// same settle loop pkg/server's scratchHome uses).
func freshMemStore(t *testing.T) *memory.Store {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() {
		var err error
		for i := 0; i < 40; i++ {
			if err = os.RemoveAll(dir); err == nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Logf("settle cleanup failed: %v", err)
	})
	return memory.Open(dir)
}

func seedTarget(t *testing.T, st *memory.Store, scope, name, body string) transformTarget {
	t.Helper()
	f, err := st.Save(memory.Fact{Name: name, Scope: scope, Body: body, Description: name + " desc"})
	if err != nil {
		t.Fatal(err)
	}
	return transformTarget{scope: scope, name: name, fact: f}
}

// TestApplyTransformPlanSimplify: each selected fact is rewritten in
// place; a write outside the selection is rejected before anything
// lands.
func TestApplyTransformPlanSimplify(t *testing.T) {
	st := freshMemStore(t)
	a := seedTarget(t, st, memory.ScopeProject, "alpha", "long body one")
	b := seedTarget(t, st, memory.ScopeProject, "beta", "long body two")
	other := seedTarget(t, st, memory.ScopeProject, "gamma", "not selected")

	var plan transformPlan
	plan.Summary = "shortened both"
	plan.Writes = append(plan.Writes, struct {
		Scope       string `json:"scope"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Body        string `json:"body"`
	}{Scope: memory.ScopeProject, Name: "alpha", Body: "short one"})

	changed, removed, err := applyTransformPlan(st, "simplify", []transformTarget{a, b}, plan)
	if err != nil {
		t.Fatalf("valid simplify: %v", err)
	}
	if len(changed) != 1 || len(removed) != 0 {
		t.Fatalf("changed=%d removed=%d", len(changed), len(removed))
	}
	if f, _ := st.Get(memory.ScopeProject, "alpha"); f.Body != "short one" {
		t.Fatalf("alpha body = %q", f.Body)
	}
	if f, _ := st.Get(memory.ScopeProject, "beta"); f.Body != "long body two" {
		t.Fatalf("beta must be untouched: %q", f.Body)
	}

	// A write to a non-selected fact fails BEFORE any write lands.
	plan.Writes[0].Name = "gamma"
	if _, _, err := applyTransformPlan(st, "simplify", []transformTarget{a, b}, plan); err == nil || !strings.Contains(err.Error(), "selected") {
		t.Fatalf("overwrite guard missing: %v", err)
	}
	if f, _ := st.Get(memory.ScopeProject, "gamma"); f.Body != "not selected" {
		t.Fatalf("gamma was written: %q", f.Body)
	}
	_ = other
}

// TestApplyTransformPlanMerge: sources are consumed, and a merge that
// keeps the FIRST source's name must not delete its own result.
func TestApplyTransformPlanMerge(t *testing.T) {
	st := freshMemStore(t)
	a := seedTarget(t, st, memory.ScopeProject, "alpha", "fact A")
	b := seedTarget(t, st, memory.ScopeProject, "beta", "fact B")

	plan := transformPlan{Summary: "merged A+B"}
	plan.Writes = append(plan.Writes, struct {
		Scope       string `json:"scope"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Body        string `json:"body"`
	}{Scope: memory.ScopeProject, Name: "alpha", Body: "A and B together"})
	plan.Deletes = append(plan.Deletes, struct {
		Scope string `json:"scope"`
		Name  string `json:"name"`
	}{Scope: memory.ScopeProject, Name: "alpha"}) // kept name listed as source too
	plan.Deletes = append(plan.Deletes, struct {
		Scope string `json:"scope"`
		Name  string `json:"name"`
	}{Scope: memory.ScopeProject, Name: "beta"})

	changed, removed, err := applyTransformPlan(st, "merge", []transformTarget{a, b}, plan)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if len(changed) != 1 || len(removed) != 1 || removed[0].name != "beta" {
		t.Fatalf("changed=%d removed=%+v", len(changed), removed)
	}
	f, ok := st.Get(memory.ScopeProject, "alpha")
	if !ok || f.Body != "A and B together" {
		t.Fatalf("merged alpha must survive: ok=%v %+v", ok, f)
	}
	if _, ok := st.Get(memory.ScopeProject, "beta"); ok {
		t.Fatal("beta must be consumed")
	}

	// Deleting a NON-selected fact is rejected.
	plan.Deletes = plan.Deletes[:1]
	plan.Deletes[0].Name = "stranger"
	if _, _, err := applyTransformPlan(st, "merge", []transformTarget{a}, plan); err == nil {
		t.Fatal("merge deleting outside the selection must fail")
	}
}

// TestApplyTransformPlanExtract: one NEW fact lands, the originals stay;
// deletes under extract are rejected; a new name that already exists
// outside the selection is rejected.
func TestApplyTransformPlanExtract(t *testing.T) {
	st := freshMemStore(t)
	a := seedTarget(t, st, memory.ScopeProject, "alpha", "one")
	b := seedTarget(t, st, memory.ScopeProject, "beta", "two")
	seedTarget(t, st, memory.ScopeProject, "occupied", "already here")

	plan := transformPlan{Summary: "extracted the essence"}
	plan.Writes = append(plan.Writes, struct {
		Scope       string `json:"scope"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Body        string `json:"body"`
	}{Scope: memory.ScopeProject, Name: "essence", Body: "shared core"})

	changed, removed, err := applyTransformPlan(st, "extract", []transformTarget{a, b}, plan)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(changed) != 1 || len(removed) != 0 {
		t.Fatalf("changed=%d removed=%d", len(changed), len(removed))
	}
	for _, name := range []string{"alpha", "beta"} {
		if _, ok := st.Get(memory.ScopeProject, name); !ok {
			t.Fatalf("%s must survive extract", name)
		}
	}
	if _, ok := st.Get(memory.ScopeProject, "essence"); !ok {
		t.Fatal("extracted fact missing")
	}

	// Deletes under extract are rejected.
	plan.Deletes = append(plan.Deletes, struct {
		Scope string `json:"scope"`
		Name  string `json:"name"`
	}{Scope: memory.ScopeProject, Name: "alpha"})
	if _, _, err := applyTransformPlan(st, "extract", []transformTarget{a, b}, plan); err == nil {
		t.Fatal("extract deleting originals must fail")
	}
	plan.Deletes = nil

	// A new name colliding with a non-selected fact is rejected.
	plan.Writes[0].Name = "occupied"
	if _, _, err := applyTransformPlan(st, "extract", []transformTarget{a, b}, plan); err == nil {
		t.Fatal("extract overwriting a non-selected fact must fail")
	}
}

// TestApplyTransformPlanShape: op shape rules fail loudly — no summary,
// wrong write counts, bad scopes, empty bodies.
func TestApplyTransformPlanShape(t *testing.T) {
	st := freshMemStore(t)
	a := seedTarget(t, st, memory.ScopeProject, "alpha", "x")

	if _, _, err := applyTransformPlan(st, "simplify", []transformTarget{a}, transformPlan{}); err == nil {
		t.Error("empty summary must fail")
	}
	merge := transformPlan{Summary: "s"}
	if _, _, err := applyTransformPlan(st, "merge", []transformTarget{a}, merge); err == nil {
		t.Error("merge with zero writes must fail")
	}
	simp := transformPlan{Summary: "s"}
	simp.Writes = append(simp.Writes, struct {
		Scope       string `json:"scope"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Body        string `json:"body"`
	}{Scope: "moon", Name: "alpha", Body: "x"})
	if _, _, err := applyTransformPlan(st, "simplify", []transformTarget{a}, simp); err == nil {
		t.Error("bad write scope must fail")
	}
	simp.Writes[0].Scope = memory.ScopeProject
	simp.Writes[0].Body = "  "
	if _, _, err := applyTransformPlan(st, "simplify", []transformTarget{a}, simp); err == nil {
		t.Error("empty body must fail")
	}
}
