package space

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scratchHome isolates $LYON_HOME so no test touches real agent data.
func scratchHome(t *testing.T) {
	t.Helper()
	t.Setenv("LYON_HOME", t.TempDir())
}

// TestSaveAndListKnowledge: a save lands in the agent's own store with
// the owner as the first endorser, and lists back name-sorted.
func TestSaveAndListKnowledge(t *testing.T) {
	scratchHome(t)
	if _, err := SaveKnowledge("alpha", "Go vet first", "vet before build",
		"Run go vet before go build in CI."); err != nil {
		t.Fatalf("SaveKnowledge: %v", err)
	}
	if _, err := SaveKnowledge("alpha", "Windows flake", "RemoveAll retry",
		"TempDir cleanup races on Windows; retry in isolation."); err != nil {
		t.Fatalf("SaveKnowledge: %v", err)
	}
	items := List("alpha", KindKnowledge)
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	// Name-sorted: "Go vet first" < "Windows flake" (case-insensitive
	// in the sort? No — byte order, 'G' < 'W').
	if items[0].Name != "Go vet first" || items[1].Name != "Windows flake" {
		t.Errorf("order = %q, %q", items[0].Name, items[1].Name)
	}
	if len(items[0].Endorsements) != 1 || items[0].Endorsements[0] != "alpha" {
		t.Errorf("endorsements = %v, want [alpha]", items[0].Endorsements)
	}
	// The file itself parses back with the body intact.
	_, body, err := readItem(items[0].Path)
	if err != nil {
		t.Fatalf("readItem: %v", err)
	}
	if !strings.Contains(body, "go vet before go build") {
		t.Errorf("body = %q", body)
	}
}

// TestSaveSkillLayoutsForScanner: skills land in dir/SKILL.md layout —
// the same shape the skills scanner consumes.
func TestSaveSkillLayoutsForScanner(t *testing.T) {
	scratchHome(t)
	if _, err := SaveSkill("alpha", "Release Notes", "draft release notes",
		"# Release Notes\n\nGather merged PRs, group by label."); err != nil {
		t.Fatalf("SaveSkill: %v", err)
	}
	path := filepath.Join(AgentsDir(), "alpha", "skills", "release-notes", "SKILL.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("SKILL.md missing at expected path: %v", err)
	}
	if !strings.Contains(string(raw), "name: Release Notes") {
		t.Errorf("frontmatter missing name:\n%s", raw)
	}
	items := List("alpha", KindSkills)
	if len(items) != 1 || items[0].Name != "Release Notes" {
		t.Fatalf("List skills = %+v", items)
	}
}

// TestSharePromotesOnConsensus: one item, owner-endorsed, then shared
// by a second source → promoted to commons. A third share is a no-op.
func TestSharePromotesOnConsensus(t *testing.T) {
	scratchHome(t)
	if _, err := SaveKnowledge("alpha", "loopback", "lyond is loopback-only",
		"lyond binds loopback and has no auth by design."); err != nil {
		t.Fatalf("SaveKnowledge: %v", err)
	}
	promoted, err := Share("alpha", KindKnowledge, "loopback", "beta")
	if err != nil {
		t.Fatalf("Share: %v", err)
	}
	if !promoted {
		t.Fatal("second distinct source must promote to commons")
	}
	commons := ListCommons(KindKnowledge)
	if len(commons) != 1 || commons[0].Name != "loopback" {
		t.Fatalf("commons = %+v", commons)
	}
	// Idempotent: same endorser again changes nothing.
	promoted, err = Share("alpha", KindKnowledge, "loopback", "beta")
	if err != nil {
		t.Fatalf("re-Share: %v", err)
	}
	if promoted {
		t.Error("re-sharing from the same source must not re-promote")
	}
	// Self-endorsement is refused — consensus needs a second source.
	if _, err := Share("alpha", KindKnowledge, "loopback", "alpha"); err == nil {
		t.Error("owner self-share must be refused")
	}
}

// TestIdenticalBodiesConverge: two agents independently saving the SAME
// body is the strongest consensus — both promote automatically.
func TestIdenticalBodiesConverge(t *testing.T) {
	scratchHome(t)
	body := "The exam pass line is 0.75 by default."
	if _, err := SaveKnowledge("alpha", "exam-pass", "default pass line", body); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveKnowledge("beta", "exam-pass-b", "default pass line", body); err != nil {
		t.Fatal(err)
	}
	commons := ListCommons(KindKnowledge)
	if len(commons) == 0 {
		t.Fatal("identical bodies from two agents must reach commons")
	}
	// Both agents' items carry mutual endorsements.
	for _, agent := range []string{"alpha", "beta"} {
		items := List(agent, KindKnowledge)
		if len(items) != 1 {
			t.Fatalf("%s items = %+v", agent, items)
		}
		if len(items[0].Endorsements) < 2 {
			t.Errorf("%s endorsements = %v, want mutual pair", agent, items[0].Endorsements)
		}
	}
}

// TestShareAllContestsWholeSpace: a contest win endorses every item of
// the winner — the election is the second source.
func TestShareAllContestsWholeSpace(t *testing.T) {
	scratchHome(t)
	SaveKnowledge("winner", "k1", "fact one", "body one")
	SaveKnowledge("winner", "k2", "fact two", "body two")
	SaveSkill("winner", "s1", "skill one", "# S1\n\nhow to s1")
	promoted := ShareAll("winner", "contest:research")
	if len(promoted) != 3 {
		t.Fatalf("promoted = %v, want all three items", promoted)
	}
	if len(ListCommons(KindKnowledge)) != 2 {
		t.Errorf("commons knowledge = %+v", ListCommons(KindKnowledge))
	}
	if len(ListCommons(KindSkills)) != 1 {
		t.Errorf("commons skills = %+v", ListCommons(KindSkills))
	}
}

// TestCopySpaceInherits: a child inherits the parent's stores; an item
// the child already holds is not duplicated.
func TestCopySpaceInherits(t *testing.T) {
	scratchHome(t)
	SaveKnowledge("parent", "pk", "parent fact", "parent body")
	SaveSkill("parent", "ps", "parent skill", "# PS")
	SaveKnowledge("child", "ck", "child fact", "child body")
	if err := CopySpace("parent", "child"); err != nil {
		t.Fatalf("CopySpace: %v", err)
	}
	items := List("child", KindKnowledge)
	if len(items) != 2 {
		t.Fatalf("child knowledge = %+v, want inherited + own", items)
	}
	if len(List("child", KindSkills)) != 1 {
		t.Errorf("child skills = %+v", List("child", KindSkills))
	}
	// Idempotent second copy adds nothing.
	if err := CopySpace("parent", "child"); err != nil {
		t.Fatalf("CopySpace again: %v", err)
	}
	if len(List("child", KindKnowledge)) != 2 {
		t.Errorf("second copy duplicated items")
	}
}

// TestSlugCollisionKeepsBoth: two different names that slugify the same
// way both survive — the second gets a suffix instead of shadowing.
func TestSlugCollisionKeepsBoth(t *testing.T) {
	scratchHome(t)
	SaveKnowledge("alpha", "go vet", "a", "body a")
	SaveKnowledge("alpha", "Go/Vet", "b", "body b")
	items := List("alpha", KindKnowledge)
	if len(items) != 2 {
		t.Fatalf("collision lost an item: %+v", items)
	}
	names := map[string]bool{items[0].Name: true, items[1].Name: true}
	if !names["go vet"] || !names["Go/Vet"] {
		t.Errorf("names = %v", names)
	}
}

// TestSaveRejectsBadInput: bad agent ids and empty bodies are refused
// before anything touches disk.
func TestSaveRejectsBadInput(t *testing.T) {
	scratchHome(t)
	if _, err := SaveKnowledge("../evil", "n", "d", "b"); err == nil {
		t.Error("path-hostile agent id must be refused")
	}
	if _, err := SaveKnowledge("alpha", "", "d", "b"); err == nil {
		t.Error("nameless knowledge must be refused")
	}
	if _, err := SaveKnowledge("alpha", "n", "d", "  "); err == nil {
		t.Error("empty body must be refused")
	}
	if _, err := SaveSkill("alpha", "n", "d", ""); err == nil {
		t.Error("empty skill body must be refused")
	}
}

// TestBriefRendersSpace: the system-note brief lists an agent's items
// and is empty for an agent with nothing.
func TestBriefRendersSpace(t *testing.T) {
	scratchHome(t)
	if got := Brief("nobody"); got != "" {
		t.Errorf("empty agent brief = %q, want empty", got)
	}
	SaveKnowledge("alpha", "loopback", "lyond binds loopback", "no auth by design")
	b := Brief("alpha")
	if !strings.Contains(b, "loopback") || !strings.Contains(b, "alpha") {
		t.Errorf("brief missing content:\n%s", b)
	}
}
