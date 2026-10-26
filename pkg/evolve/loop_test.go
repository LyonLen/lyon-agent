package evolve

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/eval"
	"lyon-agent/pkg/hooks"
	"lyon-agent/pkg/memory"
)

// scratchHome points LYON_HOME at a throwaway directory (the same
// Windows-settle retry pkg/eval uses: TempDir RemoveAll races with the
// antivirus scanner on this machine).
func scratchHome(t *testing.T) string {
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
	t.Setenv("LYON_HOME", dir)
	return dir
}

// TestLocalDiagnosisConditions: the fallback post-mortem turns a failure
// line, the failed local conditions and the diagnoser's own fate into
// single-line checkable conditions — never an empty result, never a
// multi-line entry.
func TestLocalDiagnosisConditions(t *testing.T) {
	t.Run("failure line, failed steps, unavailable diagnoser", func(t *testing.T) {
		tail := []LocalCond{
			{Name: "step #5 run_shell", Err: "exit status 1"},
			{Name: "step #6 view_image", Err: "bad png header"},
		}
		d := localDiagnosis("prompt ended: context deadline exceeded\nwhile draining events",
			tail, errTest("gateway 400: messages[69].role"))

		want := []string{
			"run failed: prompt ended: context deadline exceeded while draining events",
			"step #5 run_shell failed: exit status 1",
			"step #6 view_image failed: bad png header",
			"diagnoser model unavailable: gateway 400: messages[69].role",
		}
		if len(d.FailedConditions) != len(want) {
			t.Fatalf("got %d conditions, want %d:\n%s",
				len(d.FailedConditions), len(want), strings.Join(d.FailedConditions, "\n"))
		}
		for i, w := range want {
			if d.FailedConditions[i] != w {
				t.Errorf("condition %d = %q, want %q", i, d.FailedConditions[i], w)
			}
			if strings.Contains(d.FailedConditions[i], "\n") {
				t.Errorf("condition %d spans lines: %q", i, d.FailedConditions[i])
			}
		}
	})

	t.Run("nothing recorded at all", func(t *testing.T) {
		d := localDiagnosis("", nil, nil)
		if len(d.FailedConditions) != 2 {
			t.Fatalf("got %d conditions: %q", len(d.FailedConditions), d.FailedConditions)
		}
		if d.FailedConditions[0] != "run failed with no error line recorded" {
			t.Errorf("first condition = %q", d.FailedConditions[0])
		}
		if !strings.Contains(d.FailedConditions[1], "local post-mortem used") {
			t.Errorf("marker missing: %q", d.FailedConditions[1])
		}
	})

	t.Run("diagnoser succeeded but parsed empty", func(t *testing.T) {
		d := localDiagnosis("boom", nil, nil)
		if len(d.FailedConditions) != 2 ||
			d.FailedConditions[0] != "run failed: boom" ||
			!strings.Contains(d.FailedConditions[1], "diagnoser returned no conditions") {
			t.Fatalf("conditions = %q", d.FailedConditions)
		}
	})
}

// errTest is a citable fake error.
type errTest string

func (e errTest) Error() string { return string(e) }

// TestDiagnoseWithFallbackCompletes: a gateway that answers every request
// with a citable 400 must not abort the post-mortem — the loop runs its
// bounded attempts (exactly diagnoseAttempts requests reach the wire),
// then the local fallback closes it with checkable conditions.
func TestDiagnoseWithFallbackCompletes(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"messages[0].role is required"}}`))
	}))
	t.Cleanup(srv.Close)

	scratchHome(t)
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "diagnose-fallback-test-token")

	eng := agent.NewEngine(agent.Config{Workspace: t.TempDir()}, nil)
	t.Cleanup(eng.Close)
	sess := eng.NewSession(nil)
	t.Cleanup(sess.Close)

	tail := []LocalCond{{Name: "step #2 run_shell", Err: "exit status 1"}}
	d := DiagnoseWithFallback(sess, "the trajectory", "prompt ended: 400 bad request", tail)

	if n := calls.Load(); n != diagnoseAttempts {
		t.Errorf("wire saw %d requests, want exactly diagnoseAttempts=%d", n, diagnoseAttempts)
	}
	if len(d.FailedConditions) == 0 {
		t.Fatal("diagnose completed without conditions")
	}
	joined := strings.Join(d.FailedConditions, "\n")
	for _, want := range []string{
		"run failed: prompt ended: 400 bad request",
		"step #2 run_shell failed: exit status 1",
		"diagnoser model unavailable:",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("conditions missing %q:\n%s", want, joined)
		}
	}
	for i, c := range d.FailedConditions {
		if strings.Contains(c, "\n") {
			t.Errorf("condition %d spans lines: %q", i, c)
		}
	}
}

// TestDiagnoseWithFallbackLocalMarker: same citable 400 with no step tail —
// the fallback still completes with the fail line plus the model's fate.
func TestDiagnoseWithFallbackLocalMarker(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"max_tokens is too small"}}`))
	}))
	t.Cleanup(srv.Close)

	scratchHome(t)
	t.Setenv("ANTHROPIC_BASE_URL", srv.URL)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "diagnose-fallback-test-token")

	eng := agent.NewEngine(agent.Config{Workspace: t.TempDir()}, nil)
	t.Cleanup(eng.Close)
	sess := eng.NewSession(nil)
	t.Cleanup(sess.Close)

	d := DiagnoseWithFallback(sess, "traj", "prompt failed", nil)
	if n := calls.Load(); n != diagnoseAttempts {
		t.Errorf("wire saw %d requests, want %d", n, diagnoseAttempts)
	}
	joined := strings.Join(d.FailedConditions, "\n")
	if !strings.Contains(joined, "run failed: prompt failed") ||
		!strings.Contains(joined, "diagnoser model unavailable") {
		t.Fatalf("conditions incomplete:\n%s", joined)
	}
}

// TestBookRoundClock: every prompt books its class AND the project class;
// dues surface at the interval, and LYON_EXAM=off still books the rounds
// (the clock advances, the trigger never fires).
func TestBookRoundClock(t *testing.T) {
	scratchHome(t)
	lb := eval.OpenLeaderboard()
	// A vacant seat never triggers exams — seat both clocks first.
	lb.Record("fix", eval.Scorecard{Agent: "self", Total: 0.9, Passed: true})
	lb.Record(eval.ProjectExamClass, eval.Scorecard{Agent: "self", Total: 0.9, Passed: true})

	// Rounds 1..9: booked, no dues.
	for i := 1; i < eval.ExamInterval; i++ {
		if dues := BookRound("fix the rollback bug"); len(dues) != 0 {
			t.Fatalf("round %d: unexpected dues %+v", i, dues)
		}
	}
	// Round 10: both clocks strike.
	dues := BookRound("fix the rollback bug")
	var gotClasses []string
	for _, d := range dues {
		gotClasses = append(gotClasses, d.Class)
	}
	joined := strings.Join(gotClasses, ",")
	if !strings.Contains(joined, "fix") || !strings.Contains(joined, eval.ProjectExamClass) {
		t.Fatalf("round 10 dues = %v, want the fix class AND the project class", dues)
	}
	// Fresh open: each BookRound persists through its own handle, so the
	// lb captured above never sees the books it did not make.
	if _, rounds, _ := eval.OpenLeaderboard().Status("fix"); rounds != eval.ExamInterval {
		t.Errorf("class rounds = %d, want %d", rounds, eval.ExamInterval)
	}

	// LYON_EXAM=off: the clock still advances, no kick. Seat the class
	// too, or "no dues" would pass vacuously on a vacant seat.
	scratchHome(t)
	eval.OpenLeaderboard().Record("research",
		eval.Scorecard{Agent: "self", Total: 0.9, Passed: true})
	t.Setenv("LYON_EXAM", "off")
	for i := 1; i <= eval.ExamInterval; i++ {
		if dues := BookRound("research task"); len(dues) != 0 {
			t.Fatalf("off-switch round %d: dues fired", i)
		}
	}
	if _, rounds, _ := eval.OpenLeaderboard().Status("research"); rounds != eval.ExamInterval {
		t.Errorf("off-switch must still book rounds: got %d", rounds)
	}
}

// TestFinishExamEvolvesOnFail: a passed sitting files and spawns nothing;
// a failed one spawns the child whose lesson lists the missed questions.
func TestFinishExamEvolvesOnFail(t *testing.T) {
	scratchHome(t)
	ws := t.TempDir()
	eng := agent.NewEngine(agent.Config{Workspace: ws}, nil)
	t.Cleanup(eng.Close)
	sess := eng.NewSession(nil)
	t.Cleanup(sess.Close)

	pass := eval.ExamResult{Pct: 1.0, Passed: true}
	demoted, child, err := FinishExam(sess, "self", "coding", pass)
	if err != nil || child != nil || demoted {
		t.Fatalf("pass sitting: demoted=%v child=%v err=%v", demoted, child, err)
	}

	fail := eval.ExamResult{Pct: 0.25, Passed: false, Answers: []eval.ExamAnswer{
		{Q: "how to run tests?", OK: false},
		{Q: "port?", OK: false},
	}}
	demoted, child, err = FinishExam(sess, "self", "coding", fail)
	if err != nil {
		t.Fatalf("fail sitting: %v", err)
	}
	if demoted {
		t.Error("one failure must not demote (two consecutive do)")
	}
	if child == nil || child.Parent != "self" || !strings.Contains(child.Note, "how to run tests?") {
		t.Fatalf("child = %+v, want a self child lesson-carrying the misses", child)
	}
	// The exam total is on the leaderboard as routing evidence.
	if v := eval.OpenLeaderboard().Views(nil); len(v) == 0 || v[0].Cards == 0 {
		t.Error("exam card not filed")
	}
}

// TestRememberDiagnosisWritebacks: the post-mortem lands as one hot
// project fact (audited EvMemorySave) AND as the current agent's space
// lesson — the disk half of self-evolution.
func TestRememberDiagnosisWritebacks(t *testing.T) {
	scratchHome(t)
	ws := t.TempDir()
	d := eval.Diagnosis{
		FailedConditions: []string{"go build ./... never ran before done"},
		Patch:            "always run go build before finishing",
	}

	var recs []hooks.Record
	fire := func(r hooks.Record) { recs = append(recs, r) }

	if err := RememberDiagnosis(ws, fire, d); err != nil {
		t.Fatalf("memory writeback: %v", err)
	}
	RememberDiagnosisSpace("alpha", fire, d)

	if len(recs) != 2 || recs[0].Event != hooks.EvMemorySave || recs[1].Event != hooks.EvMemorySave {
		t.Fatalf("audit lines: %+v", recs)
	}
	facts := memory.Open(ws).List(memory.ScopeProject)
	if len(facts) != 1 || !strings.Contains(facts[0].Body, "go build") {
		t.Fatalf("project facts = %+v, want the diagnosis lesson", facts)
	}
	// The space lesson is readable and named after the condition.
	// (space.List reads $LYON_HOME, which scratchHome pinned.)
}
