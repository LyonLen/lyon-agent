// Package evolve is the shared evolution loop (docs/design/agent-evolution.md)
// — the computation half of cmd/lyon's kicks, sunk out of the TUI event
// ring so every entry point runs the same loop (BACKLOG §2.5: a lyond/web
// session must book rounds, grade, sit exams and spawn the next
// generation exactly like a TUI session). The TUI keeps only the panel
// half (async steps, tea messages) and aliases these units; the daemon
// wires Hook into its prompt_end tap.
//
// Every unit here is blocking (model calls take seconds) — callers spawn
// goroutines. The LYON_SCORE / LYON_EXAM / LYON_DIAGNOSE gates live with
// the units they gate, so one implementation serves both frontends.
package evolve

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/eval"
	"lyon-agent/pkg/hooks"
	"lyon-agent/pkg/market"
	"lyon-agent/pkg/memory"
	"lyon-agent/pkg/routing"
	"lyon-agent/pkg/space"
)

// examMaxTokens bounds one closed-book answer: a paragraph of facts,
// with headroom for the subagent-tier model's visible reasoning (mimo
// models think before answering — a too-tight cap truncates the reply
// before any answer text appears).
const examMaxTokens = 1500

// judgeMaxTokens bounds one blind grade: the grade itself, plus ample
// room for the subagent-tier model's visible reasoning (mimo models
// routinely think for hundreds of tokens before answering — a tight cap
// truncates the reply into a thinking-only response with no grade;
// 1024 was still eaten whole by thinking on longer review answers,
// so the budget went to 4096 — the grade line itself is tiny).
const judgeMaxTokens = 4096

// diagnoseMaxTokens bounds the post-mortem reply: a handful of conditions
// plus one patch sentence, as JSON.
const diagnoseMaxTokens = 1000

// The post-mortem must COMPLETE: a deadline or a poisoned payload must
// not end the diagnosis as a bare error. Two bounded model attempts,
// then a local fallback mines the run's own record — the result always
// carries at least one checkable condition.
const (
	diagnoseAttempts = 2
	diagnoseTimeout  = 60 * time.Second
)

// ── model-call adapters ───────────────────────────────────────────

// LoadExam picks a class's held-out bank: the project class is the
// dynamic, workspace-generated exam (its questions come from go.mod,
// README, cmd/ and the project memory layer); every other class uses
// the frozen seed bank.
func LoadExam(workspace, class string) (eval.Exam, error) {
	if class == eval.ProjectExamClass {
		return eval.LoadProjectExam(workspace)
	}
	return eval.LoadOrSeedExam(class)
}

// ExamAnswerer answers one question with the exam system prompt —
// no tools, no files, weights only. The project class also carries the
// standing project context so its questions are answerable closed-book.
func ExamAnswerer(sess *agent.Session, ex eval.Exam) eval.Answerer {
	sys := eval.ProjectSystem(ex, sess.Workspace())
	return func(ctx context.Context, q string) (string, error) {
		return sess.Ask(ctx, sys, q, examMaxTokens)
	}
}

// ExamAnswererOn answers closed-book on a specific candidate's model —
// the grading channel of the contest: each entrant must face the same
// held-out bank on ITS OWN model, or the result grades the wrong agent.
func ExamAnswererOn(sess *agent.Session, model string, ex eval.Exam) eval.Answerer {
	sys := eval.ProjectSystem(ex, sess.Workspace())
	return func(ctx context.Context, q string) (string, error) {
		return sess.AskModel(ctx, model, sys, q, examMaxTokens)
	}
}

// ── production scoring (design §1.2) ──────────────────────────────

// LastAssistantText pulls the newest assistant prose for the judge —
// scanning back past tool-only turns. An all-tools run yields "" (the
// judge is told the run produced no text).
func LastAssistantText(msgs []anthropic.BetaMessageParam) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != anthropic.BetaMessageParamRoleAssistant {
			continue
		}
		var parts []string
		for _, blk := range msgs[i].Content {
			if blk.OfText != nil && strings.TrimSpace(blk.OfText.Text) != "" {
				parts = append(parts, blk.OfText.Text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	return ""
}

// BookPlainRun grades a completed free-form prompt (design §1.2: no
// ground truth → the judge model blind-grades), files the production
// scorecard to the leaderboard and the total to the routing table —
// the evidence both the on-duty election and /route read. A judge
// failure files nothing, because a score of convenience is worse than
// no score. Returns a display line.
func BookPlainRun(ctx context.Context, sess *agent.Session, model, prompt string, steps int) (string, error) {
	answer := LastAssistantText(sess.Export().Messages)
	class := string(routing.Classify(prompt))
	judge, err := eval.JudgeRun(ctx, func(ctx context.Context, system, user string) (string, error) {
		return sess.Ask(ctx, system, user, judgeMaxTokens)
	}, prompt, answer)
	if err != nil {
		return "", err
	}
	sc := eval.Score(model, class, eval.RunMetrics{
		Correctness: judge,
		Steps:       steps,
	})
	eval.OpenLeaderboard().Record(class, sc)
	routing.Open().Record(class, model, model, sc.Total)
	return fmt.Sprintf("score %.2f (judge %.0f%%) → leaderboard [%s]",
		sc.Total, judge*100, class), nil
}

// ── the §1.4 exam clock ───────────────────────────────────────────

// Due names an exam that became due with the round that struck it.
type Due struct {
	Class string
	Why   string
}

// BookRound books one production round for the prompt's task class and
// for the project exam clock (the dynamic bank belongs to no task class
// but must still sit on schedule), returning the exams that became due.
// LYON_EXAM=off opts out of the trigger only: both rounds still count.
func BookRound(prompt string) []Due {
	if strings.TrimSpace(prompt) == "" {
		return nil
	}
	class := string(routing.Classify(prompt))
	lb := eval.OpenLeaderboard()
	rounds, due := lb.CountRound(class)
	projRounds, projDue := lb.CountRound(eval.ProjectExamClass)
	if strings.EqualFold(os.Getenv("LYON_EXAM"), "off") {
		return nil
	}
	var dues []Due
	if due {
		dues = append(dues, Due{Class: class,
			Why: fmt.Sprintf("round %d/%d — exam due", rounds, eval.ExamInterval)})
	}
	if projDue {
		dues = append(dues, Due{Class: eval.ProjectExamClass,
			Why: fmt.Sprintf("round %d/%d — project exam due", projRounds, eval.ExamInterval)})
	}
	return dues
}

// FinishExam files one closed-book sitting: the scorecard joins the
// leaderboard (two straight failures demote the holder), the total is
// routing evidence, and a failed exam distills into the next generation
// (§7.3: the missed questions are the lesson). Returns the demotion
// flag and the spawned child (nil when the exam passed or the lineage
// is exhausted).
func FinishExam(sess *agent.Session, agentID, class string, res eval.ExamResult) (demoted bool, child *eval.Candidate, err error) {
	lb := eval.OpenLeaderboard()
	demoted = lb.SubmitExam(class, eval.Scorecard{
		Agent: agentID, Total: res.Pct, Passed: res.Passed, Source: "exam",
	})
	routing.Open().Record(class, agentID, agentID, res.Pct)
	if res.Passed {
		return demoted, nil, nil
	}
	c, cerr := EvolveFromExamFail(sess, res)
	if cerr != nil {
		return demoted, nil, cerr
	}
	return demoted, &c, nil
}

// ── contest — 同类竞争上岗 (#53) ──────────────────────────────────

// ContestRow is one candidate's held-out result.
type ContestRow struct {
	ID     string
	Model  string
	Pct    float64
	Passed bool
}

// ContestNoFieldHint is the actionable answer for a contest with no one
// to elect between — the field is the built-in self incumbent alone.
func ContestNoFieldHint() string {
	return "no candidates beyond self — create one first: /agents add <id> <model> [effort]"
}

// RunContest is the contest's whole computation — candidates against the
// class's held-out bank CONCURRENTLY, every card filed, one election on
// the combined evidence, routing steered to the winner, and every evolved
// child that fails to beat its parent on this sitting deleted (§7.3).
func RunContest(ctx context.Context, sess *agent.Session, selfModel, class string) ([]ContestRow, string, error) {
	cands := eval.Contestants(selfModel)
	// Contestants always carries the live self incumbent, so a field of
	// one means zero external candidates: an election of one decides
	// nothing. Refuse before LoadExam, so a refused contest seeds no
	// held-out bank.
	if len(cands) <= 1 {
		return nil, "", errors.New(ContestNoFieldHint())
	}
	ex, err := LoadExam(sess.Workspace(), class)
	if err != nil {
		return nil, "", err
	}
	rows := make([]ContestRow, len(cands))
	var wg sync.WaitGroup
	for i, c := range cands {
		wg.Add(1)
		go func(i int, c eval.Candidate) {
			defer wg.Done()
			res := eval.RunExam(ctx, ex, ExamAnswererOn(sess, c.Model, ex))
			rows[i] = ContestRow{ID: c.ID, Model: c.Model, Pct: res.Pct, Passed: res.Passed}
		}(i, c)
	}
	wg.Wait()
	// File every card (the sitting agent's streak only moves on their
	// own exams — leaderboard rule), then hold ONE election on the
	// combined evidence instead of letting submission order pick.
	lb := eval.OpenLeaderboard()
	for _, r := range rows {
		lb.SubmitExam(class, eval.Scorecard{
			Agent: r.ID, Total: r.Pct, Passed: r.Passed, Source: "exam",
		})
	}
	winner := lb.Elect(class)
	// Route production traffic to the winner: agent id + its model,
	// evidence = this exam total. No winner = leave the table alone.
	for _, r := range rows {
		if r.ID == winner {
			routing.Open().Record(class, r.ID, r.Model, r.Pct)
			// 共识共享: winning a held-out election is the strongest
			// endorsement there is — the winner's whole specialist space
			// shares to commons (items reaching 2 sources promote).
			space.ShareAll(winner, "contest:"+class)
			break
		}
	}
	// 自己进化's other half: every evolved child in the field must beat
	// its parent on THIS held-out sitting, or the lineage is cut —
	// 每代必须比上代分数高，否则淘汰. Both must have sat the same exam
	// for the comparison to mean anything; a parent absent from the
	// field leaves the child to the election alone.
	scoreOf := func(id string) (float64, bool) {
		for _, r := range rows {
			if r.ID == id {
				return r.Pct, true
			}
		}
		return 0, false
	}
	for _, c := range cands {
		if c.Gen == 0 || c.Parent == "" {
			continue
		}
		parentPct, parentSat := scoreOf(c.Parent)
		childPct, _ := scoreOf(c.ID)
		if parentSat && childPct <= parentPct {
			_ = eval.DeleteCandidate(c.ID) // best-effort; the row still reports the score
		}
	}
	return rows, winner, nil
}

// ── routing consumption (design §5) ───────────────────────────────

// RoutingSwap is design §5's consumption side: for an unpinned run,
// consult the routing table for the prompt's class and recommend the
// on-duty model once its seat has earned confidence (≥3 samples and
// confidence ≥0.35 — one lucky try never steers traffic). Returns the
// model to use and the entry that decided it.
func RoutingSwap(current, prompt string) (string, routing.Entry, bool) {
	class := string(routing.Classify(prompt))
	e, ok := routing.Open().Lookup(class)
	if !ok || e.Model == "" || e.Model == current || e.Samples < 3 || e.Confidence < 0.35 {
		return current, e, false
	}
	return e.Model, e, true
}

// AdoptAgent binds the session to a routed specialist when its seat has
// earned the evidence gate (same bar RoutingSwap applies): the session
// identity switches — the space tool then writes into THIS agent's
// store — and the model is told what its space carries via a system
// note at the next turn boundary. Adopting the identity even when the
// specialist's model equals the session's is deliberate: a one-model
// gateway still wants the right persona and the right space.
func AdoptAgent(sess *agent.Session, e routing.Entry) {
	if sess == nil || e.Agent == "" || e.Agent == sess.Agent() || e.Samples < 3 || e.Confidence < 0.35 {
		return
	}
	sess.SetAgent(e.Agent)
	if brief := space.Brief(e.Agent); brief != "" {
		sess.PushSystemNote(brief)
	}
}

// ── self-evolution spawns (2026-10-23 重定义) ─────────────────────

// EvolveFromDiagnosis turns a post-mortem into the next generation of
// the CURRENT agent: the child inherits its space and carries the
// failed conditions + patch as its first lesson. Best-effort by
// design — an exhausted lineage (gen ≥ 3) or a disk failure errors;
// the caller decides whether that is worth surfacing.
func EvolveFromDiagnosis(sess *agent.Session, d eval.Diagnosis) (eval.Candidate, error) {
	lesson := d.Patch
	if len(d.FailedConditions) > 0 {
		lesson = "failed: " + strings.Join(d.FailedConditions, "; ") + " — fix: " + d.Patch
	}
	if strings.TrimSpace(lesson) == "" {
		return eval.Candidate{}, errors.New("evolve: empty diagnosis")
	}
	return eval.EvolveChild(sess.Agent(), lesson)
}

// EvolveFromExamFail turns a failed closed-book exam into the next
// generation: the lesson is the list of missed questions — exactly what
// the parent could not answer.
func EvolveFromExamFail(sess *agent.Session, res eval.ExamResult) (eval.Candidate, error) {
	var missed []string
	for _, a := range res.Answers {
		if !a.OK {
			missed = append(missed, a.Q)
		}
	}
	if len(missed) == 0 {
		return eval.Candidate{}, errors.New("evolve: exam passed — nothing to distill")
	}
	lesson := fmt.Sprintf("could not answer closed-book (%.0f%%): %s",
		res.Pct*100, strings.Join(missed, "; "))
	return eval.EvolveChild(sess.Agent(), lesson)
}

// ── failure post-mortem (design §1.3) ─────────────────────────────

// LocalCond is one pre-extracted failure the local fallback can cite
// when the diagnoser model is unavailable (the TUI's step tail, the
// daemon's event trail).
type LocalCond struct {
	Name string
	Err  string
}

// DiagnoseWithFallback is the bounded post-mortem loop: up to
// diagnoseAttempts model calls, then the local fallback mines the
// caller's pre-extracted conditions — the return always carries at
// least one checkable condition, so a post-mortem completes instead of
// dying on a deadline or a 400.
func DiagnoseWithFallback(sess *agent.Session, traj, fail string, tail []LocalCond) eval.Diagnosis {
	var d eval.Diagnosis
	var err error
	for attempt := 0; attempt < diagnoseAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), diagnoseTimeout)
		d, err = eval.SummarizeFailure(ctx,
			func(ctx context.Context, system, user string) (string, error) {
				return sess.Ask(ctx, system, user, diagnoseMaxTokens)
			}, traj)
		cancel()
		if err == nil && len(d.FailedConditions) > 0 {
			break
		}
	}
	if err != nil || len(d.FailedConditions) == 0 {
		d = localDiagnosis(fail, tail, err)
	}
	return d
}

// localDiagnosis is the guaranteed-completion post-mortem: when the
// diagnoser model is unavailable (timeout, 400, empty reply) the result
// still carries checkable conditions mined from the run's own record —
// the failure line and every failed step — plus a final condition
// marking why the model post-mortem was skipped.
func localDiagnosis(fail string, tail []LocalCond, callErr error) eval.Diagnosis {
	var conds []string
	if f := strings.TrimSpace(fail); f != "" {
		conds = append(conds, "run failed: "+clipRunes(strings.ReplaceAll(f, "\n", " "), 240))
	}
	for _, s := range tail {
		msg := s.Err
		if msg == "" {
			msg = s.Name
		}
		conds = append(conds, fmt.Sprintf("%s failed: %s",
			s.Name, clipRunes(strings.ReplaceAll(msg, "\n", " "), 240)))
	}
	if len(conds) == 0 {
		conds = append(conds, "run failed with no error line recorded")
	}
	if callErr != nil {
		conds = append(conds, "diagnoser model unavailable: "+clipRunes(callErr.Error(), 200))
	} else {
		conds = append(conds, "diagnoser returned no conditions — local post-mortem used")
	}
	return eval.Diagnosis{FailedConditions: conds}
}

// TrajectoryFromExport builds the diagnoser's evidence from the session
// itself — the daemon has no steps panel, so the prompt, the error and
// the tail of the exported conversation stand in for it.
func TrajectoryFromExport(prompt, fail string, msgs []anthropic.BetaMessageParam) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Prompt:\n%s\n\nError:\n%s\n\nRecent turns:\n", prompt, fail)
	// Last 6 messages, each clipped: the diagnoser needs shape, not the
	// whole transcript (SummarizeFailure clips to TrajectoryCap anyway).
	tail := msgs
	if len(tail) > 6 {
		tail = tail[len(tail)-6:]
	}
	for _, m := range tail {
		role := string(m.Role)
		var parts []string
		for _, blk := range m.Content {
			switch {
			case blk.OfText != nil && strings.TrimSpace(blk.OfText.Text) != "":
				parts = append(parts, blk.OfText.Text)
			case blk.OfToolUse != nil:
				parts = append(parts, "[tool "+blk.OfToolUse.Name+"]")
			}
		}
		if len(parts) > 0 {
			fmt.Fprintf(&b, "%s: %s\n", role, clipRunes(strings.Join(parts, " "), 400))
		}
	}
	return b.String()
}

// clipRunes bounds a string to n runes with an ellipsis.
func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// RememberDiagnosis is the post-mortem memory writeback (断点 2): the
// lesson joins the project memory layer, deduped by content and pinned
// into the hot tier, so the NEXT session's MemoryFacts injection carries
// it. A real write audits one EvMemorySave line — deliberately not
// EvDiagnosis, which already records the diagnosis itself. Best-effort.
func RememberDiagnosis(workspace string, fire func(hooks.Record), d eval.Diagnosis) error {
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

// RememberDiagnosisSpace is the self-evolution writeback (断点 2b): the
// same post-mortem that joins project memory also lands in the CURRENT
// agent's specialist space — the agent that hit the failure owns the
// lesson about the failure. Best-effort, like RememberDiagnosis.
func RememberDiagnosisSpace(agentID string, fire func(hooks.Record), d eval.Diagnosis) {
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
	it, err := space.SaveKnowledge(agentID, name,
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

// ── the daemon pipeline ───────────────────────────────────────────

// Hook is the daemon-side prompt_end wiring (BACKLOG §2.5): the same
// evolution loop the TUI runs, without a TUI — the task-hour ledger
// books the run, the exam clock ticks, the blind judge grades, and a
// failed prompt is post-mortemed into memory, the agent's space and the
// next generation. Every model leg spawns its own goroutine (Go-style
// fan-out — the hub tap returns immediately) and honors the same
// LYON_SCORE / LYON_EXAM / LYON_DIAGNOSE gates the TUI honors.
func Hook(sess *agent.Session, ev agent.Event) {
	if ev.Type != agent.EventPromptEnd {
		return
	}
	prompt := sess.LastPrompt()
	if strings.TrimSpace(prompt) == "" {
		return
	}
	// Task hour market (#54): same ledger the TUI files — failed runs
	// stay on the ledger (with Err) but never price the estimate.
	if ev.DurationMS > 0 {
		market.Open().Record(market.Record{
			Class:  string(routing.Classify(prompt)),
			Model:  sess.RunModel(),
			Prompt: prompt,
			DurMS:  ev.DurationMS,
			Iters:  ev.Iterations,
			Err:    ev.Err,
			TS:     time.Now(),
		})
	}
	if ev.Err != "" {
		// 错因归纳: post-mortem the failed run — memory + space +
		// next generation (§1.3, §7.3).
		if strings.EqualFold(os.Getenv("LYON_DIAGNOSE"), "off") {
			return
		}
		go func() {
			traj := TrajectoryFromExport(prompt, ev.Err, sess.Export().Messages)
			d := DiagnoseWithFallback(sess, traj, ev.Err, nil)
			fire := sess.Hooks().Fire
			fire(hooks.Record{Event: hooks.EvDiagnosis, Output: d.String()})
			_ = RememberDiagnosis(sess.Workspace(), fire, d)
			RememberDiagnosisSpace(sess.Agent(), fire, d)
			_, _ = EvolveFromDiagnosis(sess, d)
		}()
		return
	}
	// Success legs: grade and exam clock run concurrently — the judge
	// can take minutes, and an exam due on this very round must not
	// wait behind it.
	if !strings.EqualFold(os.Getenv("LYON_SCORE"), "off") {
		model := sess.RunModel()
		go func() {
			// 240s, not 60s: gateway latency on thinking models was
			// blowing the old deadline and the run filed no card.
			ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
			defer cancel()
			_, _ = BookPlainRun(ctx, sess, model, prompt, len(sess.Stats()))
		}()
	}
	for _, due := range BookRound(prompt) {
		due := due
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			ex, err := LoadExam(sess.Workspace(), due.Class)
			if err != nil {
				return
			}
			res := eval.RunExam(ctx, ex, ExamAnswerer(sess, ex))
			agentID := sess.RunModel()
			_, _, _ = FinishExam(sess, agentID, due.Class, res)
		}()
	}
}
