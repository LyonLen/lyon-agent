package main

// Tests for the TUI auto-freeze hint (flowhint.go): signature→slug
// stability, the one-hint-per-segment counter wired to flow.BumpStreak,
// the class-change / red-run resets, and the LYON_AUTOFREEZE=off gate.
// Every test points LYON_HOME at a scratch dir — the shared streak
// ledger must never touch ~/.lyon.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lyon-agent/pkg/agent"
)

// fhRound drives one full prompt round through the real event path:
// a prompt group opens, the listed tools fire, prompt_end lands with
// the given error ("" = green). The answer area is cleared first so
// assertions see only what THIS round pushed.
func fhRound(m *tuiModel, round int, tools []string, errText string) {
	m.cmdOut = nil
	m.startPromptStep("keep the routine going")
	for i, name := range tools {
		m.applyEvent(agent.Event{
			Type:     agent.EventToolUseStart,
			ToolID:   fmt.Sprintf("t-%d-%d", round, i),
			ToolName: name,
			Input:    json.RawMessage(`{}`),
		})
	}
	m.applyEvent(agent.Event{Type: agent.EventPromptEnd, Err: errText, Iterations: 1})
}

// streakLedger reads the shared flow-streak.json; a missing file is an
// empty ledger (BumpStreak has not run).
func streakLedger(t *testing.T) map[string]int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(os.Getenv("LYON_HOME"), "flow-streak.json"))
	if os.IsNotExist(err) {
		return map[string]int{}
	}
	if err != nil {
		t.Fatalf("read streak ledger: %v", err)
	}
	var counts map[string]int
	if err := json.Unmarshal(data, &counts); err != nil {
		t.Fatalf("streak ledger is not JSON: %v", err)
	}
	return counts
}

// TestStreakSlugStable: the same tool signature always maps to the
// same slug; a different order or a different tool lands elsewhere, and
// the output is inside flow.slugify's character class so /flow save
// stores exactly the hinted name.
func TestStreakSlugStable(t *testing.T) {
	sig := []string{"read_file", "edit_file", "run_shell"}
	got := streakSlug(sig)
	if got != streakSlug(sig) {
		t.Fatalf("same signature produced two slugs: %q vs %q", got, streakSlug(sig))
	}
	if got == streakSlug([]string{"run_shell", "edit_file", "read_file"}) {
		t.Fatalf("order-different signature collided: %s", got)
	}
	if got == streakSlug([]string{"read_file", "edit_file", "search_files"}) {
		t.Fatalf("tool-different signature collided: %s", got)
	}
	for _, r := range got {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_'
		if !ok {
			t.Fatalf("slug %q has slugify-unsafe rune %q", got, r)
		}
	}
	if !strings.HasPrefix(got, "read-file-edit-file-run-shell-") {
		t.Fatalf("readable head missing from slug %q", got)
	}
}

// TestFlowRunToolSigShape: only rows after the newest prompt group
// count, repeats collapse to first sighting, subagent-inner and non-tool
// rows stay out while server tools and task nodes stay in.
func TestFlowRunToolSigShape(t *testing.T) {
	steps := []execStep{
		{kind: stepPrompt, detail: "older round"},
		{kind: stepTool, name: "read_file"},
		{kind: stepTool, name: "edit_file"},
		{kind: stepPrompt, detail: "this round"},
		{kind: stepServer, name: "web_search"},
		{kind: stepTool, name: "run_shell"},
		{kind: stepTool, name: "run_shell"},      // retry: collapses
		{kind: stepTool, name: "grep", under: 7}, // subagent-inner: out
		{kind: stepMemory, name: "distill"},      // not a tool: out
		{kind: stepSubagent, name: "run_subagent"},
	}
	sig := runToolSig(steps)
	want := []string{"web_search", "run_shell", "run_subagent"}
	if len(sig) != len(want) {
		t.Fatalf("sig = %v, want %v", sig, want)
	}
	for i := range want {
		if sig[i] != want[i] {
			t.Fatalf("sig = %v, want %v", sig, want)
		}
	}
}

// TestFlowHintThreeGreensHintOnce: the tui-level path — three green
// prompt_end rounds with the same tool routine put the freeze hint in
// the answer area exactly on the third, rounds four and five stay
// quiet, and the shared ledger ticks once per round (the CLI's file,
// written only by BumpStreak).
func TestFlowHintThreeGreensHintOnce(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))
	t.Cleanup(resetFlowHint)
	resetFlowHint()
	m := newTestModel(t)
	tools := []string{"read_file", "edit_file", "run_shell"}
	slug := streakSlug(tools)

	for round := 1; round <= 5; round++ {
		fhRound(m, round, tools, "")
		out := stripANSI(m.cmdOutText())
		want := round == 3
		if strings.Contains(out, "/flow save") != want {
			t.Fatalf("round %d: hint present=%v, want %v (area):\n%s",
				round, strings.Contains(out, "/flow save"), want, out)
		}
		if want && !strings.Contains(out, slug) {
			t.Fatalf("hint lacks slug %q:\n%s", slug, out)
		}
	}

	if n := streakLedger(t)[slug]; n != 5 {
		t.Fatalf("ledger[%s] = %d, want exactly 5 (one tick per round)", slug, n)
	}
}

// TestFlowHintClassChangeResets: switching routines breaks the old
// segment — the old slug's count is zeroed through BumpStreak's false
// leg, so returning to it counts from one, not from the carried-over
// total. Three straight greens of the class are required again.
func TestFlowHintClassChangeResets(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))
	t.Cleanup(resetFlowHint)
	resetFlowHint()
	m := newTestModel(t)
	seqA := []string{"read_file", "edit_file", "run_shell"}
	seqB := []string{"search_files"}

	fhRound(m, 1, seqA, "")
	fhRound(m, 2, seqA, "")
	if strings.Contains(m.cmdOutText(), "/flow save") {
		t.Fatalf("hint before the bar:\n%s", m.cmdOutText())
	}

	fhRound(m, 3, seqB, "") // class change: A's segment breaks
	slugA := streakSlug(seqA)
	if n := streakLedger(t)[slugA]; n != 0 {
		t.Fatalf("class change left ledger[%s] = %d, want 0 (count reset)", slugA, n)
	}

	fhRound(m, 4, seqA, "") // back — counting starts over
	if n := streakLedger(t)[slugA]; n != 1 {
		t.Fatalf("returned class ledger[%s] = %d, want 1 (no carry-over)", slugA, n)
	}
	if strings.Contains(m.cmdOutText(), "/flow save") {
		t.Fatalf("carried count leaked a hint:\n%s", m.cmdOutText())
	}

	fhRound(m, 5, seqA, "")
	if strings.Contains(m.cmdOutText(), "/flow save") {
		t.Fatalf("hint at count 2:\n%s", m.cmdOutText())
	}
	fhRound(m, 6, seqA, "")
	if !strings.Contains(m.cmdOutText(), "/flow save") {
		t.Fatalf("third consecutive green must hint:\n%s", m.cmdOutText())
	}
}

// TestFlowHintRedBreaksSegment: a failed prompt_end zeroes its slug
// and re-arms the hint — after the rebuild, the class hints again.
func TestFlowHintRedBreaksSegment(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))
	t.Setenv("LYON_DIAGNOSE", "off") // no async post-mortem in tests
	t.Cleanup(resetFlowHint)
	resetFlowHint()
	m := newTestModel(t)
	tools := []string{"run_shell"}
	slug := streakSlug(tools)

	for round := 1; round <= 3; round++ {
		fhRound(m, round, tools, "")
	}
	if !strings.Contains(m.cmdOutText(), "/flow save") {
		t.Fatalf("third green must hint:\n%s", m.cmdOutText())
	}

	fhRound(m, 4, tools, "gateway exploded") // red breaks the segment
	if n := streakLedger(t)[slug]; n != 0 {
		t.Fatalf("red run left ledger[%s] = %d, want 0", slug, n)
	}
	if strings.Contains(m.cmdOutText(), "/flow save") {
		t.Fatalf("failed round must not push the hint:\n%s", m.cmdOutText())
	}

	fhRound(m, 5, tools, "")
	fhRound(m, 6, tools, "")
	if strings.Contains(m.cmdOutText(), "/flow save") {
		t.Fatalf("hint at count 2 after rebuild:\n%s", m.cmdOutText())
	}
	fhRound(m, 7, tools, "")
	if !strings.Contains(m.cmdOutText(), "/flow save") {
		t.Fatalf("rebuilt three-green segment must hint again:\n%s", m.cmdOutText())
	}
}

// TestFlowHintSkipsChatOnlyPrompts: a prompt with no tool sequence has
// no routine to freeze — it never votes, never writes the ledger and
// never hints.
func TestFlowHintSkipsChatOnlyPrompts(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))
	t.Cleanup(resetFlowHint)
	resetFlowHint()
	m := newTestModel(t)

	for round := 1; round <= 4; round++ {
		fhRound(m, round, nil, "")
		if strings.Contains(m.cmdOutText(), "/flow save") {
			t.Fatalf("chat-only round %d hinted:\n%s", round, m.cmdOutText())
		}
	}
	if len(streakLedger(t)) != 0 {
		t.Fatalf("chat-only rounds wrote the ledger: %v", streakLedger(t))
	}
}

// TestFlowHintAutoFreezeOff: LYON_AUTOFREEZE=off opts the TUI out of
// the whole mechanism, mirroring the CLI runner — no votes, no ledger,
// no hint.
func TestFlowHintAutoFreezeOff(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))
	t.Setenv("LYON_AUTOFREEZE", "off")
	t.Cleanup(resetFlowHint)
	resetFlowHint()
	m := newTestModel(t)

	for round := 1; round <= 5; round++ {
		fhRound(m, round, []string{"read_file", "run_shell"}, "")
		if strings.Contains(m.cmdOutText(), "/flow save") {
			t.Fatalf("round %d hinted despite LYON_AUTOFREEZE=off:\n%s",
				round, m.cmdOutText())
		}
	}
	if len(streakLedger(t)) != 0 {
		t.Fatalf("LYON_AUTOFREEZE=off still wrote the ledger: %v", streakLedger(t))
	}
}
