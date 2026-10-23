package eval

import (
	"strings"
	"testing"

	"lyon-agent/pkg/space"
)

// TestEvolveChildSpawnsNextGeneration: the child inherits model and
// effort, carries parent/gen, and its id is <parent>-g<gen>.
func TestEvolveChildSpawnsNextGeneration(t *testing.T) {
	scratchHome(t)
	parent := Candidate{ID: "alpha", Model: "model-a", Effort: "high", Note: "challenger"}
	if err := SaveCandidate(parent); err != nil {
		t.Fatalf("SaveCandidate: %v", err)
	}
	child, err := EvolveChild("alpha", "always forgets to run go vet")
	if err != nil {
		t.Fatalf("EvolveChild: %v", err)
	}
	if child.ID != "alpha-g1" || child.Gen != 1 || child.Parent != "alpha" {
		t.Errorf("child = %+v, want alpha-g1/gen1/parent alpha", child)
	}
	if child.Model != "model-a" || child.Effort != "high" {
		t.Errorf("inheritance lost: %+v", child)
	}
	if !strings.Contains(child.Note, "go vet") {
		t.Errorf("note does not carry the lesson: %q", child.Note)
	}
	// The child is a real candidate now.
	var found bool
	for _, c := range LoadCandidates() {
		if c.ID == "alpha-g1" {
			found = true
		}
	}
	if !found {
		t.Error("child profile not on disk")
	}
	// The lesson landed as the child's first knowledge.
	items := space.List(child.ID, space.KindKnowledge)
	if len(items) != 1 || !strings.Contains(items[0].Name, "lesson") {
		t.Errorf("child space = %+v, want the lesson document", items)
	}
}

// TestEvolveChildInheritsParentSpace: the child starts with a copy of
// the parent's specialist knowledge and skills.
func TestEvolveChildInheritsParentSpace(t *testing.T) {
	scratchHome(t)
	SaveCandidate(Candidate{ID: "beta", Model: "model-b"})
	space.SaveKnowledge("beta", "vet-first", "run vet first", "go vet before build")
	space.SaveSkill("beta", "release", "release playbook", "# Release\n\ntag, then publish")
	child, err := EvolveChild("beta", "lesson text")
	if err != nil {
		t.Fatalf("EvolveChild: %v", err)
	}
	know := space.List(child.ID, space.KindKnowledge)
	if len(know) != 2 { // inherited vet-first + own lesson
		t.Errorf("child knowledge = %+v, want inherited + lesson", know)
	}
	if len(space.List(child.ID, space.KindSkills)) != 1 {
		t.Errorf("child skills = %+v, want the inherited release skill", space.List(child.ID, space.KindSkills))
	}
}

// TestEvolveChildGenerationCap: both bounds of the cap — parallel
// siblings stop at three children per parent, and a deepening chain
// (each child parenting the next) also dies at gen 3.
func TestEvolveChildGenerationCap(t *testing.T) {
	scratchHome(t)
	SaveCandidate(Candidate{ID: "gamma", Model: "model-g"})
	// Sibling bound: three children of gamma, the fourth is refused.
	for gen := 1; gen <= MaxEvolveGen; gen++ {
		c, err := EvolveChild("gamma", "lesson")
		if err != nil {
			t.Fatalf("sibling %d: %v", gen, err)
		}
		if c.Gen != gen {
			t.Fatalf("sibling gen = %d, want %d", c.Gen, gen)
		}
	}
	if _, err := EvolveChild("gamma", "fourth"); err == nil {
		t.Fatal("a fourth child of the same parent must be refused")
	}
	// Chain bound: deepening from one child — g2, g3, then refusal.
	if _, err := EvolveChild("gamma-g1", "deepen"); err != nil {
		t.Fatalf("chain g2: %v", err)
	}
	if _, err := EvolveChild("gamma-g1-g2", "deepen"); err != nil {
		t.Fatalf("chain g3: %v", err)
	}
	if _, err := EvolveChild("gamma-g1-g2-g3", "deepen"); err == nil {
		t.Fatal("chain gen 4 must be refused")
	}
}

// TestEvolveChildFromSelfAndUnknown: "self" evolves (the incumbent's
// mistakes count); an unknown parent is a real error.
func TestEvolveChildFromSelfAndUnknown(t *testing.T) {
	scratchHome(t)
	child, err := EvolveChild("self", "kept editing files in plan mode")
	if err != nil {
		t.Fatalf("EvolveChild(self): %v", err)
	}
	if child.ID != "self-g1" || child.Parent != "self" {
		t.Errorf("self child = %+v", child)
	}
	if _, err := EvolveChild("ghost", "lesson"); err == nil {
		t.Error("unknown parent must error")
	}
	if _, err := EvolveChild("", "lesson"); err == nil {
		t.Error("empty parent must error")
	}
}

// TestEvolveChildEmptyLessonStillSpawns: a diagnosis with conditions but
// no patch still spawns — the conditions ARE the lesson.
func TestEvolveChildEmptyLessonStillSpawns(t *testing.T) {
	scratchHome(t)
	SaveCandidate(Candidate{ID: "delta", Model: "model-d"})
	child, err := EvolveChild("delta", "")
	if err != nil {
		t.Fatalf("EvolveChild empty lesson: %v", err)
	}
	// No lesson document — the child exists but carries no false lesson.
	if items := space.List(child.ID, space.KindKnowledge); len(items) != 0 {
		t.Errorf("child knowledge = %+v, want none for an empty lesson", items)
	}
	// Inherited space still copied.
	space.SaveKnowledge("delta", "kept", "kept fact", "kept body")
	if _, err := EvolveChild("delta", "x"); err != nil {
		t.Fatal(err)
	}
	// g2 carries the inherited fact AND its own lesson (two documents).
	items := space.List("delta-g2", space.KindKnowledge)
	if len(items) != 2 {
		t.Errorf("g2 knowledge = %+v, want inherited fact + lesson", items)
	}
}
