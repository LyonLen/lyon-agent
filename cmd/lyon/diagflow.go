package main

import (
	"fmt"
	"strings"

	"lyon-agent/pkg/eval"
	"lyon-agent/pkg/hooks"
	"lyon-agent/pkg/memory"
	"lyon-agent/pkg/space"
)

// rememberDiagnosis is the diagDone memory writeback (断点 2): the
// post-mortem joins the project memory layer, deduped by content and
// pinned into the hot tier, so the NEXT session's MemoryFacts
// injection carries the lesson instead of losing it with this answer.
// A real write audits one EvMemorySave line — deliberately not
// EvDiagnosis, which already records the diagnosis itself; duplicates
// and empty post-mortems write nothing and stay silent. Best-effort by
// design (like distill): the error is returned so tests and the caller
// can see a failed write, never to block the answer.
func rememberDiagnosis(workspace string, fire func(hooks.Record), d eval.Diagnosis) error {
	f, saved, err := memory.Open(workspace).SaveDiagnosis(d.FailedConditions, d.Patch)
	if err != nil {
		return err
	}
	if !saved {
		return nil // duplicate or empty — nothing written, nothing to audit
	}
	if fire != nil {
		fire(hooks.Record{
			Event:  hooks.EvMemorySave,
			Tool:   f.Name,
			Detail: fmt.Sprintf("%s scope, hot — %s writeback", f.Scope, memory.DiagCategory),
		})
	}
	return nil
}

// rememberDiagnosisSpace is the self-evolution writeback (断点 2b): the
// same post-mortem that joins project memory also lands in the CURRENT
// agent's specialist space — the agent that hit the failure owns the
// lesson about the failure. Deduped by the space layer's name
// collision (same name+body overwrites in place). Best-effort: a failed
// space write never blocks the answer, mirroring rememberDiagnosis.
func rememberDiagnosisSpace(agent string, fire func(hooks.Record), d eval.Diagnosis) {
	if len(d.FailedConditions) == 0 && strings.TrimSpace(d.Patch) == "" {
		return
	}
	name := "diagnosis"
	if len(d.FailedConditions) > 0 {
		name = "diagnosis: " + d.FailedConditions[0]
	}
	body := strings.TrimSpace(d.Patch)
	if len(d.FailedConditions) > 0 {
		body = "Failed:\n- " + strings.Join(d.FailedConditions, "\n- ") + "\n\nFix:\n" + body
	}
	it, err := space.SaveKnowledge(agent, name,
		"lesson from a failed diagnosis", body)
	if err != nil {
		return // best-effort, like the memory writeback
	}
	if fire != nil {
		fire(hooks.Record{
			Event:  hooks.EvMemorySave,
			Tool:   "space",
			Detail: fmt.Sprintf("agent %s — diagnosis lesson saved", it.Name),
		})
	}
}
