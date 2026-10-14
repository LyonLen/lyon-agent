package main

// Wiring for docs/design/agent-evolution.md — the eval/routing/flow
// machinery reachable from the TUI: /route classifies and advises,
// /exam runs the §1.4 closed-book test, /contest seats the whole
// candidate field against the same held-out bank (#53), /agents curates
// the entrants, /flow browses and freezes workflows, and every
// successful prompt books a production round that can trigger the exam
// clock. The CLI twins (-flow/-coverage/-exam) live in main.go; this
// file is the interactive half.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/anthropics/anthropic-sdk-go"

	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/eval"
	"lyon-agent/pkg/flow"
	"lyon-agent/pkg/market"
	"lyon-agent/pkg/models"
	"lyon-agent/pkg/routing"
)

// examMaxTokens bounds one closed-book answer: a paragraph of facts,
// with headroom for the subagent-tier model's visible reasoning (mimo
// models think before answering — a too-tight cap truncates the reply
// before any answer text appears).
const examMaxTokens = 1500

// flowSaveMaxTokens bounds the gate's checklist generation and blind
// answers during a /flow save.
const flowSaveMaxTokens = 2048

// judgeMaxTokens bounds one blind grade: the grade itself, plus ample
// room for the subagent-tier model's visible reasoning (mimo models
// routinely think for hundreds of tokens before answering — a tight cap
// truncates the reply into a thinking-only response with no grade;
// 1024 was still eaten whole by thinking on longer review answers,
// so the budget went to 4096 — the grade line itself is tiny).
const judgeMaxTokens = 4096

// examDone carries an async closed-book exam back into the loop.
type examDone struct {
	class string
	why   string
	res   eval.ExamResult
	err   error
	step  int
}

// flowSaved carries an async /flow save back into the loop.
type flowSaved struct {
	slug     string
	nodes    int
	exam     eval.ExamResult
	gateNote string
	err      error
	step     int
}

// scoreDone carries an async production grade (design §1.2 blind judge)
// back into the loop.
type scoreDone struct {
	step int
	line string
	err  error
}

// ── model-call adapters ───────────────────────────────────────────

// asker adapts Session.Ask (subagent tier, stateless) to eval's shape.
func asker(sess *agent.Session) eval.Asker {
	return func(ctx context.Context, system, user string) (string, error) {
		return sess.Ask(ctx, system, user, flowSaveMaxTokens)
	}
}

// coverageGen / coverageBook are the §2 quiz legs: generate the
// checklist, answer it blind.
func coverageGen(sess *agent.Session) eval.ChecklistGen {
	return func(ctx context.Context, task string) (string, error) {
		return sess.Ask(ctx, eval.GenSystem, task, flowSaveMaxTokens)
	}
}

func coverageBook(sess *agent.Session) eval.Book {
	return func(ctx context.Context, qs []string) (string, error) {
		return sess.Ask(ctx, eval.BookSystem,
			"- "+strings.Join(qs, "\n- "), flowSaveMaxTokens)
	}
}

// examAnswerer answers one question with the exam system prompt —
// no tools, no files, weights only.
func examAnswerer(sess *agent.Session) eval.Answerer {
	return func(ctx context.Context, q string) (string, error) {
		return sess.Ask(ctx, eval.ClosedBookSystem, q, examMaxTokens)
	}
}

// ── /route ────────────────────────────────────────────────────────

// showRoute renders the router's state: bare lists the evidence table,
// the exam clocks and the current model's profile; with text it
// classifies the task and advises (routing table, capability profile,
// matched workflow).
func (m *tuiModel) showRoute(args string) {
	args = strings.TrimSpace(args)
	rt := routing.Open()
	lb := eval.OpenLeaderboard()

	if args == "" {
		lines := []string{paint(cBold, "routing — task class → best evidenced agent")}
		rows := rt.All()
		if len(rows) == 0 {
			lines = append(lines, paint(cDim,
				"  no scores yet — production runs and exams fill this table"))
		}
		for _, e := range rows {
			lines = append(lines, fmt.Sprintf("  %-8s %-22s conf %.2f  mean %.2f  n=%d",
				e.Class, e.Agent, e.Confidence, e.MeanScore, e.Samples))
		}
		// Views(nil) = every ledger class, sorted — a locked snapshot,
		// safe against Record filing cards from a scoring goroutine.
		if views := lb.Views(nil); len(views) > 0 {
			lines = append(lines, paint(cBold,
				fmt.Sprintf("exams — every %d rounds the on-duty agent sits a closed-book test",
					eval.ExamInterval)))
			for _, v := range views {
				seat := v.OnDuty
				if seat == "" {
					seat = "(open)"
				}
				line := fmt.Sprintf("  %-8s on-duty %-20s rounds %d/%d",
					v.Class, seat, v.Rounds, eval.ExamInterval)
				if v.ExamFails > 0 {
					line += fmt.Sprintf("  fails %d", v.ExamFails)
				}
				lines = append(lines, line)
			}
		}
		lines = append(lines, modelProfileLine(m.cfg.Model))
		m.pushCmd(lines...)
		return
	}

	class := routing.Classify(args)
	open := routing.Openness(args)
	lines := []string{
		paint(cBold, "route — "+clipTask(args)),
		fmt.Sprintf("  class: %s   openness %d/5", class, open),
	}
	if e, ok := rt.Lookup(string(class)); ok {
		lines = append(lines, fmt.Sprintf("  table: %s (conf %.2f, mean %.2f, n=%d)",
			e.Agent, e.Confidence, e.MeanScore, e.Samples))
	} else {
		lines = append(lines, paint(cDim, "  table: no evidence for this class yet"))
	}
	lines = append(lines, modelProfileLine(m.cfg.Model))
	if info := models.Lookup(m.cfg.Model); info != nil {
		prof := info.CapProfile()
		if open > int(prof.Openness) {
			lines = append(lines, paint(cYellow, fmt.Sprintf(
				"  ! task openness %d > model openness %d — a more open-ended model may serve better",
				open, prof.Openness)))
		}
	}
	if wfs, err := flow.Load(); err == nil {
		if w, ok := flow.Match(wfs, args); ok {
			lines = append(lines, fmt.Sprintf("  workflow: %s — %s", w.Slug, sanitizeText(w.Title)))
		}
	}
	m.pushCmd(lines...)
}

// modelProfileLine renders a model id with its 0–5 capability profile —
// what "this model is" in the router's language.
func modelProfileLine(model string) string {
	info := models.Lookup(model)
	if info == nil {
		return "  model: " + model + "  (no profile card)"
	}
	p := info.CapProfile()
	return fmt.Sprintf("  model: %s   reasoning=%d long-haul=%d openness=%d speed=%d thrift=%d",
		shortModel(model), p.Reasoning, p.LongHaul, p.Openness, p.Speed, p.Thrift)
}

// clipTask collapses a task to one display line.
func clipTask(s string) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) > 60 {
		return string(r[:60]) + "…"
	}
	return string(r)
}

// ── /exam + the §1.4 clock ────────────────────────────────────────

// examCommand is the manual /exam [class]: bare classifies from the
// last prompt. The run is async — the panel's stepEval row opens here
// and examDone closes it.
func (m *tuiModel) examCommand(args string) {
	class := strings.TrimSpace(args)
	why := "manual"
	if class == "" {
		if len(m.hist) == 0 {
			m.pushCmd(paint(cYellow, "usage: /exam [class] — class defaults from the last prompt"))
			m.refresh()
			return
		}
		class = string(routing.Classify(m.hist[len(m.hist)-1]))
		why = "manual — class from last prompt"
	}
	m.kickExam(class, why)
}

// kickExam runs the closed-book exam off the event stream: seed the
// bank, ask every question without tools, report as examDone.
func (m *tuiModel) kickExam(class, why string) {
	step := m.pushAsyncStep(stepEval, "exam."+class, why+" — closed-book…")
	sess := m.sess
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		var res eval.ExamResult
		var err error
		if ex, e := eval.LoadOrSeedExam(class); e != nil {
			err = e
		} else {
			res = eval.RunExam(ctx, ex, examAnswerer(sess))
		}
		if p := tuiProg.Load(); p != nil {
			p.Send(examDone{class: class, why: why, res: res, err: err, step: step})
		}
	}()
}

// ── /contest — 同类竞争上岗 (#53) ─────────────────────────────

// contestRow is one candidate's held-out result.
type contestRow struct {
	ID     string
	Model  string
	Pct    float64
	Passed bool
}

// contestDone carries an async multi-candidate contest back into the loop.
type contestDone struct {
	class  string
	rows   []contestRow
	winner string // new on-duty id ("" = seat open after the election)
	err    error
	step   int
}

// examAnswererOn answers closed-book on a specific candidate's model —
// the grading channel of the contest: each entrant must face the same
// held-out bank on ITS OWN model, or the result grades the wrong agent.
func examAnswererOn(sess *agent.Session, model string) eval.Answerer {
	return func(ctx context.Context, q string) (string, error) {
		return sess.AskModel(ctx, model, eval.ClosedBookSystem, q, examMaxTokens)
	}
}

// contestCommand is the manual /contest [class]: bare classifies from
// the last prompt. Async — the stepEval row opens here and contestDone
// closes it.
func (m *tuiModel) contestCommand(args string) {
	class := strings.TrimSpace(args)
	why := "manual"
	if class == "" {
		if len(m.hist) == 0 {
			m.pushCmd(paint(cYellow, "usage: /contest [class] — class defaults from the last prompt"))
			m.refresh()
			return
		}
		class = string(routing.Classify(m.hist[len(m.hist)-1]))
		why = "manual — class from last prompt"
	}
	m.kickContest(class, why)
}

// kickContest runs the whole field against the class's held-out exam
// OFF the event stream: every candidate answers concurrently (one
// goroutine per entrant — Go-style fan-out, the turn semaphore still
// bounds in-flight calls), all cards file, then one Elect() decides the
// seat on the combined evidence and the routing table starts steering
// traffic to the winner.
func (m *tuiModel) kickContest(class, why string) {
	self := m.cfg.Model
	cands := eval.Contestants(self)
	step := m.pushAsyncStep(stepEval, "contest."+class,
		fmt.Sprintf("%d candidates — held-out, closed-book…", len(cands)))
	sess := m.sess
	go func() {
		// K candidates × Q questions, concurrent but queue-prone behind
		// the gateway — 5 minutes bounds the step without starving real
		// exams of a chance to finish.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		rows, winner, err := runContest(ctx, sess, self, class)
		if p := tuiProg.Load(); p != nil {
			p.Send(contestDone{class: class, rows: rows, winner: winner, err: err, step: step})
		}
	}()
}

// runContest is the contest's whole computation — candidates against the
// class's held-out bank CONCURRENTLY, every card filed, one election on
// the combined evidence, routing steered to the winner. Split from the
// TUI wiring so tests can run it synchronously against a mock gateway.
func runContest(ctx context.Context, sess *agent.Session, selfModel, class string) ([]contestRow, string, error) {
	ex, err := eval.LoadOrSeedExam(class)
	if err != nil {
		return nil, "", err
	}
	cands := eval.Contestants(selfModel)
	rows := make([]contestRow, len(cands))
	var wg sync.WaitGroup
	for i, c := range cands {
		wg.Add(1)
		go func(i int, c eval.Candidate) {
			defer wg.Done()
			res := eval.RunExam(ctx, ex, examAnswererOn(sess, c.Model))
			rows[i] = contestRow{ID: c.ID, Model: c.Model, Pct: res.Pct, Passed: res.Passed}
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
			break
		}
	}
	return rows, winner, nil
}

// agentsCommand manages the candidate field: bare lists every entrant
// with its seats; add/rm curate file-backed profiles ("self" — the
// running model — is built in and untouchable).
func (m *tuiModel) agentsCommand(args string) {
	args = strings.TrimSpace(args)
	usage := paint(cYellow,
		"usage: /agents · /agents add <id> <model> [effort] · /agents rm <id>")
	switch {
	case args == "":
		cands := eval.Contestants(m.cfg.Model)
		lb := eval.OpenLeaderboard()
		lines := []string{paint(cBold, fmt.Sprintf("candidates (%d)", len(cands)))}
		for _, c := range cands {
			src := "file"
			if c.ID == "self" {
				src = "running"
			}
			line := fmt.Sprintf("  %-14s %-24s %s", c.ID, c.Model, paint(cDim, src))
			if v := lb.Seats(c.ID); len(v) > 0 {
				line += paint(cCyan, "  on-duty: "+strings.Join(v, ","))
			}
			if c.Effort != "" {
				line += paint(cDim, "  effort "+c.Effort)
			}
			lines = append(lines, line)
		}
		lines = append(lines, paint(cDim,
			"  /contest <class> runs the whole field against held-out tasks"))
		m.pushCmd(lines...)

	case strings.HasPrefix(args, "add "):
		fields := strings.Fields(strings.TrimPrefix(args, "add "))
		if len(fields) < 2 {
			m.pushCmd(usage)
			return
		}
		c := eval.Candidate{ID: fields[0], Model: fields[1]}
		if len(fields) >= 3 {
			c.Effort = fields[2]
		}
		if err := eval.SaveCandidate(c); err != nil {
			m.pushCmd(paint(cRed, "agents: "+sanitizeText(err.Error())))
			return
		}
		m.pushCmd(paint(cCyan, fmt.Sprintf("candidate %s → %s added — /contest to put it in the ring",
			c.ID, c.Model)))

	case strings.HasPrefix(args, "rm "):
		id := strings.TrimSpace(strings.TrimPrefix(args, "rm "))
		if err := eval.DeleteCandidate(id); err != nil {
			m.pushCmd(paint(cRed, "agents: "+sanitizeText(err.Error())))
			return
		}
		m.pushCmd(paint(cCyan, "candidate "+id+" removed"))

	default:
		m.pushCmd(usage)
	}
}

// marketCommand answers /market — the task hour ledger (#54): the
// per-class rollup first (what things cost here lately), then the
// newest finished tasks. Estimates ride the last 10 successful runs
// per class; /market itself is read-only.
func (m *tuiModel) marketCommand(args string) {
	if strings.TrimSpace(args) != "" {
		m.pushCmd(paint(cYellow, "usage: /market — the task hour ledger"))
		return
	}
	tab := market.Open()
	lines := []string{paint(cBold, "task hour market")}
	stats := tab.Stats()
	if len(stats) == 0 {
		lines = append(lines, paint(cDim,
			"  no finished tasks yet — every done prompt files its wall time here"))
		m.pushCmd(lines...)
		return
	}
	lines = append(lines, paint(cBold, "  class        n      avg      last"))
	for _, s := range stats {
		lines = append(lines, fmt.Sprintf("  %-12s %-6d %-8s %s",
			s.Class, s.Samples, fmtDur(s.AvgMS), fmtDur(s.LastMS)))
	}
	lines = append(lines, "", paint(cBold, "recent tasks"))
	recent := tab.Recent(8)
	for i := len(recent) - 1; i >= 0; i-- { // newest first
		r := recent[i]
		mark, col := "✓", cGreen
		if r.Err != "" {
			mark, col = "✗", cRed
		}
		line := fmt.Sprintf("  %s %-8s %-10s %s",
			paint(col, mark), fmtDur(r.DurMS), r.Class, r.Prompt)
		if r.Model != "" {
			line += paint(cDim, " · "+r.Model)
		}
		lines = append(lines, line)
	}
	lines = append(lines, paint(cDim,
		"  estimates price from each class's last 10 successful runs"))
	m.pushCmd(lines...)
}

// countRound books one production round for the class of the prompt
// that just finished and fires the exam when the clock strikes ten.
// LYON_EXAM=off opts out of the automatic trigger (the round still
// counts). Called from prompt_end's success leg only.
func (m *tuiModel) countRound() {
	if len(m.hist) == 0 {
		return
	}
	class := string(routing.Classify(m.hist[len(m.hist)-1]))
	lb := eval.OpenLeaderboard()
	rounds, due := lb.CountRound(class)
	if !due || strings.EqualFold(os.Getenv("LYON_EXAM"), "off") {
		return
	}
	m.kickExam(class, fmt.Sprintf("round %d/%d — exam due",
		rounds, eval.ExamInterval))
}

// ── production scoring ────────────────────────────────────────────

// lastAssistantText pulls the newest assistant prose for the judge —
// scanning back past tool-only turns. An all-tools run yields "" (the
// judge is told the run produced no text).
func lastAssistantText(msgs []anthropic.BetaMessageParam) string {
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

// bookPlainRun grades a completed free-form prompt (design §1.2: no
// ground truth → the judge model blind-grades), files the production
// scorecard to the leaderboard and the total to the routing table —
// the evidence both the on-duty election and /route read. The TUI's
// kickScore and the CLI's scorePlainCLI share it; a judge failure
// files nothing, because a score of convenience is worse than no
// score. Returns a display line.
func bookPlainRun(ctx context.Context, sess *agent.Session, model, prompt string, steps int) (string, error) {
	answer := lastAssistantText(sess.Export().Messages)
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

// routingSwap is design §5's consumption side: for an unpinned run,
// consult the routing table for the prompt's class and recommend the
// on-duty model once its seat has earned confidence (≥3 samples and
// confidence ≥0.35 — one lucky try never steers traffic). Returns the
// model to use and the entry that decided it.
func routingSwap(current, prompt string) (string, routing.Entry, bool) {
	class := string(routing.Classify(prompt))
	e, ok := routing.Open().Lookup(class)
	if !ok || e.Model == "" || e.Model == current || e.Samples < 3 || e.Confidence < 0.35 {
		return current, e, false
	}
	return e.Model, e, true
}

// kickScore grades the prompt that just finished: one cheap
// subagent-tier judge call off the event stream, then the card files
// itself inside bookPlainRun. The panel's stepEval row opens here and
// closes on scoreDone; LYON_SCORE=off skips the grade (the exam clock
// still ticks via countRound — the split mirrors LYON_EXAM).
func (m *tuiModel) kickScore(iterations int) {
	if strings.EqualFold(os.Getenv("LYON_SCORE"), "off") || len(m.hist) == 0 {
		return
	}
	prompt := m.hist[len(m.hist)-1]
	// Credit the model that actually ran the prompt: routing may have
	// swapped it at submit (#53), and a score filed under the wrong model
	// steers traffic the wrong way.
	model := m.runModel
	if model == "" {
		model = m.cfg.Model
	}
	step := m.pushAsyncStep(stepEval, "score", "blind judge grading this run…")
	sess := m.sess
	go func() {
		// The gateway's thinking models can queue for a while before
		// the grade lands — 60s was missing real runs (deadline
		// exceeded → no card); 240s keeps the async step bounded but
		// generous. JudgeRun retries on parse failure inside it.
		ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
		defer cancel()
		line, err := bookPlainRun(ctx, sess, model, prompt, iterations)
		if p := tuiProg.Load(); p != nil {
			p.Send(scoreDone{step: step, line: line, err: err})
		}
	}()
}

// ── /flow ─────────────────────────────────────────────────────────

// findFlow looks a slug up in the loaded set.
func findFlow(wfs []flow.Workflow, slug string) (flow.Workflow, bool) {
	for _, w := range wfs {
		if w.Slug == slug {
			return w, true
		}
	}
	return flow.Workflow{}, false
}

// flowCommand dispatches /flow's subcommands. save runs async (exam +
// coverage gate are model calls) and returns its kick; the rest answer
// straight from the local workflow files.
func (m *tuiModel) flowCommand(args string) tea.Cmd {
	sub, rest, _ := strings.Cut(strings.TrimSpace(args), " ")
	rest = strings.TrimSpace(rest)
	wfs, err := flow.Load()
	if err != nil {
		m.pushCmd(paint(cRed, "flow: "+sanitizeText(err.Error())))
		m.refresh()
		return nil
	}
	usage := paint(cYellow,
		"usage: /flow [list | show <slug> | match <text> | export <slug> | save <slug>]")

	switch sub {
	case "", "list":
		builtins, _ := flow.Builtins()
		bset := make(map[string]bool, len(builtins))
		for _, b := range builtins {
			bset[b.Slug] = true
		}
		lines := []string{paint(cBold, fmt.Sprintf("workflows (%d)", len(wfs)))}
		for _, w := range wfs {
			src := "user"
			if bset[w.Slug] {
				src = "builtin"
			}
			lines = append(lines, fmt.Sprintf("  %-14s %2d nodes  %s  %s",
				w.Slug, len(w.Nodes), sanitizeText(oneLine(w.Title)), paint(cDim, src)))
		}
		if len(wfs) == 0 {
			lines = append(lines, paint(cDim, "  none — /flow save <slug> freezes this session"))
		}
		lines = append(lines, paint(cDim,
			"  /flow show <slug> · match <text> · export <slug> · save <slug>"))
		m.pushCmd(lines...)

	case "show":
		w, ok := findFlow(wfs, rest)
		if !ok {
			m.pushCmd(paint(cYellow, "no workflow "+sanitizeText(rest)+" — /flow list"))
			break
		}
		lines := []string{paint(cBold, w.Slug+" — "+sanitizeText(oneLine(w.Title)))}
		if len(w.Trigger) > 0 {
			lines = append(lines, paint(cDim, "  trigger: "+strings.Join(w.Trigger, ", ")))
		}
		for i, n := range w.Nodes {
			kind := n.Exit.Kind
			if kind == "" {
				kind = flow.ExitAlways
			}
			spec := kind
			switch kind {
			case flow.ExitCommand:
				spec += " " + sanitizeText(n.Exit.Command)
			case flow.ExitContains:
				spec += " " + sanitizeText(n.Exit.Contains)
			case flow.ExitJudge:
				spec += " " + sanitizeText(clipTask(n.Exit.Judge))
			}
			fb := n.Fallback
			if fb == "" {
				fb = flow.FallbackRetry
			}
			line := fmt.Sprintf("  %d. %-14s exit:%s  fallback:%s",
				i+1, n.Name, spec, fb)
			if n.OnPass != "" {
				line += " → " + n.OnPass
			}
			lines = append(lines, line)
		}
		m.pushCmd(lines...)

	case "match":
		if rest == "" {
			m.pushCmd(usage)
			break
		}
		if w, ok := flow.Match(wfs, rest); ok {
			m.pushCmd(fmt.Sprintf("%s → %s — %s",
				paint(cBold, "match"), w.Slug, sanitizeText(oneLine(w.Title))))
		} else {
			m.pushCmd(paint(cYellow, "no workflow matches — /flow list to browse"))
		}

	case "export":
		w, ok := findFlow(wfs, rest)
		if !ok {
			m.pushCmd(paint(cYellow, "no workflow "+sanitizeText(rest)+" — /flow list"))
			break
		}
		data, err := json.MarshalIndent(w, "", "  ")
		if err != nil {
			m.pushCmd(paint(cRed, "export: "+err.Error()))
			break
		}
		m.pushCmd(append([]string{paint(cBold, w.Slug+".json")},
			strings.Split(string(data), "\n")...)...)

	case "save":
		if rest == "" {
			m.pushCmd(usage)
			break
		}
		return m.kickFlowSave(rest)

	default:
		m.pushCmd(usage)
	}
	m.refresh()
	return nil
}

// kickFlowSave freezes the session's prompt groups into a workflow —
// but not before the §1.4 and §2 preconditions: the agent must pass a
// closed-book exam on the task class, and the coverage gate's misses
// are baked into the node prompts so the frozen procedure carries the
// gap knowledge itself.
func (m *tuiModel) kickFlowSave(slug string) tea.Cmd {
	// Snapshot now: the goroutine must not race the panel's mutations.
	var prompts []string
	for _, s := range m.steps {
		if s.kind == stepPrompt {
			if p := strings.TrimSpace(s.detail); p != "" {
				prompts = append(prompts, p)
			}
		}
	}
	if len(prompts) == 0 {
		m.pushCmd(paint(cYellow, "flow save: no prompts in this session to freeze"))
		m.refresh()
		return nil
	}
	step := m.pushAsyncStep(stepEval, "flow.save."+slug, "exam + coverage gate…")
	sess := m.sess
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		msg := flowSaved{slug: slug, step: step}
		send := func() {
			if p := tuiProg.Load(); p != nil {
				p.Send(msg)
			}
		}

		// §1.4 precondition: pass a closed-book exam before freezing.
		class := string(routing.Classify(prompts[0]))
		ex, err := eval.LoadOrSeedExam(class)
		if err != nil {
			msg.err = err
			send()
			return
		}
		msg.exam = eval.RunExam(ctx, ex, examAnswerer(sess))
		if !msg.exam.Passed {
			msg.err = fmt.Errorf("exam %s failed (%.0f%%) — a procedure freezes only over a passing exam",
				class, msg.exam.Pct*100)
			send()
			return
		}

		// §2 precondition: coverage gate. A miss bakes the gap lines into
		// every node; a broken gate degrades to a note, never a block.
		ok, inject, gerr := flow.NewGate(coverageGen(sess), coverageBook(sess))(ctx, prompts[0])
		switch {
		case gerr != nil:
			msg.gateNote = "gate skipped: " + sanitizeText(gerr.Error())
		case ok:
			msg.gateNote = "coverage gate pass — nothing to inject"
		default:
			msg.gateNote = fmt.Sprintf("coverage miss — %d gap(s) baked into every node", len(inject))
		}

		wf := flow.Workflow{
			Slug:  slug,
			Title: clipTask(prompts[0]),
		}
		for i, p := range prompts {
			if gerr == nil && !ok && len(inject) > 0 {
				p += "\n\nKnowledge gaps from the coverage quiz — answer with these in mind:\n- " +
					strings.Join(inject, "\n- ")
			}
			wf.Nodes = append(wf.Nodes, flow.Node{
				Name:   fmt.Sprintf("step%d", i+1),
				Prompt: p,
				Exit: flow.Exit{
					Kind:  flow.ExitJudge,
					Judge: "这一步的输出是否完整达成了它的目标？",
				},
			})
		}
		if err := flow.Save(wf); err != nil {
			msg.err = err
		} else {
			msg.nodes = len(wf.Nodes)
		}
		send()
	}()
	return nil
}
