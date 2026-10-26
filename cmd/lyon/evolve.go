package main

// Wiring for docs/design/agent-evolution.md — the eval/routing/flow
// machinery reachable from the TUI: /route classifies and advises,
// /exam runs the §1.4 closed-book test, /contest seats the whole
// candidate field against the same held-out bank (#53), /agents curates
// the entrants, /flow browses and freezes workflows, and every
// successful prompt books a production round that can trigger the exam
// clock. The CLI twins (-flow/-coverage/-exam) live in main.go; this
// file is the interactive half.
//
// The loop itself (grading, exam clock, contests, evolution spawns,
// diagnosis writebacks) lives in pkg/evolve — shared with lyond's
// prompt_end tap so daemon sessions run the same loop (BACKLOG §2.5).
// What stays here is the panel half: async steps, tea messages and the
// slash commands that kick them.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/eval"
	"lyon-agent/pkg/evolve"
	"lyon-agent/pkg/flow"
	"lyon-agent/pkg/market"
	"lyon-agent/pkg/models"
	"lyon-agent/pkg/routing"
)

// flowSaveMaxTokens bounds the gate's checklist generation and blind
// answers during a /flow save.
const flowSaveMaxTokens = 2048

// The computation units, aliased from pkg/evolve — same functions the
// daemon's Hook runs, zero behavior change for the TUI. contestRow is a
// type alias so the tea messages and the report renderer keep their
// shape.
type contestRow = evolve.ContestRow

var (
	loadExam               = evolve.LoadExam
	examAnswerer           = evolve.ExamAnswerer
	examAnswererOn         = evolve.ExamAnswererOn
	lastAssistantText      = evolve.LastAssistantText
	bookPlainRun           = evolve.BookPlainRun
	routingSwap            = evolve.RoutingSwap
	adoptAgent             = evolve.AdoptAgent
	evolveFromDiagnosis    = evolve.EvolveFromDiagnosis
	evolveFromExamFail     = evolve.EvolveFromExamFail
	runContest             = evolve.RunContest
	rememberDiagnosis      = evolve.RememberDiagnosis
	rememberDiagnosisSpace = evolve.RememberDiagnosisSpace
)

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
		if ex, e := loadExam(sess.Workspace(), class); e != nil {
			err = e
		} else {
			res = eval.RunExam(ctx, ex, examAnswerer(sess, ex))
		}
		if p := tuiProg.Load(); p != nil {
			p.Send(examDone{class: class, why: why, res: res, err: err, step: step})
		}
	}()
}

// ── /contest — 同类竞争上岗 (#53) ─────────────────────────────

// contestDone carries an async multi-candidate contest back into the loop.
type contestDone struct {
	class  string
	rows   []contestRow
	winner string // new on-duty id ("" = seat open after the election)
	err    error
	step   int
}

// contestNoFieldHint is the actionable answer for a contest with no one
// to elect between — the field is the built-in self incumbent alone.
// The command syntax mirrors agentsCommand's usage line verbatim, so the
// hint is always copy-paste runnable (EVOLUTION-GAPS task C).
func contestNoFieldHint() string {
	return tr(
		"no candidates beyond self — create one first: /agents add <id> <model> [effort]",
		"除 self 外没有候选 — 先创建一个：/agents add <id> <model> [effort]")
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
	// Contestants always carries the live self incumbent, so a field of
	// one means zero external candidates: an election of one decides
	// nothing. Answer with the create-a-candidate hint instead of
	// opening a step that would burn a gateway exam on a one-horse race.
	if len(cands) <= 1 {
		m.pushCmd(paint(cYellow, contestNoFieldHint()))
		m.refresh()
		return
	}
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
// that just finished and fires the exam when the clock strikes ten
// (evolve.BookRound owns the clocks; the TUI owns the kicks). Called
// from prompt_end's success legs only.
func (m *tuiModel) countRound() {
	if len(m.hist) == 0 {
		return
	}
	for _, due := range evolve.BookRound(m.hist[len(m.hist)-1]) {
		m.kickExam(due.Class, due.Why)
	}
}

// ── production scoring ────────────────────────────────────────────

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

// ── evolution panel facts (#14) ─────────────────────────────────────

// evoSummary is the computed fact sheet behind the panel's overall
// evolution block: totals from ONE locked Views snapshot plus the
// routing table's evidence count — statistics only, never a model call.
// A dimension the ledger does not record (version-over-version deltas —
// the board keeps no version history) stays absent from the struct, so
// the renderer can only show what actually exists.
type evoSummary struct {
	Classes int // task classes the panel tracks
	Tested  int // classes with at least one scorecard
	Cards   int // scorecards on file, all classes
	Held    int // classes with an on-duty seat
	Open    int // vacant seats among tested classes (demotion/staleness)
	Fails   int // classes on a consecutive exam-failure streak
	Due     int // classes whose exam clock strikes the interval
	Routed  int // classes whose routing evidence earns steering

	HasScores  bool    // at least one tested class
	Single     bool    // exactly one scored class — a "range" of one point
	Best       float64 // strongest class's newest scorecard total
	BestClass  string
	BestAgent  string
	Worst      float64 // weakest class's newest scorecard total
	WorstClass string
	WorstAgent string

	RecentClass  string // class owning the newest scorecard anywhere
	RecentExam   bool   // that card was an exam (else a production grade)
	RecentTotal  float64
	RecentPassed bool
	RecentAt     time.Time // zero = nothing filed yet
}

// summarizeEvolution folds the panel's Views snapshot and the routing
// entries into the summary facts. Strongest/weakest rank by each class's
// NEWEST scorecard total: the on-duty mean only exists for seated
// classes, and a demoted (failing) class is exactly the weakest seat a
// summary must not hide.
func summarizeEvolution(views []eval.View, routes []routing.Entry) evoSummary {
	s := evoSummary{Classes: len(views)}
	for _, v := range views {
		if v.OnDuty != "" {
			s.Held++
		}
		if v.ExamFails > 0 {
			s.Fails++
		}
		if v.Rounds > 0 && v.Rounds%eval.ExamInterval == 0 {
			s.Due++
		}
		if v.Cards == 0 {
			continue
		}
		s.Tested++
		s.Cards += v.Cards
		if v.OnDuty == "" {
			s.Open++
		}
		switch {
		case !s.HasScores:
			s.HasScores, s.Single = true, true
			s.Best, s.Worst = v.LastTotal, v.LastTotal
			s.BestClass, s.WorstClass = v.Class, v.Class
			s.BestAgent, s.WorstAgent = v.LastAgent, v.LastAgent
		default:
			s.Single = false
			if v.LastTotal > s.Best {
				s.Best, s.BestClass, s.BestAgent = v.LastTotal, v.Class, v.LastAgent
			}
			if v.LastTotal < s.Worst {
				s.Worst, s.WorstClass, s.WorstAgent = v.LastTotal, v.Class, v.LastAgent
			}
		}
		if s.RecentAt.IsZero() || v.LastAt.After(s.RecentAt) {
			s.RecentClass, s.RecentExam = v.Class, v.LastSource == "exam"
			s.RecentTotal, s.RecentPassed, s.RecentAt = v.LastTotal, v.LastPassed, v.LastAt
		}
	}
	for _, e := range routes {
		// The steering bar routingSwap enforces: a recommendation only
		// steers traffic once it carries 3+ samples and confidence ≥ 0.35.
		if e.Samples >= 3 && e.Confidence >= 0.35 {
			s.Routed++
		}
	}
	return s
}

// classBank is one task class's exam-set footprint on disk.
type classBank struct {
	Class  string
	Quests int  // questions in the file (0 = not seeded yet)
	Seeded bool // $LYON_HOME/exams/<class>.json exists
}

// examSetInfo is the fact sheet behind the panel's exam-set block: what
// the evaluation set IS — classes, on-disk banks, question counts,
// candidate field — read straight from pkg/eval's files, no model call.
type examSetInfo struct {
	Classes    int
	Banks      int // exam files present on disk
	Questions  int // questions across those banks
	PerClass   []classBank
	Candidates int // contest field (self included)
}

// describeExamSet reads the evaluation set's real footprint: one
// LoadExam per class (an error means the shipped bank has not landed on
// disk yet) plus the assembled candidate field. The classes come from
// the panel's Views snapshot so ledger extras are described too.
func describeExamSet(views []eval.View, selfModel string) examSetInfo {
	info := examSetInfo{Classes: len(views)}
	for _, v := range views {
		b := classBank{Class: v.Class}
		if ex, err := eval.LoadExam(v.Class); err == nil {
			b.Seeded, b.Quests = true, len(ex.Questions)
			info.Banks++
			info.Questions += b.Quests
		}
		info.PerClass = append(info.PerClass, b)
	}
	info.Candidates = len(eval.Contestants(selfModel))
	return info
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
		ex, err := loadExam(sess.Workspace(), class)
		if err != nil {
			msg.err = err
			send()
			return
		}
		msg.exam = eval.RunExam(ctx, ex, examAnswerer(sess, ex))
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
