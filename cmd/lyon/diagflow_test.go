package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lyon-agent/pkg/eval"
	"lyon-agent/pkg/hooks"
	"lyon-agent/pkg/memory"
	"lyon-agent/pkg/tools"
)

// diagTest returns a diagnosis carrying both a checkable condition and
// the patch a retry would inherit.
func diagTest() eval.Diagnosis {
	return eval.Diagnosis{
		FailedConditions: []string{"go build ./... never ran before done"},
		Patch:            "always run go build before finishing",
	}
}

// TestRememberDiagnosisWriteback: a diagnosis lands as one hot
// project fact, the write audits EvMemorySave (never EvDiagnosis), and
// the next session's injection path — tools.MemoryFacts — carries the
// patch text. A repeat diagnoses the same lesson: no second fact, no
// second audit line.
func TestRememberDiagnosisWriteback(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))
	ws := tempDir(t)
	d := diagTest()

	var recs []hooks.Record
	fire := func(r hooks.Record) { recs = append(recs, r) }

	if err := rememberDiagnosis(ws, fire, d); err != nil {
		t.Fatalf("writeback: %v", err)
	}
	if len(recs) != 1 || recs[0].Event != hooks.EvMemorySave || recs[0].Tool == "" {
		t.Fatalf("audit after write: %+v", recs)
	}
	if strings.Contains(recs[0].Tool, hooks.EvDiagnosis) {
		t.Fatalf("memory audit must not ride EvDiagnosis: %+v", recs[0])
	}

	st := memory.Open(ws)
	facts := st.List(memory.ScopeProject)
	if len(facts) != 1 {
		t.Fatalf("want 1 project fact, got %d", len(facts))
	}
	if !facts[0].Hot || !strings.Contains(facts[0].Body, d.Patch) {
		t.Fatalf("fact not hot or patch missing: %+v", facts[0])
	}
	if inj := tools.MemoryFacts(ws); !strings.Contains(inj, d.Patch) {
		t.Fatalf("MemoryFacts misses the patch:\n%s", inj)
	}

	if err := rememberDiagnosis(ws, fire, d); err != nil {
		t.Fatalf("repeat writeback: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("duplicate wrote again: %d audits", len(recs))
	}
	if n := len(st.List(memory.ScopeProject)); n != 1 {
		t.Fatalf("duplicate stacked: %d facts", n)
	}
}

// TestDiagDoneBranchWriteback: the diagDone branch itself performs the
// writeback — point the model at a scratch workspace first so the test
// fact lands in a temp store, never the source tree.
func TestDiagDoneBranchWriteback(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))
	ws := tempDir(t)
	m := newTestModel(t)
	m.workspace = ws

	step := m.pushAsyncStep(stepDiagnose, "diagnose", "post-mortem of the failed run…")
	d := diagTest()
	m.Update(diagDone{step: step, diag: d})

	if inj := tools.MemoryFacts(ws); !strings.Contains(inj, d.Patch) {
		t.Fatalf("MemoryFacts after diagDone misses the patch:\n%s", inj)
	}

	// The writeback audits memory_save (non-EvDiagnosis) alongside the
	// diagnosis line the same branch already fires.
	m.sess.Hooks().Flush()
	files, _ := filepath.Glob(filepath.Join(os.Getenv("LYON_HOME"), "audit", "*.jsonl"))
	var sawMemorySave, sawDiagnosis bool
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		text := string(data)
		if strings.Contains(text, `"event":"memory_save"`) &&
			strings.Contains(text, `"tool":"`+memory.DiagCategory+"-") {
			sawMemorySave = true
		}
		if strings.Contains(text, `"event":"diagnosis"`) {
			sawDiagnosis = true
		}
	}
	if !sawMemorySave {
		t.Fatalf("memory writeback not audited (files: %v)", files)
	}
	if !sawDiagnosis {
		t.Fatalf("the diagnosis line itself must stay in the audit (files: %v)", files)
	}
}
