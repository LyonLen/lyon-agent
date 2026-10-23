package eval

// Self-evolution (2026-10-23 重定义): a failure the system already
// understood — a diagnosis post-mortem, a failed exam — becomes a new
// candidate instead of a lost session. The child inherits the parent's
// model and effort, starts with a copy of its specialist space, and
// carries the lesson as its first knowledge document. Elections then
// decide whether the child outperforms the parent; the generation cap
// bounds the lineage so a hopeless line terminates instead of
// respawning forever.

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"lyon-agent/pkg/space"
)

// MaxEvolveGen bounds one lineage: parent → g1 → g2 → g3, then no more
// spawns from that line. Matches the exam-fail retry bound — a line
// that could not earn a seat in three tries is a dead end.
const MaxEvolveGen = 3

// evolveMu makes the read-gen → save-child sequence atomic: a failed
// exam and a diagnosis landing on the same turn would otherwise both
// compute the same next generation and silently overwrite each other.
var evolveMu sync.Mutex

// EvolveChild spawns the next generation of a lineage from a lesson.
// parent may be any candidate id including "self" (the incumbent's own
// mistakes evolve too). The child id is "<parent>-g<gen>" with gen =
// max existing child generation + 1; past MaxEvolveGen it refuses.
// The lesson lands as the child's first knowledge document, and the
// parent's whole specialist space is copied over (space.CopySpace —
// knowledge and skills both; the copy is idempotent).
//
// Errors are real: a bad parent id, an exhausted lineage, or a failed
// disk write all return non-nil so the caller can report instead of
// silently losing the lesson.
func EvolveChild(parentID, lesson string) (Candidate, error) {
	parentID = strings.TrimSpace(parentID)
	lesson = strings.TrimSpace(lesson)
	if parentID == "" {
		return Candidate{}, errors.New("eval: evolve needs a parent id")
	}

	evolveMu.Lock()
	defer evolveMu.Unlock()

	// Resolve the parent's profile: a saved candidate, or the live
	// incumbent standing in for its id.
	var parent Candidate
	found := false
	for _, c := range LoadCandidates() {
		if c.ID == parentID {
			parent, found = c, true
			break
		}
	}
	if !found && parentID == selfID {
		parent = SelfCandidate("")
		found = true
	}
	if !found {
		return Candidate{}, fmt.Errorf("eval: no candidate %q to evolve from", parentID)
	}

	// Generation is the max of two bounds, both capped at MaxEvolveGen:
	// the chain depth (parent.Gen+1 — g1's child is g2, so a deepening
	// line dies at gen 3) and the sibling count (living children of the
	// same parent each take the next free slot — parallel attempts also
	// stop at 3). A deleted child frees its slot: elimination means the
	// attempt is replaced, and the cap's real enforcement is the exam
	// cadence that triggers each spawn.
	gen := parent.Gen + 1
	for _, c := range LoadCandidates() {
		if c.Parent == parentID && c.Gen >= gen {
			gen = c.Gen + 1
		}
	}
	if gen > MaxEvolveGen {
		return Candidate{}, fmt.Errorf(
			"eval: lineage %s exhausted (gen %d ≥ %d) — the line had its chances",
			parentID, gen-1, MaxEvolveGen)
	}

	child := Candidate{
		ID:     fmt.Sprintf("%s-g%d", parentID, gen),
		Model:  parent.Model,
		Effort: parent.Effort,
		Note:   "evolved from " + parentID + ": " + clipRunes(lesson, 160),
		Gen:    gen,
		Parent: parentID,
	}
	if child.Model == "" {
		// The live incumbent's model when evolving "self" without a
		// saved profile — the child must answer on SOME model.
		child.Model = SelfCandidate("").Model
	}
	if err := SaveCandidate(child); err != nil {
		return Candidate{}, err
	}

	// Inherit the parent's specialist space, then record the lesson as
	// the child's own first knowledge. Best-effort on the copy (an empty
	// parent space is normal); a failed lesson write is a real error —
	// the child without its reason to exist is not worth keeping.
	_ = space.CopySpace(parent.ID, child.ID)
	if lesson != "" {
		if _, err := space.SaveKnowledge(child.ID,
			"lesson from "+parent.ID,
			"why this generation exists", lesson); err != nil {
			return Candidate{}, fmt.Errorf("eval: child space: %w", err)
		}
	}
	return child, nil
}
