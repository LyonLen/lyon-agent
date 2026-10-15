package main

// §3.1 in the TUI: the auto-freeze clock that used to tick only under
// `lyon run --flow` (evolve_cli.go). Each finished TUI prompt votes on
// its run's tool-sequence class through flow.BumpStreak — the SAME
// function and the SAME $LYON_HOME/flow-streak.json the CLI path uses —
// and once a class has gone green autoFreezeStreak times straight, the
// answer area gets a one-line hint offering `/flow save <slug>`.
// It only suggests: §1.4 keeps the closed-book exam gate in front of
// every freeze, so this file never saves anything itself.

import (
	"fmt"
	"hash/fnv"
	"os"
	"strings"

	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/flow"
)

// flowHintState is the process-local view of the current consecutive
// segment: which tool-sequence class the session last voted on, and
// whether its hint already showed. Deliberately not persisted — the
// durable count lives in flow-streak.json (BumpStreak writes it), and a
// fresh process re-hinting an unsaved, still-green class is desirable.
type flowHintState struct {
	slug   string // streak key of the current segment
	hinted bool   // the freeze hint already showed within this segment
}

// fhState is UI-thread state: flowHint is only called from Update's
// prompt_end branch (and tests on the same goroutine), no locking.
var fhState flowHintState

// resetFlowHint drops the segment so tests start from a clean clock.
func resetFlowHint() { fhState = flowHintState{} }

// runToolSig returns the run's tool names in first-occurrence order —
// only rows after the newest stepPrompt group, which IS this run's
// group (a mid-run queue injection adds no panel row). Repeats collapse
// to their first sighting: read→edit→read→edit has the same shape as
// read→edit, so retries inside one run never fork the class. Server
// tools and subagent task nodes count (they are part of the routine's
// shape); inner subagent executions (rows hanging under a task) do not
// — a dispatch's internals are its own business, and their arrival
// order is concurrent noise. Non-tool rows (memory, diagnose, eval)
// never vote.
func runToolSig(steps []execStep) []string {
	start := 0
	for i := len(steps) - 1; i >= 0; i-- {
		if steps[i].kind == stepPrompt {
			start = i + 1
			break
		}
	}
	var sig []string
	seen := make(map[string]bool, 8)
	for _, s := range steps[start:] {
		if s.under != 0 || s.name == "" {
			continue
		}
		if s.kind != stepTool && s.kind != stepServer && s.kind != stepSubagent {
			continue
		}
		if seen[s.name] {
			continue
		}
		seen[s.name] = true
		sig = append(sig, s.name)
	}
	return sig
}

// streakSlug maps a tool signature onto a stable, slug-safe streak key.
// Rule: up to three sanitized tool tokens (lowercase, '_'→'-', non
// [a-z0-9_-] dropped, 16-char cap each) form the readable head that the
// hint line shows, then six hex digits of FNV-1a over the FULL
// signature carry the discrimination — the same routine always hashes
// to the same slug; any difference in tools, order, or a longer run's
// tail lands on a different one. Output stays inside flow.slugify's
// [a-z0-9_-] character class, so `/flow save <slug>` stores exactly
// the slug the hint named.
func streakSlug(sig []string) string {
	toks := make([]string, 0, 3)
	for _, name := range sig {
		if len(toks) == 3 {
			break
		}
		if t := slugToken(name); t != "" {
			toks = append(toks, t)
		}
	}
	head := strings.Join(toks, "-")
	if head == "" {
		head = "run"
	}
	h := fnv.New64a()
	for _, name := range sig {
		_, _ = h.Write([]byte(name))
		_, _ = h.Write([]byte{0}) // separator: [a,b] must not hash like [ab]
	}
	return fmt.Sprintf("%s-%06x", head, h.Sum64()&0xffffff)
}

// slugToken sanitizes one tool name into a slug fragment.
func slugToken(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	t := strings.ReplaceAll(b.String(), "_", "-")
	if len(t) > 16 {
		t = t[:16]
	}
	return strings.Trim(t, "-")
}

// flowHint ticks the auto-freeze clock for one finished prompt and
// pushes the freeze hint once a tool-sequence class has gone green
// autoFreezeStreak runs in a row. Wired into the prompt_end branch —
// before deferred actions can clear the steps panel.
//
//   - vote: flow.BumpStreak(slug, ok) with ok = prompt_end carries no
//     error — the CLI's own ledger, self-persisting; this file never
//     writes the file itself;
//   - class change breaks the old segment: BumpStreak(old, false) is
//     the reset (consecutive means consecutive) and the new slug starts
//     from its own ledger entry;
//   - a red run (ev.Err != "") zeroes its own slug and re-arms the
//     hint, so a later three-green rebuild can hint again;
//   - runs with no tool sequence don't vote at all — a chat-only turn
//     has no routine to freeze and neither advances nor breaks counts;
//   - LYON_AUTOFREEZE=off skips everything, the exact switch the CLI
//     runner honors (there is no LYON_FLOW switch in pkg/flow).
//
// One hint per segment: greens 4, 5, 6 … stay quiet until a break
// (red run or class change) brings the class back to the bar.
func (m *tuiModel) flowHint(ev agent.Event) {
	if strings.EqualFold(os.Getenv("LYON_AUTOFREEZE"), "off") {
		return
	}
	sig := runToolSig(m.steps)
	if len(sig) == 0 {
		return
	}
	slug := streakSlug(sig)
	if slug != fhState.slug {
		if fhState.slug != "" {
			// Class switched: the old class's straight-green run ends here.
			_ = flow.BumpStreak(fhState.slug, false)
		}
		fhState = flowHintState{slug: slug}
	}
	ok := ev.Err == ""
	n := flow.BumpStreak(slug, ok)
	if !ok {
		fhState.hinted = false // red breaks the segment — re-arm
		return
	}
	if n >= autoFreezeStreak && !fhState.hinted {
		fhState.hinted = true
		m.pushCmd(paint(cCyan, fmt.Sprintf(tr(
			"❄ same routine green %d runs — freeze it: /flow save %s",
			"❄ 该套路已连续 %d 次成功，可固化：/flow save %s"), n, slug)))
	}
}
