package main

import (
	"fmt"

	"lyon-agent/pkg/eval"
	"lyon-agent/pkg/hooks"
	"lyon-agent/pkg/memory"
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
