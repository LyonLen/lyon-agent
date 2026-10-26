package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	isatty "github.com/mattn/go-isatty"

	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/eval"
	"lyon-agent/pkg/evolve"
	"lyon-agent/pkg/hooks"
	"lyon-agent/pkg/market"
	"lyon-agent/pkg/memory"
	"lyon-agent/pkg/routing"
	"lyon-agent/pkg/tools"
)

// isTTY reports whether f is an interactive terminal — the full-screen TUI
// needs one; otherwise chat falls back to the plain line REPL.
func isTTY(f *os.File) bool {
	return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
}

// tuiProg is how a tool's approval hook reaches the running program. The
// hook runs on the engine's tool goroutine; the program is created after
// the engine, so the wiring happens through this atomic.
var tuiProg atomic.Pointer[tea.Program]

// apprMu serializes approval prompts: parallel-safe tools would otherwise
// race the single overlay slot. Same guarantee newApprovalHook gives the
// plain run path via its mutex.
var apprMu sync.Mutex

// tuiApprove is tools.ApproveFunc for TUI mode: it routes the question to
// the overlay and blocks until the user answers y/n (or the tool context
// dies, which denies — fail closed).
func tuiApprove(ctx context.Context, e tools.ExecEvent) bool {
	apprMu.Lock()
	defer apprMu.Unlock()
	if ctx.Err() != nil {
		return false
	}
	p := tuiProg.Load()
	if p == nil {
		return false
	}
	ch := make(chan bool, 1)
	p.Send(approveReq{ev: e, ch: ch}) // blocks until the UI takes it; drops if quit
	select {
	case v := <-ch:
		return v
	case <-ctx.Done():
		return false
	}
}

type approveReq struct {
	ev tools.ExecEvent
	ch chan bool
}

// tuiEv carries an agent event into the bubbletea message loop.
type tuiEv struct{ ev agent.Event }

// tlineKind selects how one transcript line is styled at render time.
type tlineKind int

const (
	lineStyled tlineKind = iota // pre-rendered ANSI (events, stats, banners)
	linePrompt                  // user prompt → green ❯ prefix
	lineText                    // assistant text → markdown-lite at view time
	lineThink                   // thinking → dim magenta
)

type tline struct {
	kind tlineKind
	// blk tags a thinking line with its block id (0 = ungrouped). A
	// contiguous run sharing an id renders as one collapsible block.
	blk int
	raw string
	// tool / exec tag styled rows with the API tool_use id and the
	// harness execution id, so parallel executions can insert themselves
	// under their own tool_use instead of piling at the tail (#45).
	tool string
	exec uint64
	// todo marks rows of the live plan block (#66): one contiguous
	// tagged run, rewritten in place by the next todo_write.
	todo bool
}

// tuiModel is the full-screen chat: scrollable transcript viewport on top,
// status line, single-line input at the bottom.
type tuiModel struct {
	sess      *agent.Session
	metrics   *tools.Metrics
	cfg       agent.Config // for /model: effective params (model, effort, …)
	header    string
	workspace string // where /resume looks for the saved snapshot

	vp     viewport.Model
	ti     textinput.Model
	spin   spinner.Model
	width  int
	height int

	// follow pins the viewport to the bottom as output streams; any scroll
	// away releases it, scrolling back to the bottom re-engages it.
	follow bool

	lines    []tline
	pend     string // partial streaming line, styled at flush
	pendKind tlineKind

	// Thinking folds into blocks: thinkSeq issues one id per contiguous run
	// of thinking deltas, curBlk is the run still streaming (0 = none, it is
	// closed by the next non-thinking line), thinkOpen is the global expand
	// toggle. A block still open always shows its body — the live stream is
	// the progress indicator — completed ones fold to a header line.
	thinkSeq  int
	curBlk    int
	thinkOpen bool
	// expandAll unfolds every collapsed region at once (ctrl+o, the
	// Claude Code-style verbose toggle). Thinking blocks consult it in
	// addition to their own fold; any future fold must too.
	expandAll bool

	// Slash popup: sugs holds the commands the current input matches,
	// sugIdx the highlighted row, sugHide set by esc until typing resumes.
	// sugCat is the browsed category tab for bare "/" (←/→ switch it).
	sugs    []sugItem
	sugIdx  int
	sugHide bool
	sugCat  int

	// statusSeen/statusAt timestamp the last status change so the idle
	// status line can age a toast back into the resident legend (#45).
	statusSeen string
	statusAt   time.Time

	// Session picker, opened by a bare /resume when the store holds several
	// sessions: pick lists them newest-first, pickIdx the highlighted slot.
	// It renders through the same bottom popup as the slash list and owns
	// the keyboard until enter (restore) or esc (cancel) closes it.
	pick    []agent.SnapshotRef
	pickIdx int

	busy      bool
	armedQuit bool
	// pasting guards the alt+v clipboard read: one at a time, so a held
	// key can't stack reads (and the status line can say it is working).
	pasting bool
	// pending is a slash action deferred until prompt_end lands ("/resume"
	// or "/new"): it interrupted an in-flight prompt, and the restore/reset
	// must never race a live drive holding histMu. pendingIdx carries the
	// 1-based slot a deferred /resume should restore; 0 = bare /resume
	// (the picker / single-session path runs at dispatch time).
	pending    string
	pendingIdx int
	// queued parks prompts typed while busy — enter never drops a
	// keystroke: each prompt_end sends the next one in FIFO order (one
	// send per end, a natural chain until the queue drains).
	queued []string
	// evoOn swaps the right column from the steps audit to the evolution
	// panel (leaderboard + exam clock) — alt+e / /evolve.
	evoOn bool
	// evoFeed carries evolution output (score/exam cards) to the right
	// panel while it is open; collapsed, the same cards fall through to
	// the output box below the input.
	evoFeed []string
	// curAgent is the live badge for the run in flight: the task class
	// the prompt was classified into (routing.Classify). Shown in the
	// status line so the working agent is visible before any score
	// lands — no more waiting for the ✎ score card.
	curAgent string
	// runModel is the model the current prompt actually runs on (#53):
	// routing may have swapped it at submit (the ⇄ line in the transcript
	// says so); "" = the session's config model. Scoring and the live
	// badge both read this so the credit lands on the model that did the
	// work, not the one configured.
	runModel string
	// todos is the live step plan (#66): the latest todo_write list,
	// drawn as one in-transcript block (pushTodos) and as a scale strip
	// in the busy status line. Snapshot rides via SetTodos/applyTodos.
	todos  []todoItem
	status string
	appr   *approveReq
	// apprSel is the highlighted option card on the approval popup
	// (0=y approve, 1=n deny, 2=a always).
	apprSel int
	// allowAll pre-authorizes a tool for the rest of the session ("a" on
	// the overlay) — approvals now default on, so a long run must not ask
	// the same question ten times. Deny decisions never persist.
	allowAll map[string]bool
	// cmdOut is the latest command's answer, rendered in the temporary
	// area below the transcript (pushCmd) — the chat region itself keeps
	// only questions, answers and call activity. Slash dispatch, prompt
	// submit and esc reset it so the newest answer stands alone.
	cmdOut []string

	hist    []string
	histIdx int // len(hist) = not recalling; up decrements, down increments

	// memRows mirrors the last /memory table's numbering so "/memory N"
	// toggles exactly the row the user is looking at.
	memRows []memory.Fact

	// titleBusy guards the async title refresh kicked at prompt_end:
	// one intent check in flight at a time. titlePending re-kicks a
	// prompt_end that landed while a check was still running, so the
	// latest prompt is always recognized.
	titleBusy    bool
	titlePending bool

	// Right-hand steps panel: the live action audit (steps.go). steps
	// accumulates the session's effective actions, stepSeq numbers them
	// session-wide, stepsOn is the /steps toggle (the panel also needs
	// enough width to appear — panelOn). tw/pw are the current column
	// widths: transcript and panel, gutter between.
	steps   []execStep
	stepSeq int
	stepsOn bool
	tw, pw  int
	// stepSel is the selected step's number (0 = live-follow the tail),
	// stepOpen expands it; panelW is the user-chosen panel width
	// (0 = default panelWidth). alt+↑/↓/enter/←/→ drive all three.
	stepSel  int
	stepOpen bool
	panelW   int
	// stepOutOpen expands the right column's live-output band (#13): the
	// column stacks steps / live output / plan, and alt+o grows the
	// middle band from its one-line summary to the output tail window.
	stepOutOpen bool
}

func newTUIModel(cfg agent.Config, sess *agent.Session, metrics *tools.Metrics) *tuiModel {
	ti := textinput.New()
	ti.Prompt = "❯ "
	ti.PromptStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	ti.Placeholder = "ask anything — /help for commands · alt+v paste image"
	ti.PlaceholderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	ti.CharLimit = 8192
	ti.Focus()

	sp := spinner.New(spinner.WithSpinner(spinner.Dot))
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("212"))

	m := &tuiModel{
		sess:      sess,
		metrics:   metrics,
		cfg:       cfg,
		workspace: cfg.Workspace,
		header: paint(cBold, "lyon chat") + paint(cDim,
			fmt.Sprintf("  model=%s effort=%s workspace=%s", cfg.Model, cfg.Effort, cfg.Workspace)),
		vp:       viewport.New(80, 20),
		ti:       ti,
		spin:     sp,
		width:    80,
		height:   24,
		follow:   true,
		status:   idleStatus,
		histIdx:  0,
		pendKind: lineText,
		stepsOn:  true, // panel appears once the terminal is wide enough
	}
	m.relayout() // tw/pw for the starting 80 columns (panel hidden)
	m.resetTranscript()
	m.updateSuggestions()
	m.refresh()
	return m
}

// headerLine is the pinned header plus the live session title once the
// async intent check has recognized one. Read per-frame, so a title
// landing between prompts shows on the next redraw. The context/cost
// gauge pins the bottom-right corner instead (see inputLine) — the
// corner the eye lands on while typing, out of the header's way.
func (m *tuiModel) headerLine() string {
	left := m.header + modeBadge(m.sess.Mode())
	if t := sanitizeText(m.sess.Title()); t != "" {
		left += paint(cDim, " · "+t)
	}
	return left
}

// lyonLogo is the big wordmark pinned at the top of the header ("software
// area") — ANSI Shadow block letters for LYON, rendered from the embedded
// full-alphabet FIGFont (figfont.go) so letterforms come from one typeface
// instead of hand-typed rows. Drawn only on roomy terminals (see
// headerRows) so small windows keep their transcript rows.
var lyonLogo = logoFont.render("LYON")

// headerRows builds the pinned header block: the wordmark with the four
// status cells riding its right side (roomy terminals only — #16,
// header_widgets.go), the identity line, and the divider that separates
// the software area from the session area below it. Row count is
// unchanged either way, so View's frame math never moves.
func (m *tuiModel) headerRows() []string {
	rows := []string{m.clip(m.headerLine())}
	if m.width >= 46 && m.height >= 26 {
		cells := widgetColumns(m, m.width-logoCells) // nil if too narrow to show
		for i, l := range lyonLogo {
			row := paint(cBold+cCyan, l)
			if i < len(cells) {
				row += cells[i]
			}
			rows = append(rows, m.clip(row))
		}
	}
	rows = append(rows, m.clip(ruleRow(m.width)))
	return rows
}

// ruleRow is a full-width dim divider between layout zones.
func ruleRow(w int) string {
	if w < 1 {
		w = 1
	}
	return paint(cDim, strings.Repeat("─", w))
}

// resetTranscript re-seeds the transcript with the banner — the opening
// view for a fresh model and again after /new or /resume wipe the old one.
// The header itself is not part of it: View draws it as pinned chrome
// above the viewport, so seeding it here would print it twice.
func (m *tuiModel) resetTranscript() {
	m.lines = []tline{
		{kind: lineStyled, raw: paint(cDim, "commands: /stats  /clear  /resume  /sessions  /new  /think  /help  /quit")},
		{kind: lineStyled, raw: ""},
	}
	// /new starts a fresh audit. /resume wipes here too, then reloads the
	// saved panel from Snapshot.Steps (applySteps) — durations and all.
	m.steps = nil
	m.stepSeq = 0
	m.stepSel = 0
	m.stepOpen = false
	m.stepOutOpen = false // the band collapses with the audit it reads
	m.cmdOut = nil
	m.todos = nil // the plan block dies with the lines; restore re-adds it
}

func (m *tuiModel) Init() tea.Cmd { return tea.Batch(m.ti.Focus(), dashTickCmd()) }

// newLine builds one transcript line, tagging thinking lines with the block
// that is currently streaming.
func (m *tuiModel) newLine(kind tlineKind, raw string) tline {
	blk := 0
	if kind == lineThink {
		blk = m.curBlk
	}
	return tline{kind: kind, blk: blk, raw: raw}
}

// appendDelta feeds streaming text/thinking into the partial line, cutting
// completed logical lines at each newline. Thinking opens a block (or joins
// the open one); any other kind closes it.
func (m *tuiModel) appendDelta(kind tlineKind, text string) {
	if m.pend != "" && m.pendKind != kind {
		m.flushPend()
	}
	if kind == lineThink {
		if m.curBlk == 0 {
			m.thinkSeq++
			m.curBlk = m.thinkSeq
		}
	} else {
		m.curBlk = 0
	}
	m.pendKind = kind
	m.pend += sanitizeText(text)
	for {
		i := strings.IndexByte(m.pend, '\n')
		if i < 0 {
			return
		}
		m.lines = append(m.lines, m.newLine(m.pendKind, m.pend[:i]))
		m.pend = m.pend[i+1:]
	}
}

// flushPend commits the partial line (if any) to the transcript.
func (m *tuiModel) flushPend() {
	if m.pend != "" {
		m.lines = append(m.lines, m.newLine(m.pendKind, m.pend))
		m.pend = ""
	}
}

func (m *tuiModel) pushStyled(lines ...string) {
	m.curBlk = 0 // anything pushed outside the stream ends the thinking run
	for _, l := range lines {
		m.lines = append(m.lines, tline{kind: lineStyled, raw: l})
	}
}

// insertLine splices one line at pos, shifting the tail right.
func (m *tuiModel) insertLine(pos int, l tline) {
	m.lines = append(m.lines, tline{})
	copy(m.lines[pos+1:], m.lines[pos:])
	m.lines[pos] = l
}

// pushTool appends a tool-intent row tagged with its API tool_use id so
// the executions of this call can nest beneath it (unknown id — replay,
// server tools — behaves like pushStyled).
func (m *tuiModel) pushTool(toolID, s string) {
	m.curBlk = 0
	m.lines = append(m.lines, tline{kind: lineStyled, raw: s, tool: toolID})
}

// pushExec inserts an execution row directly under the latest row of its
// tool_use (the intent row, or a previous execution of the same call),
// falling back to the tail when the id is unknown — subagent-inner
// executions have no main-flow intent row to nest under.
func (m *tuiModel) pushExec(toolID string, execID uint64, s string) {
	m.curBlk = 0
	pos := len(m.lines)
	if toolID != "" {
		for i := len(m.lines) - 1; i >= 0; i-- {
			if m.lines[i].tool == toolID {
				pos = i + 1
				break
			}
		}
	}
	m.insertLine(pos, tline{kind: lineStyled, raw: s, tool: toolID, exec: execID})
}

// pushExecDone inserts a completion row right beneath its own exec-start
// row, so each execution reads as one pair wherever parallel runs land.
func (m *tuiModel) pushExecDone(execID uint64, s string) {
	m.curBlk = 0
	pos := len(m.lines)
	if execID != 0 {
		for i := len(m.lines) - 1; i >= 0; i-- {
			if m.lines[i].exec == execID {
				pos = i + 1
				break
			}
		}
	}
	m.insertLine(pos, tline{kind: lineStyled, raw: s, exec: execID})
}

// pushCmd routes command output to the temporary area under the
// transcript instead of the conversation itself: tables, help text and
// action feedback are explanations of the UI, not things that happened
// in the chat. Lines accumulate within one command (loops append); the
// clear points (slash dispatch, prompt submit, esc) guarantee the area
// shows the newest answer alone.
func (m *tuiModel) pushCmd(lines ...string) {
	m.flushPend() // a half-streamed answer line still belongs to the chat
	m.curBlk = 0
	m.cmdOut = append(m.cmdOut, lines...)
}

// evoOutput routes evolution feedback (#47): with the panel open the card
// lands in the panel's own feed; collapsed it falls through to the output
// box below the input — evo output belongs to the panel when it shows.
// Entries may be multi-line; renderEvolve splits them into rows.
func (m *tuiModel) evoOutput(entries ...string) {
	if m.evoOn && m.pw > 0 {
		m.evoFeed = append(m.evoFeed, entries...)
		if len(m.evoFeed) > 24 {
			m.evoFeed = m.evoFeed[len(m.evoFeed)-24:]
		}
		return
	}
	m.pushCmd(entries...)
}

// pushText commits a finished (non-streaming) text block as transcript
// lines of the given kind — used by replay and for whole-message content.
func (m *tuiModel) pushText(kind tlineKind, text string) {
	m.flushPend()
	text = sanitizeText(text)
	for _, ln := range strings.Split(text, "\n") {
		m.lines = append(m.lines, m.newLine(kind, ln))
	}
	if kind != lineThink {
		m.curBlk = 0
	}
}

// replay seeds the transcript with a restored conversation so `lyon chat -c`
// opens with visible history instead of a blank screen. Content maps to the
// same line kinds the live stream uses; oversized special blocks (workspace
// memory, compaction summaries) collapse to one notice line each.
func (m *tuiModel) replay(snap *agent.Snapshot) {
	if snap == nil || len(snap.Messages) == 0 {
		return
	}
	m.pushStyled(
		paint(cDim, fmt.Sprintf("restored %d messages · %s", len(snap.Messages),
			snap.SavedAt.Format("2006-01-02 15:04"))),
		"",
	)
	// No [turn N] accounting rows on replay either: the transcript shows
	// the conversation, not its token ledger — usage lives in /stats and
	// the corner gauge (snap.Turns is still restored for those views).
	for _, msg := range snap.Messages {
		for _, b := range msg.Content {
			switch {
			case b.OfText != nil:
				text := sanitizeText(b.OfText.Text)
				switch {
				case strings.HasPrefix(text, "[persistent-memory]"):
					m.pushStyled(paint(cDim, "· memory restored from a previous session"))
				case strings.HasPrefix(text, "[conversation compacted"):
					head := strings.SplitN(text, "\n", 2)[0]
					head = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(head, "[conversation compacted"), "]"))
					m.pushStyled(fmt.Sprintf("%s %s",
						paint(cYellow, "📦 compaction"), paint(cDim, head)))
				case msg.Role == anthropic.BetaMessageParamRoleUser:
					m.pushText(linePrompt, text)
				default:
					m.pushText(lineText, text)
				}

			case b.OfThinking != nil:
				// Restored thinking needs a block id so consecutive
				// thinking runs group apart (pushText tags lines with
				// curBlk, which is 0 throughout replay — two runs would
				// merge into one fold). The fold itself is blk-agnostic.
				m.thinkSeq++
				m.curBlk = m.thinkSeq
				m.pushText(lineThink, b.OfThinking.Thinking)
				m.curBlk = 0

			case b.OfToolUse != nil:
				in, _ := json.Marshal(b.OfToolUse.Input)
				m.pushStyled(indTool + fmt.Sprintf("%s %s", paint(cCyan, b.OfToolUse.Name),
					paint(cDim, truncateRunes(stepArgPreview(b.OfToolUse.Name, in), 160))))

			case b.OfToolResult != nil:
				marker := paint(cMagenta, "⤷ result")
				if b.OfToolResult.IsError.Value {
					marker = paint(cRed, "⤷ error")
				}
				var txt []string
				for _, c := range b.OfToolResult.Content {
					if c.OfText != nil && strings.TrimSpace(c.OfText.Text) != "" {
						txt = append(txt, sanitizeText(c.OfText.Text))
					}
				}
				m.pushStyled(fmt.Sprintf("%s %s", marker,
					paint(cDim, clipReplay(strings.Join(txt, " "), 200))))
			case b.OfImage != nil:
				m.pushStyled(paint(cDim, "· image"))
			}
		}
	}
	m.pushStyled("")
	m.refresh()
}

// clipReplay shortens one replayed line for the transcript.
func clipReplay(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// Transcript indentation for the event hierarchy: tool calls sit one
// level under the turn's prose, executions under their tool_use, and
// subagent-inner executions one level deeper still.
const (
	indTool = "  "     // tool_use / server / subagent-summary rows
	indExec = "    "   // exec / done / result rows
	indSub  = "      " // exec / done inside a run_subagent
)

// applyEvent translates one engine event into transcript lines. Mirrors
// render()'s plain-mode styling, minus the immediate stdout writes.
func (m *tuiModel) applyEvent(ev agent.Event) {
	m.stepEvent(ev) // fold into the right-hand audit panel first
	switch ev.Type {
	case agent.EventThinkingDelta:
		m.appendDelta(lineThink, ev.Thinking)

	case agent.EventTextDelta:
		m.appendDelta(lineText, ev.Text)

	case agent.EventToolUseStart:
		m.flushPend()
		if ev.Server {
			m.pushStyled(indTool + fmt.Sprintf("%s %s %s", paint(cMagenta, "🌐 server"), ev.ToolName,
				paint(cDim, truncateRunes(stepArgPreview(ev.ToolName, ev.Input), 160))))
		} else {
			// A compact intent row: the API call id is noise to a reader,
			// and the live tool_use event carries no input yet — the exec
			// row right below shows the actual arguments. Tagged with the
			// block id so parallel executions nest under this row (#45).
			m.pushTool(ev.ToolID, indTool+paint(cCyan, ev.ToolName))
		}

	case agent.EventServerToolResult:
		m.flushPend()
		marker := paint(cMagenta, "⤷ results")
		if ev.IsError {
			marker = paint(cRed, "⤷ error")
		}
		m.pushStyled(indExec + fmt.Sprintf("%s %s %s", marker, ev.ToolName, paint(cDim, shortJSON(ev.Input, 240))))

	case agent.EventToolExecStart:
		m.flushPend()
		// todo_write is the plan itself (#66): the block it rewrites IS
		// the readable row — no exec noise above it. Subagent plans stay
		// out of the main flow (their fan-out is visible on their own).
		if ev.ToolName == "todo_write" && !ev.Sub {
			if items := parseTodos(ev.Input); items != nil {
				m.setTodos(items)
			}
			break
		}
		// Executions nest under their own tool_use row (#45 — parallel
		// tool calls each stay with their parent via the ToolID FIFO);
		// subagent-inner ones go one level deeper at the tail.
		ind := indExec
		if ev.Sub {
			ind = indSub
		}
		m.pushExec(ev.ToolID, ev.ExecID,
			ind+fmt.Sprintf("%s #%d %s %s", paint(cCyan, "exec"), ev.ExecID, ev.ToolName,
				paint(cDim, truncateRunes(stepArgPreview(ev.ToolName, ev.Input), 160))))

	case agent.EventToolExecEnd:
		m.flushPend()
		if ev.ToolName == "todo_write" && !ev.Sub {
			// A rejected plan (validation error) keeps the old block —
			// the failure row is the only new line.
			if ev.IsError {
				m.pushStyled(indExec + fmt.Sprintf("%s %s",
					paint(cRed, "todo_write "+tr("failed", "失败")),
					paint(cRed, sanitizeText(ev.Err))))
			}
			break
		}
		ind := indExec
		if ev.Sub {
			ind = indSub
		}
		if ev.IsError {
			m.pushExecDone(ev.ExecID, ind+fmt.Sprintf("%s #%d %s %s", paint(cRed, "failed"), ev.ExecID,
				paint(cDim, fmt.Sprintf("%dms", ev.DurationMS)), paint(cRed, sanitizeText(ev.Err))))
		} else {
			m.pushExecDone(ev.ExecID, ind+fmt.Sprintf("%s #%d %s", paint(cGreen, "done"), ev.ExecID,
				paint(cDim, fmt.Sprintf("%dms", ev.DurationMS))))
		}

	case agent.EventTurnEnd:
		// Intentionally blank: the per-turn [turn N] accounting row is no
		// longer drawn — usage lives in /stats and the corner gauge. No
		// flushPend either: the next turn continues the same partial line.

	case agent.EventCompaction:
		m.flushPend()
		line := sanitizeText(ev.Detail)
		if ev.Usage != nil {
			line = fmt.Sprintf("%s (in=%d out=%d)", ev.Detail, ev.Usage.Input, ev.Usage.Output)
		}
		m.pushStyled("", fmt.Sprintf("%s %s", paint(cYellow, "📦 compaction"), paint(cDim, line)))

	case agent.EventSubagentEnd:
		m.flushPend()
		if ev.IsError {
			// A dispatched subtask failed while its siblings finished —
			// the transcript names the loss, the panel node went red.
			m.pushStyled(indTool + fmt.Sprintf("%s %s %s",
				paint(cCyan, "🧠 subagent"), ev.ToolName,
				paint(cRed, sanitizeText(ev.Err))))
			break
		}
		u := ev.Usage
		if u == nil {
			u = &agent.Usage{}
		}
		m.pushStyled(indTool + fmt.Sprintf("%s %s in=%d out=%d cache_read=%d %s %s",
			paint(cCyan, "🧠 subagent"), ev.ToolName,
			u.Input, u.Output, u.CacheRead, sanitizeText(ev.Detail),
			paint(cDim, fmt.Sprintf("%dms", ev.DurationMS))))

	case agent.EventQueueInjected:
		// The session delivered a parked line at a turn gap (#2): drop the
		// display mirror and record it like the prompt it now is — a real
		// prompt row mid-run, so the transcript shows where the new
		// instruction entered the conversation.
		m.flushPend()
		m.queueInjected(ev.Text)

	case agent.EventError:
		m.flushPend()
		m.pushStyled("", paint(cRed, tr("error: ", "错误: ")+sanitizeText(ev.Err)))

	case agent.EventPromptEnd:
		m.flushPend()
		m.busy = false
		m.armedQuit = false
		// §3.1 streak tick + freeze hint while this run's steps are still
		// on the panel (flowhint.go) — before deferred actions can wipe them.
		m.flowHint(ev)
		// Topic may have shifted: kick the cheap intent check so the
		// header title follows the conversation.
		m.kickTitleCheck()
		// Surface completion as a system toast — long runs are watched by
		// notification, not by staring at the transcript.
		budget := ev.StopReason == "max_iterations" // loop cut by MaxIterations
		cutOff := ev.StopReason == "max_tokens"     // reply truncated at the output cap
		switch {
		case ev.Err == agent.ErrInterrupted.Error():
			notifyDone("lyon: interrupted", "the run was stopped — history repaired")
		case ev.Err != "":
			notifyDone("lyon: run failed", truncNotif(ev.Err, 160))
		case budget:
			notifyDone("lyon: iteration budget reached",
				fmt.Sprintf("%d API calls — press enter to continue the task", ev.Iterations))
		case cutOff:
			notifyDone("lyon: reply cut off", "output token cap reached — raise -max-tokens / LYON_MAX_TOKENS")
		default:
			notifyDone("lyon: run finished",
				fmt.Sprintf("%d API calls complete — press enter for the next prompt", ev.Iterations))
		}
		if ev.Err == agent.ErrInterrupted.Error() {
			m.pushStyled(paint(cYellow, "■ "+tr("interrupted", "已中断")),
				paint(cDim, tr("history repaired — keep chatting", "历史已修复 — 继续聊就好")))
			m.status = idleStatus
		} else if ev.Err != "" {
			m.pushStyled(paint(cRed, "✗ "+tr("prompt failed", "提问失败")),
				paint(cDim, sanitizeText(ev.Err)))
			m.status = idleStatus
			// 错因归纳: post-mortem the failed run while the UI idles.
			m.kickDiagnose(ev.Err)
		} else if budget {
			// Total wall time of the whole prompt, next to the call
			// count — "how long was I waiting" answered at a glance (#43).
			dur := ""
			if ev.DurationMS > 0 {
				dur = " · " + fmtDur(ev.DurationMS)
			}
			m.pushStyled(paint(cYellow, "⚡ "+tr("budget reached", "预算耗尽")),
				paint(cDim, tr(fmt.Sprintf("%d API calls%s — pending tools skipped, enter to continue · raise with -max-iter / LYON_MAX_ITER", ev.Iterations, dur),
					fmt.Sprintf("%d 次 API 调用%s — 未执行的工具已跳过，enter 继续 · -max-iter / LYON_MAX_ITER 可调高", ev.Iterations, dur))))
			m.status = idleStatus
			m.countRound()
			// The grade sees the partial answer as it stands — honest,
			// not withheld: a cut run that answered nothing scores low.
			m.kickScore(ev.Iterations)
		} else if cutOff {
			dur := ""
			if ev.DurationMS > 0 {
				dur = " · " + fmtDur(ev.DurationMS)
			}
			m.pushStyled(paint(cYellow, "✂ "+tr("reply cut off", "回复被截断")),
				paint(cDim, tr(fmt.Sprintf("%d API calls%s — the output token cap ended the answer mid-thought · raise with -max-tokens / LYON_MAX_TOKENS", ev.Iterations, dur),
					fmt.Sprintf("%d 次 API 调用%s — 输出上限把回答截断在半句 · -max-tokens / LYON_MAX_TOKENS 可调高", ev.Iterations, dur))))
			m.status = idleStatus
			m.countRound()
			m.kickScore(ev.Iterations)
		} else {
			dur := ""
			if ev.DurationMS > 0 {
				dur = " · " + fmtDur(ev.DurationMS)
			}
			m.pushStyled(paint(cGreen, "✓ "+tr("done", "完成")),
				paint(cDim, tr(fmt.Sprintf("%d API calls%s", ev.Iterations, dur),
					fmt.Sprintf("%d 次 API 调用%s", ev.Iterations, dur))))
			m.status = idleStatus
			// §1.4 exam clock: book one production round; every tenth
			// round the on-duty agent sits a closed-book exam
			// (LYON_EXAM=off opts out of the trigger).
			m.countRound()
			// §1.2 production score: blind-judge this run and file the
			// card (LYON_SCORE=off opts out) — scoring is automatic.
			m.kickScore(ev.Iterations)
		}
		// Task hour market (#54): file this run's real wall time. hist's
		// last entry IS this run — queued inputs don't join hist until
		// kicked after this event. Failed runs stay on the ledger (with
		// Err) but never price the estimate.
		if ev.DurationMS > 0 && len(m.hist) > 0 {
			p := m.hist[len(m.hist)-1]
			market.Open().Record(market.Record{
				Class:  string(routing.Classify(p)),
				Model:  m.runModel,
				Prompt: p,
				DurMS:  ev.DurationMS,
				Iters:  ev.Iterations,
				Err:    ev.Err,
				TS:     time.Now(),
			})
		}
		if m.pending != "" { // deferred /resume, /new or /rollback: prompt ended
			act, idx := m.pending, m.pendingIdx
			m.pending, m.pendingIdx = "", 0
			switch {
			case act == "/new":
				m.doNew()
			case act == "/rollback":
				m.rollbackAction(idx)
			case idx > 0:
				m.doResume(idx)
			default:
				m.resumeAction() // bare /resume: pick now that we're idle
			}
		}
	}
}

// doResume restores slot idx (1-based, newest first; 0/1 = the latest) into
// the running session and rebuilds the transcript from it. Only called with
// no prompt in flight (idle, or immediately after prompt_end), so histMu is
// free and the restore cannot wait on a live drive.
func (m *tuiModel) doResume(idx int) {
	refs, err := agent.ListSnapshots(m.workspace)
	if err != nil {
		m.pushStyled(paint(cRed, tr("resume: ", "恢复失败: ")+err.Error()))
		m.refresh()
		return
	}
	m.restoreFrom(refs, idx)
}

// restoreFrom is doResume's load half over an already-listed store — the
// picker restores the exact list it displayed, immune to the store changing
// between opening and choosing.
func (m *tuiModel) restoreFrom(refs []agent.SnapshotRef, idx int) {
	switch {
	case len(refs) == 0:
		m.pushStyled(paint(cYellow, tr("no saved session for this workspace",
			"本工作区没有已保存的会话")))
	case idx < 1 || idx > len(refs):
		m.pushStyled(paint(cYellow, tr(
			fmt.Sprintf("no session #%d — /sessions lists %d", idx, len(refs)),
			fmt.Sprintf("没有 #%d 号会话 — /sessions 列出 %d 个", idx, len(refs)))))
	default:
		loaded, err := agent.LoadSnapshotFrom(refs[idx-1].Path)
		switch {
		case err != nil:
			m.pushStyled(paint(cRed, tr("resume: ", "恢复失败: ")+err.Error()))
		case loaded == nil:
			m.pushStyled(paint(cYellow, tr("that session is empty", "那个会话是空的")))
		default:
			m.sess.Restore(*loaded)
			m.pend = ""
			m.curBlk = 0
			m.follow = true
			m.resetTranscript()
			m.replay(loaded)
			m.applySteps(loaded.Steps) // audit panel rides into the snapshot
			m.applyTodos(loaded.Todos) // step plan rides too (#66)
			m.status = idleStatus
		}
	}
	m.refresh()
}

// resumeAction is a bare /resume: one saved session restores directly, none
// reports, several open the interactive picker — the user picks the slot
// instead of being handed the newest. Only called idle.
func (m *tuiModel) resumeAction() {
	refs, err := agent.ListSnapshots(m.workspace)
	switch {
	case err != nil:
		m.pushStyled(paint(cRed, tr("resume: ", "恢复失败: ")+err.Error()))
		m.refresh()
	case len(refs) == 0:
		m.pushStyled(paint(cYellow, tr("no saved session for this workspace",
			"本工作区没有已保存的会话")))
		m.refresh()
	case len(refs) == 1:
		m.doResume(1)
	default:
		m.pick = refs
		m.pickIdx = 0
		m.status = idleStatus
		m.updateSuggestions() // viewport yields rows to the picker
		m.refresh()
	}
}

// closePick dismisses the session picker and gives the space back.
func (m *tuiModel) closePick() {
	m.pick = nil
	m.pickIdx = 0
	m.updateSuggestions()
	m.refresh()
}

// updatePick is the picker's modal keyboard: ↑/↓ walk the store (wrapping),
// enter restores the highlighted slot, esc cancels. Any other key is
// swallowed — the list owns the bottom until it closes.
func (m *tuiModel) updatePick(msg tea.KeyMsg) {
	switch msg.String() {
	case "up":
		if n := len(m.pick); n > 0 {
			m.pickIdx = (m.pickIdx - 1 + n) % n
		}
	case "down":
		if n := len(m.pick); n > 0 {
			m.pickIdx = (m.pickIdx + 1) % n
		}
	case "enter":
		refs, idx := m.pick, m.pickIdx+1
		m.closePick()
		m.restoreFrom(refs, idx)
	case "esc", "ctrl+c":
		m.closePick() // cancel, don't quit — /quit or esc-then-ctrl+c exits
	case "pgup", "pgdown":
		m.pageScroll(msg.String() == "pgup")
	}
}

// showSessions lists the workspace's session store so /resume N has a
// target. Read-only — runs even while a prompt is in flight.
func (m *tuiModel) showSessions() {
	refs, err := agent.ListSnapshots(m.workspace)
	switch {
	case err != nil:
		m.pushCmd(paint(cRed, tr("sessions: ", "会话列表失败: ")+err.Error()))
	case len(refs) == 0:
		m.pushCmd(paint(cYellow, tr("no saved sessions for this workspace",
			"本工作区没有已保存的会话")))
	default:
		m.pushCmd(paint(cBold, tr(fmt.Sprintf("saved sessions (%d, newest first)", len(refs)),
			fmt.Sprintf("已保存会话（%d 个，最新在前）", len(refs)))))
		for i, r := range refs {
			m.pushCmd(tr(fmt.Sprintf("  %2d  %s  %d messages %s", i+1,
				r.SavedAt.Format("2006-01-02 15:04"), r.Messages,
				paint(cDim, fmt.Sprintf("— /resume %d", i+1))),
				fmt.Sprintf("  %2d  %s  %d 条消息 %s", i+1,
					r.SavedAt.Format("2006-01-02 15:04"), r.Messages,
					paint(cDim, fmt.Sprintf("— /resume %d", i+1)))))
		}
		if len(refs) > 1 {
			m.pushCmd(paint(cDim, tr("/resume opens the picker — ↑/↓ choose",
				"/resume 打开选择器 — ↑/↓ 选择")))
		} else {
			m.pushCmd(paint(cDim, tr("/resume alone restores the newest",
				"裸 /resume 恢复最新一条")))
		}
	}
	m.refresh()
}

// doNew wipes the conversation — engine state and transcript — back to a
// fresh session. The live conversation is archived first (unless it
// duplicates the store), so nothing resumable dies with /new; later saves
// rotate rather than overwrite, which is why browsing old sessions needs
// /sessions + /resume N instead of a single slot.
// rollbackAction lists the session's undo checkpoints (idx == 0) or
// restores slot idx (1-based, newest first — same numbering as /sessions).
// Caller guarantees no prompt is in flight (idle, or right after prompt_end).
func (m *tuiModel) rollbackAction(idx int) {
	cps := m.sess.Checkpoints()
	switch {
	case len(cps) == 0:
		m.pushCmd(paint(cYellow, tr("no checkpoint yet — one is captured before each prompt that had history",
			"还没有检查点 — 每次带历史的提问前都会自动打一个")))
	case idx == 0:
		m.pushCmd(paint(cBold, tr(fmt.Sprintf("checkpoints (%d, newest first)", len(cps)),
			fmt.Sprintf("检查点（%d 个，最新在前）", len(cps)))))
		for i, cp := range cps {
			mark := "•"
			if cp.Kind == "compact" {
				mark = "📦"
			}
			m.pushCmd(fmt.Sprintf("  %2d  %s %s  %s  %s", i+1, mark,
				paint(cYellow, cp.Label), paint(cDim, fmt.Sprintf("%d msgs · %s",
					cp.Messages, cp.At.Format("15:04:05"))),
				paint(cDim, fmt.Sprintf("— /rollback %d", i+1))))
		}
		m.pushCmd(paint(cDim, tr("rolling back keeps turn stats & usage — only the conversation rewinds",
			"回退保留轮次统计与用量 — 只倒回对话本身")))
	case idx > len(cps):
		m.pushCmd(paint(cRed, tr(
			fmt.Sprintf("no slot %d — /rollback lists %d checkpoints", idx, len(cps)),
			fmt.Sprintf("没有 %d 号槽位 — /rollback 列出 %d 个检查点", idx, len(cps)))))
	default:
		target := cps[idx-1]
		info, err := m.sess.Rollback(target.ID)
		m.flushPend() // the audit belongs in the chat, after any pending text
		if err != nil {
			m.pushStyled(paint(cRed, tr("rollback: ", "回退失败: ")+err.Error()))
			break
		}
		m.pushStyled("")
		m.pushStyled(fmt.Sprintf("%s %s",
			paint(cYellow, "↺ "+tr("rolled back", "已回退")),
			paint(cDim, tr(fmt.Sprintf("#%d %s — %d messages restored · turns/usage kept · audit line written",
				info.ID, info.Label, info.Messages),
				fmt.Sprintf("#%d %s — 恢复 %d 条消息 · 轮次/用量保留 · 审计行已写入",
					info.ID, info.Label, info.Messages)))))
		m.pushStyled(paint(cDim, tr("transcript above the marker reflects discarded turns; next prompt continues from the checkpoint",
			"标记以上的转录反映被丢弃的轮次；下次提问从检查点继续")))
		m.pushStyled("")
		m.status = tr("rolled back to #", "已回退到 #") + fmt.Sprint(info.ID)
	}
	m.refresh()
}

func (m *tuiModel) doNew() {
	archived, err := m.sess.ArchiveIfNew()
	m.sess.Reset()
	m.pend = ""
	m.curBlk = 0
	m.follow = true
	m.resetTranscript()
	notice := tr("new session — history cleared", "新会话 — 历史已清空")
	switch {
	case err != nil:
		m.pushStyled(paint(cRed, tr("archive: ", "归档失败: ")+err.Error()))
	case archived:
		notice += tr(", previous conversation archived (/sessions to browse)",
			"，上一段对话已归档（/sessions 浏览）")
	}
	m.pushStyled(paint(cDim, notice), "")
	m.status = idleStatus
	m.refresh()
}

// idleStatus doubles as the resident key legend: it renders whenever the
// The resident key legend — baked through tr() at init and rebuilt on
// /lang (a var, because the language is runtime state). When the status
// line has nothing urgent to say it falls back to this, so the shortcuts
// are discoverable without knowing /help exists (narrow terminals clip
// the tail).
const (
	idleLegendEN = "enter send · ctrl+c interrupt · esc close output · ctrl+o all folds · pgup/pgdn scroll · shift+alt+↑↓ select · alt+enter open · alt+o live output · alt+←→ width · alt+e evolve · /help"
	idleLegendZH = "enter 发送 · ctrl+c 中断 · esc 关输出 · ctrl+o 全展开 · pgup/pgdn 滚动 · shift+alt+↑↓ 选择 · alt+enter 展开 · alt+o 实时输出 · alt+←→ 调宽 · alt+e 进化 · /help"
)

var idleStatus = tr(idleLegendEN, idleLegendZH)

// sugItem is one row of the slash-command popup, tagged with its
// category so bare "/" can browse one tab at a time (#42).
type sugItem struct{ cmd, desc, cat string }

// slashCommands is the popup's source of truth — keep it in sync with
// slash(): every entry must be a command that slash() actually handles.
// Built through tr() so /lang can rebuild it (refreshBakedTexts).
var slashCommands = buildSlashCommands()

// sugCats is the popup's category tabs in display order; sugCat indexes
// into it. A typed prefix still searches across every category.
var sugCats = []string{"session", "model", "panel", "evolve", "misc"}

func sugCatLabel(cat string) string {
	switch cat {
	case "session":
		return tr("session", "会话")
	case "model":
		return tr("model", "模型")
	case "panel":
		return tr("panel", "面板")
	case "evolve":
		return tr("evolve", "进化")
	default:
		return tr("misc", "其他")
	}
}

func buildSlashCommands() []sugItem {
	return []sugItem{
		{"/clear", tr("clear the transcript view (history kept)", "清空转录视图（会话历史保留）"), "session"},
		{"/sessions", tr("list this workspace's saved sessions", "列出本工作区的已保存会话"), "session"},
		{"/resume", tr("restore a saved session — bare /resume opens a picker", "恢复已保存会话 — 裸 /resume 打开选择器"), "session"},
		{"/new", tr("start a fresh session (current one archived)", "开新会话（当前会话归档）"), "session"},
		{"/rollback", tr("undo history to a checkpoint — list, or /rollback N", "回退历史到检查点 — 列出或 /rollback N"), "session"},
		{"/quit", tr("exit", "退出"), "session"},
		{"/model", tr("model card: context window, official params, pricing", "模型卡：上下文窗口、官方参数、定价"), "model"},
		{"/mode", tr("switch plan (read-only) / bypass (auto-approve) / default", "切换 plan（只读）/ bypass（自动批准）/ default"), "model"},
		{"/think", tr("expand/collapse thinking blocks", "展开/折叠思考块"), "model"},
		{"/steps", tr("action audit panel: bare toggles, open/close/width N", "操作审计面板：裸命令开关，open/close/width N"), "panel"},
		{"/evolve", tr("evolution panel: leaderboard, exam clock (alt+e)", "进化面板：分类榜单、考试时钟（alt+e）"), "panel"},
		{"/route", tr("task classifier + model profile — /route <text> routes a task", "任务分类器 + 模型画像 — /route <text> 给任务路由"), "evolve"},
		{"/exam", tr("closed-book exam — /exam [class], auto every 10 rounds", "闭卷考试 — /exam [class]，每 10 轮自动考"), "evolve"},
		{"/contest", tr("seat competition — the whole candidate field vs held-out tasks", "竞争上岗 — 全部候选同考 held-out 任务"), "evolve"},
		{"/agents", tr("candidate field: list · add <id> <model> · rm <id>", "候选名单：list · add <id> <model> · rm <id>"), "evolve"},
		{"/market", tr("task hour market: history ledger + per-class estimates", "任务工时市场：历史台账 + 分类预估"), "evolve"},
		{"/flow", tr("workflows: list · show · match · export · save <slug>", "工作流：list · show · match · export · save <slug>"), "evolve"},
		{"/stats", tr("usage table — tokens, cache hits, tool latencies", "用量表 — token、缓存命中、工具耗时"), "misc"},
		{"/memory", tr("layered memory store — view, toggle hot, cloud sync", "分层记忆库 — 查看、热词开关、云端同步"), "misc"},
		{"/remember", tr("distill this session's learnings into memory now", "立即把本会话的收获提炼进记忆"), "misc"},
		{"/paste", tr("attach the clipboard image to your next prompt (alt+v)", "把剪贴板图片附到下一条提问（alt+v）"), "misc"},
		{"/timer", tr("timers: list next fire/status/runs · add · enable/disable · rm", "定时器：列出下次触发/状态/次数 · add · enable/disable · rm"), "misc"},
		{"/lang", tr("interface language: /lang zh|en", "界面语言: /lang zh|en"), "misc"},
		{"/help", tr("list commands and keys", "列出命令和快捷键"), "misc"},
	}
}

// refresh rebuilds the viewport content from the transcript. Height and
// bottom-pinning are owned by View — the single frame pass recomputes the
// layout each render; a second owner here fought it (#39).
func (m *tuiModel) refresh() {
	m.vp.SetContent(m.renderTranscript())
}

// suggestions matches the current input against the command list. The popup
// shows while the input is one bare slash token ("/stats", "/th", …); a
// space ends it (the argument commands answer from slash(), not the popup)
// and esc hides it until typing. Bare "/" browses one category tab at a
// time (←/→ switch); a typed prefix searches across every category.
func (m *tuiModel) suggestions() []sugItem {
	if m.sugHide || m.appr != nil {
		return nil
	}
	v := m.ti.Value()
	// The @ file popup (#64) shares this surface: a trailing bare
	// @token browses the workspace instead of the command list.
	if _, start, ok := atToken(v); ok {
		return atEntries(m.workspace, v[start+1:]) // strip the "@"
	}
	if !strings.HasPrefix(v, "/") || strings.ContainsAny(v, " \t") {
		return nil
	}
	cat := ""
	if v == "/" && m.sugCat >= 0 && m.sugCat < len(sugCats) {
		cat = sugCats[m.sugCat]
	}
	out := make([]sugItem, 0, len(slashCommands))
	for _, c := range slashCommands {
		if cat != "" && c.cat != cat {
			continue
		}
		if strings.HasPrefix(c.cmd, v) {
			out = append(out, c)
		}
	}
	return out
}

// sugBrowse reports whether the popup is in category-browse mode (a bare
// "/" with matches): the tab strip shows and ←/→ switch categories; with a
// typed prefix the arrows stay with the text cursor.
func (m *tuiModel) sugBrowse() bool {
	return len(m.sugs) > 0 && m.ti.Value() == "/"
}

// pageOverlap is how many lines a pgup/pgdn hop keeps on screen as
// reading context — a full-viewport jump loses the reader's place (#65).
const pageOverlap = 3

// pageScroll moves the transcript viewport by one page minus the
// overlap: less is a scroll, more is a jump, and the reader never has to
// guess what vanished off the top. SetYOffset clamps at both ends.
func (m *tuiModel) pageScroll(up bool) {
	step := m.vp.Height - pageOverlap
	if step < 1 {
		step = 1
	}
	if up {
		m.vp.SetYOffset(m.vp.YOffset - step)
	} else {
		m.vp.SetYOffset(m.vp.YOffset + step)
	}
	m.follow = m.vp.AtBottom()
}

// updateSuggestions refreshes the popup cache. It no longer touches the
// viewport: height and bottom-pinning belong to View alone — a second
// height formula here (header/cmdOut rows uncounted) fought View's clamp
// and made idle typing jump the tail (#39).
func (m *tuiModel) updateSuggestions() {
	v := m.ti.Value()
	if !strings.HasPrefix(v, "/") {
		m.sugHide = false // leaving slash-land re-arms the popup
		m.sugIdx = 0
	}
	m.sugs = m.suggestions()
	if m.sugIdx >= len(m.sugs) {
		m.sugIdx = 0
	}
}

// suggestionRows renders the popup rows: matches (selected one highlighted),
// then a key hint while more than one command matches. While the session
// picker is open it owns the same space instead.
func (m *tuiModel) suggestionRows() []string {
	if m.appr != nil { // approval card owns the bottom
		return m.approvalRows()
	}
	if len(m.pick) > 0 {
		return m.pickRows()
	}
	if len(m.sugs) == 0 {
		// Nothing to pick — but parked prompts deserve to be READABLE,
		// not just counted in the status line (#52).
		return m.queueRows()
	}
	browse := m.sugBrowse()
	rows := make([]string, 0, len(m.sugs)+2)
	if browse { // category tab strip on top (#42)
		rows = append(rows, m.sugTabRow())
	}
	for i, s := range m.sugs {
		if i == m.sugIdx {
			rows = append(rows, paint(cCyan, "❯ "+s.cmd)+paint(cDim, "  "+s.desc))
		} else {
			rows = append(rows, paint(cCyan, "  "+s.cmd)+paint(cDim, "  "+s.desc))
		}
	}
	if browse || len(m.sugs) > 1 {
		if m.atPopup() {
			rows = append(rows, paint(cDim, "  "+tr("↑↓ choose · enter/tab insert · esc close",
				"↑↓ 选择 · enter/tab 插入 · esc 关闭")))
		} else {
			rows = append(rows, paint(cDim, "  "+tr("↑↓ choose · ←/→ category · tab complete · esc close",
				"↑↓ 选择 · ←/→ 切换类别 · tab 补全 · esc 关闭")))
		}
	}
	return rows
}

// sugTabRow renders the category tab strip with the active tab bracketed.
func (m *tuiModel) sugTabRow() string {
	var b strings.Builder
	b.WriteString("  ")
	for i, c := range sugCats {
		label := sugCatLabel(c)
		if i == m.sugCat {
			b.WriteString(paint(cCyan, "["+label+"]"))
		} else {
			b.WriteString(paint(cDim, label))
		}
		if i < len(sugCats)-1 {
			b.WriteString(paint(cDim, " · "))
		}
	}
	return b.String()
}

// approvalRows renders the tool approval as a card: the tool name, a peek
// at its input, and three selectable option cards (y/n/a) with the current
// choice highlighted. ↑/↓/tab move, enter commits (updateApproval).
func (m *tuiModel) approvalRows() []string {
	rows := []string{paint(cBold+cYellow, "approve? "+m.appr.ev.Name)}
	if in := shortJSON(m.appr.ev.Input, 400); in != "" {
		rows = append(rows, paint(cDim, "  "+truncateRunes(in, max(20, m.width-8))))
	}
	opts := []struct{ key, label string }{
		{"y", tr("approve", "批准")},
		{"n", tr("deny", "拒绝")},
		{"a", tr("always", "总是允许")},
	}
	row := "  "
	for i, o := range opts {
		label := fmt.Sprintf(" %s %s ", o.key, o.label)
		if i == m.apprSel {
			row += paint(cBold+cCyan, "["+label+"]") + " "
		} else {
			row += paint(cDim, label) + " "
		}
	}
	rows = append(rows, row)
	rows = append(rows, paint(cDim, "  "+tr("↑↓/tab choose · enter confirm · y/n/a direct",
		"↑↓/tab 选择 · enter 确认 · y/n/a 直接")))
	return rows
}

// queueRows previews the prompts parked while a run is busy — the count
// in the status line answers "how many", this answers "what" (#52).
func (m *tuiModel) queueRows() []string {
	if len(m.queued) == 0 {
		return nil
	}
	rows := make([]string, 0, len(m.queued)+1)
	rows = append(rows, paint(cDim, tr(fmt.Sprintf("queued (%d):", len(m.queued)),
		fmt.Sprintf("已排队 (%d)：", len(m.queued)))))
	for i, q := range m.queued {
		rows = append(rows, paint(cDim, fmt.Sprintf("  %d› %s", i+1,
			truncateRunes(oneLine(q), max(12, m.width-8)))))
	}
	return rows
}

// pickRows renders the session picker: a header, one row per saved session
// (newest first, the highlighted one marked), and the key hint.
func (m *tuiModel) pickRows() []string {
	rows := make([]string, 0, len(m.pick)+2)
	rows = append(rows, paint(cBold, tr(
		fmt.Sprintf("resume — saved sessions (%d, newest first)", len(m.pick)),
		fmt.Sprintf("恢复 — 已保存会话（%d 个，最新在前）", len(m.pick)))))
	for i, r := range m.pick {
		head := tr(fmt.Sprintf("%2d  %s  %d messages", i+1,
			r.SavedAt.Format("2006-01-02 15:04"), r.Messages),
			fmt.Sprintf("%2d  %s  %d 条消息", i+1,
				r.SavedAt.Format("2006-01-02 15:04"), r.Messages))
		if r.Title != "" {
			head += "  " + sanitizeText(r.Title)
		}
		if i == m.pickIdx {
			rows = append(rows, paint(cCyan, "❯ "+head))
		} else {
			rows = append(rows, "  "+head)
		}
	}
	rows = append(rows, paint(cDim, "  "+tr("↑/↓ choose · enter restore · esc close",
		"↑/↓ 选择 · enter 恢复 · esc 关闭")))
	return rows
}

// renderTranscript styles and wraps every line for the current width.
// Assistant text gets markdown-lite treatment: inline spans (code, links,
// bold) through styleTextLine; block cues — pipe tables, list hanging
// indent, ─ rules — dispatched here, where width and fence state are
// known, and emitted pre-aligned through emitRaw because ansi.Wrap would
// shred the grid. Everything else was styled when it was pushed.
// Contiguous thinking lines render as one block: a fold header, with the
// body only while the block streams or thinking is globally expanded.
func (m *tuiModel) renderTranscript() string {
	w := m.tw // transcript column — narrower when the steps panel splits
	if w <= 0 {
		w = 80
	}
	var b strings.Builder
	first := true
	emit := func(s string) {
		if !first {
			b.WriteByte('\n')
		}
		first = false
		b.WriteString(ansi.Wrap(s, w, " "))
	}
	// emitRaw writes output that is already laid out — aligned table rows,
	// wrapped list items — so ansi.Wrap cannot reflow it.
	emitRaw := func(s string) {
		if !first {
			b.WriteByte('\n')
		}
		first = false
		b.WriteString(s)
	}
	inFence := false
	for i := 0; i < len(m.lines); {
		l := m.lines[i]
		// The ``` delimiter itself is renderer bookkeeping, not content:
		// toggle the fence and drop the line so code blocks read as plain
		// content between invisible bounds (GFM visual convention).
		if l.kind == lineText && strings.HasPrefix(strings.TrimSpace(l.raw), "```") {
			inFence = !inFence
			i++
			continue
		}
		if l.kind == lineThink {
			// Every thinking line folds, whatever its blk tag: replayed
			// history carries blk=0 (curBlk was idle) and used to bypass
			// the fold entirely, always rendering expanded.
			j := i + 1
			for j < len(m.lines) && m.lines[j].kind == lineThink && m.lines[j].blk == l.blk {
				j++
			}
			group := m.lines[i:j]
			open := m.thinkBlockOpen(group)
			emit(m.thinkHeader(group, open))
			if open {
				for _, tl := range group {
					emit(m.styleLine(tl, inFence, &inFence))
				}
			}
			i = j
			continue
		}
		if l.kind == lineText && !inFence {
			if isTableLine(l.raw) {
				j := i + 1
				for j < len(m.lines) && m.lines[j].kind == lineText && isTableLine(m.lines[j].raw) {
					j++
				}
				run := make([]string, j-i)
				for k := i; k < j; k++ {
					run[k-i] = m.lines[k].raw
				}
				if rows, ok := renderTable(run, w); ok {
					for _, r := range rows {
						emitRaw(r)
					}
					i = j
					continue
				}
				// No separator row: fall through so this line renders as
				// ordinary prose with its pipes intact (the next lines
				// re-test the same way). Width is no longer a reason to
				// give up — renderTable shrinks columns to fit.
			} else if isHRule(l.raw) {
				emitRaw(paint(cDim, strings.Repeat("─", min(w, 60))))
				i++
				continue
			} else if ind, mk, sp, rest, ok := listSplit(l.raw); ok {
				emitRaw(wrapListItem(ind, mk, sp, rest, w))
				i++
				continue
			}
		}
		emit(m.styleLine(l, inFence, &inFence))
		i++
	}
	if m.pend != "" &&
		(m.pendKind != lineText || !strings.HasPrefix(strings.TrimSpace(m.pend), "```")) {
		// The partial line is styled but must not toggle fence state — it
		// will, exactly once, when it commits as a full line. A pending
		// fence delimiter renders nothing — the committed line will toggle
		// and vanish, so drawing the partial first would only flash it.
		// A pending thinking line streams into the live block: strict fold
		// keeps it hidden with the rest until ctrl+t expands (the header
		// preview still shows the first committed line).
		if m.pendKind != lineThink || m.thinkOpen {
			emit(m.stylePending(inFence))
		}
	}
	return b.String()
}

// thinkBlockOpen reports whether a block shows its body — the global
// toggle only. The fold is strict: even the block still streaming stays
// behind its header, so the last answer never auto-expands under a closed
// fold; ctrl+t (or /think) opens everything.
func (m *tuiModel) thinkBlockOpen(group []tline) bool {
	// ctrl+o expands every fold at once; ctrl+t keeps its thinking-only
	// meaning underneath.
	return m.expandAll || m.thinkOpen
}

// thinkHeader is the fold marker — "thinking · N lines" plus a one-line
// preview while folded. No leading arrow: on fonts that substitute geometric
// glyphs it rendered as a stray gutter dot.
func (m *tuiModel) thinkHeader(group []tline, open bool) string {
	unit := "lines"
	if len(group) == 1 {
		unit = "line"
	}
	s := paint(cDim+cMagenta, "thinking") +
		paint(cDim, fmt.Sprintf(" · %d %s", len(group), unit))
	if open {
		return s
	}
	for _, l := range group {
		if p := strings.TrimSpace(l.raw); p != "" {
			return s + paint(cDim, "  "+truncateRunes(p, 60))
		}
	}
	return s
}

// toggleThinking flips the global thinking fold and reports it on the status
// line (it survives until the next prompt boundary).
func (m *tuiModel) toggleThinking() {
	m.thinkOpen = !m.thinkOpen
	if m.thinkOpen {
		m.status = tr("thinking expanded — ctrl+t folds again", "思考已展开 — ctrl+t 再折叠")
	} else {
		m.status = tr("thinking folded — ctrl+t expands", "思考已折叠 — ctrl+t 展开")
	}
	m.refresh()
}

// truncateRunes cuts s to at most n runes, appending an ellipsis.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// sanitizeText scrubs terminal-hostile bytes out of content we do not
// control — model text, thinking, tool output, errors. A raw or truncated
// escape sequence inside a rendered frame is lethal: a CSI still collecting
// parameters executes the renderer's own \r\n separators as control bytes,
// terminal and renderer then disagree about the row count, and the frame
// tears (top rows clipped, stale rows at the bottom — including a second
// copy of the input line). Carriage returns (progress bars) rewrite rows
// mid-line, and tabs expand to unmeasured widths. Everything goes — even
// SGR colors: our wrappers paint each line, and clipReplay/truncateRunes
// cut by rune count, which would happily slice a color code in half.
// Newlines are kept (line structure); tabs become four spaces. Idempotent.
// Emoji-presentation symbols that terminals render double-width while
// ansi.StringWidth counts them as one (⏹ ⏸ ⏺ ⚒ ⚠ ✎ ⌘ ★ 🖼 ⟳ — a
// width-measure mismatch that soft-wraps a full-width line and tears the
// whole frame, #46) are rewritten to measured-safe stand-ins.
func sanitizeText(s string) string {
	if !strings.ContainsAny(s, "\x1b\r\t\x7f⏸⏹⏺⚒⚠✎⌘★🖼⟳") && !hasCtl(s) {
		return s // fast path: ordinary text
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == 0x1b:
			i++ // drop the whole escape sequence, even a dangling ESC
			if i < len(s) && s[i] == '[' {
				i++
				for i < len(s) && !(s[i] >= 0x40 && s[i] <= 0x7e) {
					i++
				}
				i++ // final byte (or end)
			} else if i < len(s) && (s[i] == ']' || s[i] == 'P' || s[i] == 'X' || s[i] == '^' || s[i] == '_') {
				i++ // OSC/DCS/SOS/PM/APC: swallow until BEL or ST
				for i < len(s) {
					if s[i] == 0x07 {
						i++
						break
					}
					if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
						i += 2
						break
					}
					i++
				}
			} else if i < len(s) {
				i++ // two-byte escape
			}
		case c == '\r':
			i++ // \r\n loses only the \r; a lone progress-bar \r goes too
		case c == '\t':
			b.WriteString("    ")
			i++
		case c == '\n':
			b.WriteByte(c)
			i++
		case c < 0x20 || c == 0x7f:
			i++ // stray C0 / DEL
		default:
			b.WriteByte(c)
			i++
		}
	}
	return wideRisk.Replace(b.String())
}

// wideRisk maps symbols the terminal may draw double-width (emoji font
// fallback) although ansi measures them single — the mismatch that wraps
// a full-width row by one cell (#46).
var wideRisk = strings.NewReplacer(
	"⏸", "■", "⏹", "■", "⏺", "●", "⚒", "»", "⚠", "!",
	"✎", "·", "⌘", "·", "★", "*", "🖼", "·", "⟳", "»",
)

// hasCtl reports control bytes other than \n — the fast-path gate for
// sanitizeText without allocating.
func hasCtl(s string) bool {
	for i := 0; i < len(s); i++ {
		if (s[i] < 0x20 && s[i] != '\n') || s[i] == 0x7f {
			return true
		}
	}
	return false
}

// styleLine renders one committed line, advancing inFence for text lines.
func (m *tuiModel) styleLine(l tline, inFence bool, state *bool) string {
	switch l.kind {
	case linePrompt:
		return paint(cGreen, "❯ ") + l.raw
	case lineThink:
		return paint(cDim+cMagenta, l.raw)
	case lineText:
		trimmed := strings.TrimSpace(l.raw)
		if strings.HasPrefix(trimmed, "```") {
			*state = !inFence
			return paint(cDim, l.raw)
		}
		if inFence {
			return paint(cCyan, l.raw)
		}
		return styleTextLine(l.raw)
	default:
		return l.raw
	}
}

// stylePending renders the unterminated partial line without mutating
// fence state.
func (m *tuiModel) stylePending(inFence bool) string {
	switch m.pendKind {
	case linePrompt:
		return paint(cGreen, "❯ ") + m.pend
	case lineThink:
		return paint(cDim+cMagenta, m.pend)
	case lineText:
		if strings.HasPrefix(strings.TrimSpace(m.pend), "```") {
			return paint(cDim, m.pend)
		}
		if inFence {
			return paint(cCyan, m.pend)
		}
		return styleTextLine(m.pend)
	default:
		return m.pend
	}
}

// styleTextLine applies markdown-lite cues to a raw assistant line: block
// cues that fit one line (ATX heading, blockquote) first, then inline
// spans via inlineStyle (markdown.go). Fenced code never reaches here —
// styleLine diverts it while inFence.
func styleTextLine(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return raw
	}
	if text, ok := stripHeading(raw); ok {
		return tint(cBold, inlineStyle(text))
	}
	if t := strings.TrimSpace(raw); strings.HasPrefix(t, ">") {
		body := strings.TrimSpace(strings.TrimPrefix(t, ">"))
		return tint(cDim, "│ "+inlineStyle(body))
	}
	return inlineStyle(raw)
}

// View assembles the frame: header, transcript viewport, status, optional
// popup, input. The viewport height is recomputed here rather than trusted
// from updateSuggestions — approval and picker state can change without an
// input change — so the frame is always exactly m.height rows. Every chrome
// row is clipped to m.width: one row wider than the terminal soft-wraps,
// which scrolls the altscreen frame by a line and desyncs the renderer
// (stale rows at the bottom, top rows pushed off).
func (m *tuiModel) View() string {
	head := m.headerRows() // identity line + wordmark + top rule (pinned)
	rows := m.suggestionRows()
	// The popup can never push the frame: chrome + one transcript row
	// bound what it may claim — a giant list clips (windowing keeps the
	// selection visible), same as the session picker.
	if max := m.height - len(head) - 4; len(rows) > max {
		if max <= 0 {
			rows = nil
		} else {
			sel := m.sugIdx
			if len(m.pick) > 0 {
				sel = m.pickIdx + 1 // picker: row 0 is its header
			}
			lo := 0
			if sel >= max {
				lo = sel - max + 1
			}
			if hi := lo + max; hi > len(rows) {
				lo = len(rows) - max
				hi = len(rows)
			}
			rows = rows[lo : lo+max]
		}
	}
	// Below the transcript: rule + status + popup + input (3 + rows).
	// The output box lives BELOW the input — a drawer under the chat, so
	// input and output read as two separate zones (#48) and the split
	// panel keeps the full transcript height (no right-hand void).
	below := 3 + len(rows)
	avail := m.height - len(head) - below
	if avail < 1 {
		avail = 1
	}
	outBudget := avail - 3 // keep a 3-row transcript floor for the drawer
	if outBudget < 0 {
		outBudget = 0
	}
	cmd := m.cmdOutView(outBudget)
	vh := avail - len(cmd)
	if vh < 1 {
		vh = 1
	}
	m.vp.Height = vh
	// Pin to the tail unless scrolled away — and while a prompt streams
	// under a half-written question the tail freezes instead (the draft
	// must not scroll under the typing, #39/#43).
	if m.follow && !(m.busy && m.ti.Value() != "") {
		m.vp.GotoBottom()
	} else {
		m.vp.SetYOffset(m.vp.YOffset) // re-clamp: Height may have just shrunk
	}
	var b strings.Builder
	for i, l := range head {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(l)
	}
	b.WriteByte('\n')
	if m.pw > 0 {
		b.WriteString(m.composeSplit(vh))
	} else {
		b.WriteString(m.vp.View())
	}
	b.WriteByte('\n')
	b.WriteString(m.clip(ruleRow(m.width))) // session area │ input area
	b.WriteByte('\n')
	b.WriteString(m.clip(m.statusLine()))
	b.WriteByte('\n')
	for _, r := range rows {
		b.WriteString(m.clip(r))
		b.WriteByte('\n')
	}
	b.WriteString(m.clip(m.inputLine()))
	for _, l := range cmd {
		b.WriteByte('\n')
		b.WriteString(l)
	}
	frame := b.String()
	// Exact-height contract: always emit m.height rows. A short frame
	// leaves stale rows for the renderer to clear — the Trae terminal
	// mishandled that and tore the whole grid ("终端会坏掉"); blank tail
	// rows cost nothing and keep the frame height constant.
	if n := strings.Count(frame, "\n") + 1; n < m.height {
		frame += strings.Repeat("\n", m.height-n)
	}
	return frame
}

// cmdOutMaxRows caps the temporary area's content rows — the "+N more"
// hint keeps long answers discoverable without ever hiding the chat.
const cmdOutMaxRows = 10

// cmdOutView renders the temporary command area: a dim rule plus the
// latest command's answer, head-clamped with a "+N more" hint so a long
// table can never swallow the transcript. Returns nil when the terminal
// is too short to spare the rows — the conversation always wins.
func (m *tuiModel) cmdOutView(budget int) []string {
	if len(m.cmdOut) == 0 {
		return nil
	}
	max := cmdOutMaxRows
	if budget-4 < max { // rule + hint, and 4 rows of transcript floor
		max = budget - 4
	}
	if max > len(m.cmdOut) {
		max = len(m.cmdOut)
	}
	if max < 1 {
		return nil
	}
	out := []string{m.clip(paint(cDim, "── output ──"))}
	for _, l := range m.cmdOut[:max] {
		out = append(out, m.clip(l))
	}
	if n := len(m.cmdOut) - max; n > 0 {
		out = append(out, m.clip(paint(cDim, fmt.Sprintf("…(+%d more)", n))))
	}
	return out
}

// composeSplit joins the transcript viewport with the steps panel row by
// row: [transcript, tw cells] │ [right column, pw cells], exactly vh
// rows. The right column composes bottom up (#1/#13): one shared key-hint
// footer, the resident plan block above it, the live-output band above
// that (steps mode only), and whichever panel (steps or evolution)
// fills what's left — the plan parks in the corner with either panel
// open, the band reads the selected (or live) step's #11 output store in
// between. Transcript rows are clipped and padded to tw so the gutter and
// the right edge stay on their columns.
func (m *tuiModel) composeSplit(vh int) string {
	vlines := strings.Split(m.vp.View(), "\n")
	avail := vh - 1 // budget: the column minus its shared footer
	if avail < 0 {
		avail = 0
	}
	planH := planSectionH(m.todos, avail) // the seat keeps first claim (#1)
	if planH > avail {
		planH = avail
	}
	mid := avail - planH // rows the top segments may claim
	outH := 0
	if !m.evoOn {
		outH = m.stepOutSectionH(m.pw, mid)
	}
	panelH := mid - outH
	if panelH < 0 {
		panelH = 0
	}
	var col []string
	if panelH > 0 {
		col = m.renderSteps(m.pw, panelH)
		if m.evoOn {
			col = m.renderEvolve(m.pw, panelH)
		}
	}
	if outH > 0 {
		col = append(col, m.renderStepOut(m.pw, outH)...)
	}
	col = append(col, m.renderPlan(m.pw, planH)...)
	if vh >= 1 {
		footer := stepsFooter(m.pw)
		if m.evoOn {
			footer = evoFooter(m.pw)
		}
		col = append(col, footer)
	}
	// The render contracts make col exactly vh rows; the join below
	// indexes col[i], so pad/trim anyway — an edge case must degrade to
	// blank rows, never panic the frame.
	for len(col) < vh {
		col = append(col, "")
	}
	col = col[:vh]

	var b strings.Builder
	for i := 0; i < vh; i++ {
		if i > 0 {
			b.WriteByte('\n')
		}
		trow := ""
		if i < len(vlines) {
			trow = vlines[i]
		}
		if ansi.StringWidth(trow) > m.tw {
			trow = ansi.Truncate(trow, m.tw, "…")
		}
		if pad := m.tw - ansi.StringWidth(trow); pad > 0 {
			trow += strings.Repeat(" ", pad)
		}
		b.WriteString(trow)
		b.WriteByte(' ')
		b.WriteString(paint(cDim, "│"))
		b.WriteString(col[i]) // exactly pw cells (render contract)
	}
	return b.String()
}

// clip hard-truncates one chrome line to the terminal width. Chrome is
// ephemeral (headers, hints, prompts) — losing its tail beats losing the
// frame's row alignment.
func (m *tuiModel) clip(s string) string {
	if m.width <= 0 || ansi.StringWidth(s) <= m.width {
		return s
	}
	return ansi.Truncate(s, m.width, "…")
}

func (m *tuiModel) statusLine() string {
	if m.appr != nil {
		// The card (suggestionRows) carries the input peek and the
		// options; the status line just names the decision.
		return paint(cYellow, fmt.Sprintf("approve? %s  [↑↓/tab · enter · y/n/a]",
			m.appr.ev.Name))
	}
	if m.busy {
		s := m.spin.View() + " working…"
		if m.curAgent != "" {
			// Live agent badge (#50): which class/model is on the job —
			// visible mid-run, not only when the score card lands. The
			// model shown is the one actually running: routing may have
			// swapped it at submit (#53).
			model := m.runModel
			if model == "" {
				model = m.cfg.Model
			}
			s += paint(cCyan, "  agent "+m.curAgent) +
				paint(cDim, " · "+shortModel(model))
		}
		// Readable step feedback (#66): the plan scale rides the busy
		// line — progress is glanceable without scrolling the block.
		if done, total, cur := m.todoProgress(); total > 0 {
			s += paint(cYellow, fmt.Sprintf("  📋 %d/%d", done, total))
			if cur != "" {
				s += paint(cDim, " ▸ "+truncateRunes(cur, 48))
			}
		}
		if m.status != "" && m.status != idleStatus {
			s += paint(cDim, "  "+m.status)
		} else {
			s += paint(cDim, "  ctrl+c interrupt")
		}
		return s
	}
	// Transient toasts get a few seconds of the status line, then the
	// resident legend takes the row back (#45 — after ctrl+o "这里要一直
	// 是原始的那个提示"): feedback still lands, but it never parks there.
	// Async completions that clear the status to "" fall back the same way.
	if m.status != m.statusSeen {
		m.statusSeen = m.status
		m.statusAt = time.Now()
	}
	if m.status == "" || m.status == idleStatus || time.Since(m.statusAt) > statusToastTTL {
		return paint(cDim, idleStatus)
	}
	return paint(cDim, m.status)
}

// statusToastTTL is how long a status toast holds the idle status line
// before the resident key legend reclaims it.
const statusToastTTL = 3 * time.Second

// inputLine is the bottom row: the text field on the left, and the
// resident context/cost gauge pinned to the bottom-right corner (moved
// out of the header — the corner reads as "the session's fuel gauge"
// while typing). Tight rows keep the input: a clipped field hurts more
// than a missing figure.
func (m *tuiModel) inputLine() string {
	if m.appr != nil {
		return paint(cGreen, "y") + paint(cDim, tr("=approve  ", "=批准  ")) +
			paint(cRed, "n") + paint(cDim, tr("=deny  ", "=拒绝  ")) +
			paint(cYellow, "a") + paint(cDim, tr("=always", "=总是允许"))
	}
	// textinput pads its view to ti.Width, so the corner seat must be
	// carved out of the field itself: size the field to leave the gauge
	// its width plus a 3-cell gutter (prompt + cursor already claim 3 of
	// the row). Under 40 cells of input the gauge goes — a cramped field
	// hurts more than a missing figure.
	seg := newGauge(m.cfg, m.sess.Stats(), m.sess.SubUsage()).headerSeg()
	want := m.width - 3
	if seg != "" {
		if w := m.width - ansi.StringWidth(seg) - 6; w >= 40 {
			want = w
		} else {
			seg = ""
		}
	}
	m.ti.Width = want
	in := m.ti.View()
	if seg == "" {
		return in
	}
	gap := m.width - ansi.StringWidth(in) - ansi.StringWidth(seg)
	return in + strings.Repeat(" ", gap) + seg
}

func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.relayout() // sets vp.Width: transcript column (panel may split it)
		// prompt (2 cells) + value field + cursor (1 cell) must fit the
		// row exactly; -2 left the cursor one cell past the right edge.
		m.ti.Width = msg.Width - 3
		m.updateSuggestions() // sets vp.Height: header + status + input + popup
		m.refresh()
		// Resizes leave stale cells on some terminals (Trae): clear and
		// repaint the whole frame instead of trusting a diff.
		cmds = append(cmds, tea.ClearScreen)

	case tuiEv:
		m.applyEvent(msg.ev)
		m.refresh()
		// Queue drain: each prompt_end releases exactly one parked line
		// (submit's busy branch), chaining until the queue runs dry.
		if msg.ev.Type == agent.EventPromptEnd && m.kickQueued() {
			cmds = append(cmds, m.spin.Tick)
		}

	case spinner.TickMsg:
		if !m.busy {
			break
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		cmds = append(cmds, cmd)

	case approveReq:
		if m.allowAll[msg.ev.Name] {
			msg.ch <- true // "a" already vouched for this tool this session
			break
		}
		// First-come overlay; apprMu upstream guarantees no overlap.
		m.appr = &msg
		m.apprSel = 0 // card opens on "approve"
		// The user is probably not watching the TUI — raise a system box.
		notifyDecision("lyon: approval needed",
			truncNotif(msg.ev.Name+" "+string(msg.ev.Input), 200))

	case titleUpdated:
		// The intent check finished; the next View re-reads Title().
		// A prompt_end that landed mid-check re-kicks now.
		m.titleBusy = false
		if m.titlePending {
			m.titlePending = false
			m.kickTitleCheck()
		}

	case pasteMsg:
		m.attachPaste(msg)

	case rememberDone:
		m.status = ""
		switch {
		case msg.err != nil:
			m.finishStep(msg.step, stepError, "", sanitizeText(msg.err.Error()))
			m.pushCmd(paint(cYellow, tr("remember: ", "记忆失败: ")+sanitizeText(msg.err.Error())))
		case msg.n == 0:
			m.finishStep(msg.step, stepDone, "nothing durable to add", "")
			m.pushCmd(paint(cDim, tr("remember: nothing durable to add",
				"remember: 没有值得留存的收获")))
		default:
			m.finishStep(msg.step, stepDone, fmt.Sprintf("saved %d fact(s)", msg.n), "")
			m.pushCmd(msg.echoLine())
		}
		m.refresh()

	case memSyncDone:
		m.status = ""
		if msg.err != nil {
			m.finishStep(msg.step, stepError, "", sanitizeText(msg.err.Error()))
			m.pushCmd(paint(cYellow, tr("memory sync: ", "记忆同步失败: ")+sanitizeText(msg.err.Error())))
		} else {
			m.finishStep(msg.step, stepDone,
				fmt.Sprintf("pushed %d, pulled %d", msg.pushed, msg.pulled), "")
			m.pushCmd(tr(fmt.Sprintf("%s sync: pushed %d, pulled %d",
				paint(cCyan, "🧠 memory"), msg.pushed, msg.pulled),
				fmt.Sprintf("%s 同步：推送 %d，拉取 %d",
					paint(cCyan, "🧠 memory"), msg.pushed, msg.pulled)))
		}
		m.refresh()

	case diagDone:
		switch {
		case msg.err != nil:
			m.finishStep(msg.step, stepError, "", sanitizeText(msg.err.Error()))
			m.sess.Hooks().Fire(hooks.Record{
				Event:   hooks.EvDiagnosis,
				IsError: true,
				Err:     sanitizeText(msg.err.Error()),
			})
		default:
			d := msg.diag
			detail := strings.Join(d.FailedConditions, "; ")
			if detail == "" {
				detail = d.Patch
			}
			if detail == "" {
				detail = "no checkable conditions extracted"
			}
			m.finishStep(msg.step, stepDone, detail, "")
			// The answer area shows the full post-mortem: conditions and
			// the constraint patch a retry would inherit.
			lines := []string{paint(cMagenta, "· 错因归纳")}
			for _, c := range d.FailedConditions {
				lines = append(lines, "• "+sanitizeText(c))
			}
			if d.Patch != "" {
				lines = append(lines, paint(cDim, "patch: "+sanitizeText(d.Patch)))
			}
			m.pushCmd(lines...)
			// Writeback: the post-mortem joins the audit log — the §1.3
			// record a later session can recall (and the flow retry hook
			// pattern reads from).
			m.sess.Hooks().Fire(hooks.Record{
				Event:  hooks.EvDiagnosis,
				Output: sanitizeText(d.String()),
			})
			// Memory writeback: the lesson pins into the project layer's
			// hot tier, so the NEXT session's MemoryFacts carries it —
			// deduped and audited inside, best-effort like distill.
			_ = rememberDiagnosis(m.workspace, m.sess.Hooks().Fire, d)
			// Space writeback: the CURRENT agent owns the lesson about
			// its own failure — self-evolution's disk half.
			rememberDiagnosisSpace(m.sess.Agent(), m.sess.Hooks().Fire, d)
			// 自己进化: the same lesson spawns the next generation of
			// this agent (child inherits the space; gen capped at 3).
			if child, err := evolveFromDiagnosis(m.sess, d); err == nil {
				m.evoOutput(paint(cMagenta, "· evolve") +
					fmt.Sprintf("  %s spawned from %s (gen %d)", child.ID, child.Parent, child.Gen))
			}
		}
		m.refresh()

	case scoreDone:
		m.status = ""
		switch {
		case msg.err != nil:
			// A failed grade files nothing — say so and move on.
			m.finishStep(msg.step, stepError, "", sanitizeText(msg.err.Error()))
			m.evoOutput(paint(cYellow, "· score") + "  " + sanitizeText(msg.err.Error()))
		default:
			m.finishStep(msg.step, stepDone, msg.line, "")
			m.evoOutput(paint(cCyan, "· score") + "  " + msg.line)
		}
		m.refresh()

	case examDone:
		m.status = ""
		switch {
		case msg.err != nil:
			m.finishStep(msg.step, stepError, "", sanitizeText(msg.err.Error()))
			m.evoOutput(paint(cYellow, tr("exam: ", "考试失败: ")+sanitizeText(msg.err.Error())))
		default:
			// §1.4: file the score, apply the exam clock, echo the table —
			// two consecutive failures by the sitting agent demote them.
			// The filing (and §7.3's evolve-on-fail) is evolve.FinishExam —
			// the same unit the daemon's prompt_end tap runs.
			demoted, child, ferr := evolve.FinishExam(m.sess, m.cfg.Model, msg.class, msg.res)
			_ = ferr // a spawn failure (gen cap, disk) stays silent, as before
			m.finishStep(msg.step, stepDone, msg.res.Summary(), "")
			card := paint(cCyan, "· exam — "+msg.why) + "  " + msg.res.Summary()
			for _, a := range msg.res.Answers {
				if !a.OK {
					card += "\n" + paint(cRed, "  ✗ "+sanitizeText(oneLine(a.Q)))
				}
			}
			if demoted {
				card += "\n" + paint(cRed, "  demoted after two failures — the seat is open")
			}
			m.evoOutput(card)
			// 自己进化: a failed exam distills into the next generation —
			// the missed questions are the lesson (gen capped at 3).
			if child != nil {
				m.evoOutput(paint(cMagenta, "· evolve") +
					fmt.Sprintf("  %s spawned from %s (gen %d) — missed exam → lesson",
						child.ID, child.Parent, child.Gen))
			}
		}
		m.refresh()

	case contestDone:
		m.status = ""
		switch {
		case msg.err != nil:
			m.finishStep(msg.step, stepError, "", sanitizeText(msg.err.Error()))
			m.evoOutput(paint(cYellow, "· contest") + "  " + sanitizeText(msg.err.Error()))
		default:
			// Rank the field by held-out score — the election already
			// happened inside kickContest (lb.Elect + routing.Record);
			// this is the report (#53).
			rows := make([]contestRow, len(msg.rows))
			copy(rows, msg.rows)
			sort.SliceStable(rows, func(i, j int) bool { return rows[i].Pct > rows[j].Pct })
			seat := "(open)"
			if msg.winner != "" {
				seat = msg.winner
			}
			m.finishStep(msg.step, stepDone,
				fmt.Sprintf("%d candidates — %s seated", len(rows), seat), "")
			card := paint(cCyan, "· contest — "+msg.class) +
				paint(cDim, fmt.Sprintf("  held-out, closed-book · seat: %s", seat))
			for _, r := range rows {
				mark := "  "
				line := fmt.Sprintf("    %s%-14s %-24s %3.0f%%  ", mark, r.ID, r.Model, r.Pct*100)
				if r.ID == msg.winner {
					line = paint(cCyan, fmt.Sprintf("    → %-14s %-24s %3.0f%%  ", r.ID, r.Model, r.Pct*100))
				}
				if r.Passed {
					line += paint(cGreen, "PASS")
				} else {
					line += paint(cRed, "FAIL")
				}
				card += "\n" + line
			}
			card += "\n" + paint(cDim, "  routing now steers this class to the winner · /route to inspect")
			m.evoOutput(card)
		}
		m.refresh()

	case flowSaved:
		m.status = ""
		switch {
		case msg.err != nil:
			m.finishStep(msg.step, stepError, "", sanitizeText(msg.err.Error()))
			m.pushCmd(paint(cYellow, tr("flow save: ", "工作流保存失败: ")+sanitizeText(msg.err.Error())))
		default:
			m.finishStep(msg.step, stepDone,
				fmt.Sprintf("%s — %d nodes", msg.slug, msg.nodes), "")
			m.pushCmd(
				paint(cCyan, "workflow frozen — "+msg.slug),
				"  "+msg.exam.Summary(),
				"  "+msg.gateNote,
				paint(cDim, "  run: lyon run -flow "+msg.slug+
					"  ·  inspect: /flow show "+msg.slug),
			)
		}
		m.refresh()

	case dashTickMsg:
		cmds = append(cmds, m.dashTick())

	case tea.MouseMsg:
		before := m.vp.YOffset
		m.vp, _ = m.vp.Update(msg)
		if m.vp.YOffset != before {
			m.follow = m.vp.AtBottom()
		}

	case tea.KeyMsg:
		if m.appr != nil {
			m.updateApproval(msg, &cmds)
			return m, tea.Batch(cmds...)
		}
		if len(m.pick) > 0 { // picker is modal: it owns every key
			m.updatePick(msg)
			return m, nil
		}
		switch k := msg.String(); k {
		case "ctrl+c":
			if m.busy && !m.armedQuit {
				m.armedQuit = true
				if m.sess.Interrupt() {
					m.status = tr("interrupting… (ctrl+c again quits)", "正在中断…（再按 ctrl+c 退出）")
					// The interrupt collapses streaming rows mid-frame;
					// a clean repaint keeps the grid from tearing.
					return m, tea.Batch(m.spin.Tick, tea.ClearScreen)
				}
				return m, nil
			}
			return m, tea.Quit

		case "esc":
			// esc dismisses overlays only — output area, slash popup.
			// Interrupting a run belongs to ctrl+c alone now: too many
			// sessions were killed by a reflex esc aimed at the output
			// box ("我本来想管 output，把会话给中断了").
			m.armedQuit = false
			m.cmdOut = nil
			if len(m.sugs) > 0 {
				m.sugHide = true
				m.updateSuggestions()
			}
			return m, nil

		case "ctrl+o":
			// Claude Code-style: one toggle unfolds every collapsed
			// region (thinking blocks now, any future fold later).
			m.armedQuit = false
			m.expandAll = !m.expandAll
			if m.expandAll {
				m.status = tr("all folds expanded — ctrl+o folds back",
					"已展开全部折叠 — ctrl+o 收回")
			} else {
				m.status = tr("folds restored — ctrl+o expands all",
					"已恢复折叠 — ctrl+o 全部展开")
			}
			m.refresh()
			return m, nil

		case "tab":
			m.armedQuit = false
			if m.atPopup() && len(m.sugs) > 0 {
				m.insertAtSelection() // @token → path, not whole input
				return m, nil
			}
			if len(m.sugs) > 0 && m.sugIdx < len(m.sugs) {
				m.ti.SetValue(m.sugs[m.sugIdx].cmd)
				m.ti.CursorEnd()
				m.updateSuggestions()
			}
			return m, nil

		case "shift+tab":
			// Cycle execution mode: default → plan → bypass → default.
			m.armedQuit = false
			prev := m.sess.Mode()
			if _, err := m.sess.SetMode(cycleMode(prev)); err == nil {
				m.cmdOut = nil // feedback replaces, never stacks under, an old answer
				m.pushCmd(modeSwitchLine(prev, m.sess.Mode()))
				m.status = modeToast(m.sess.Mode())
				m.refresh()
			}
			return m, nil

		case "ctrl+t":
			m.armedQuit = false
			m.toggleThinking()
			return m, nil

		// Panel navigation (steps.go / evopanel.go): walk the audit,
		// expand the selected step, toggle the live-output band, resize
		// the column. Every arrow also answers to alt+shift+… because
		// IDEs (Trae) swallow plain alt+arrows. The panel owns these
		// while it is on; they never reach the input. Selection/band keys
		// are steps-only (evo panel has neither); resize applies to both.
		case "alt+up", "alt+shift+up":
			m.armedQuit = false
			if m.panelOn() && !m.evoOn {
				m.stepMove(-1)
			}
			return m, nil

		case "alt+down", "alt+shift+down":
			m.armedQuit = false
			if m.panelOn() && !m.evoOn {
				m.stepMove(+1)
			}
			return m, nil

		case "alt+enter", "alt+shift+enter":
			m.armedQuit = false
			if m.panelOn() && !m.evoOn {
				m.stepToggleOpen()
			}
			return m, nil

		// alt+o grows/collapses the live-output band (#13) — the middle
		// segment of the steps-mode column. The chord's hint rides the
		// band's own header, not the shared footer (#6's footer stays
		// exactly as the panel contract spells it).
		case "alt+o", "alt+shift+o":
			m.armedQuit = false
			if m.panelOn() && !m.evoOn {
				m.stepToggleOut()
			}
			return m, nil

		// Width semantics follow the physical intuition: ← moves the
		// gutter left (a wider panel), → moves it right (narrower). When
		// the panel is closed the ← family OPENS the steps window — the
		// "反的" complaint was ← shrinking a closed intent to expand.
		// ctrl+shift+… variants included because IDEs remap plain alt.
		case "alt+right", "alt+shift+right", "ctrl+right", "ctrl+shift+right":
			m.armedQuit = false
			if m.panelOn() {
				m.stepResize(-4)
			}
			return m, nil

		case "alt+left", "alt+shift+left", "ctrl+left", "ctrl+shift+left":
			m.armedQuit = false
			if !m.panelOn() {
				m.stepsOn = true
				m.relayout()
				m.status = tr("steps panel · alt+← widen · alt+→ narrow · alt+e close",
					"steps 面板 · alt+← 加宽 · alt+→ 收窄 · alt+e 关闭")
				m.refresh()
				return m, nil
			}
			m.stepResize(+4)
			return m, nil

		// alt+e flips the right column between the steps audit and the
		// evolution panel (/evolve does the same from the slash menu).
		case "alt+e", "alt+shift+e":
			m.armedQuit = false
			m.evoOn = !m.evoOn
			m.relayout()
			if m.evoOn {
				m.status = tr("evolution panel · alt+e close", "进化面板 · alt+e 关闭")
			} else {
				m.status = idleStatus
			}
			m.refresh()
			return m, nil

		// alt+v pastes a picture from the clipboard (clip_windows.go): the
		// pixels land in the workspace and the @path token naming them is
		// typed into the input. The read is async — it may wait out another
		// program's hold on the clipboard — so the TUI keeps painting.
		case "alt+v":
			m.armedQuit = false
			if m.pasting {
				return m, nil // one read at a time
			}
			m.pasting = true
			m.status = tr("pasting image…", "正在粘贴图片…")
			return m, pasteCmd(m.workspace)

		case "enter":
			m.armedQuit = false
			return m, m.submit()

		case "up":
			m.armedQuit = false
			// The @ file popup owns the arrows even with one entry —
			// mid-token, history recall would clobber the path (#64).
			if m.atPopup() && len(m.sugs) > 0 {
				m.sugIdx = (m.sugIdx - 1 + len(m.sugs)) % len(m.sugs)
				return m, nil
			}
			if n := len(m.sugs); n > 1 { // popup open → it owns ↑/↓
				m.sugIdx = (m.sugIdx - 1 + n) % n
				return m, nil
			}
			if m.histIdx > 0 {
				m.histIdx--
				m.ti.SetValue(m.hist[m.histIdx])
				m.ti.CursorEnd()
				m.updateSuggestions()
			}
			return m, nil

		case "down":
			m.armedQuit = false
			if m.atPopup() && len(m.sugs) > 0 {
				m.sugIdx = (m.sugIdx + 1) % len(m.sugs)
				return m, nil
			}
			if n := len(m.sugs); n > 1 {
				m.sugIdx = (m.sugIdx + 1) % n
				return m, nil
			}
			if m.histIdx < len(m.hist) {
				m.histIdx++
				if m.histIdx == len(m.hist) {
					m.ti.SetValue("")
				} else {
					m.ti.SetValue(m.hist[m.histIdx])
				}
				m.ti.CursorEnd()
				m.updateSuggestions()
			}
			return m, nil

		case "left":
			m.armedQuit = false
			if m.sugBrowse() { // bare "/": ← switches the category tab
				m.sugCat = (m.sugCat - 1 + len(sugCats)) % len(sugCats)
				m.sugs = m.suggestions()
				m.sugIdx = 0
				return m, nil
			}
			var cmdL tea.Cmd
			m.ti, cmdL = m.ti.Update(msg)
			cmds = append(cmds, cmdL)

		case "right":
			m.armedQuit = false
			if m.sugBrowse() { // bare "/": → switches the category tab
				m.sugCat = (m.sugCat + 1) % len(sugCats)
				m.sugs = m.suggestions()
				m.sugIdx = 0
				return m, nil
			}
			var cmdR tea.Cmd
			m.ti, cmdR = m.ti.Update(msg)
			cmds = append(cmds, cmdR)

		case "pgup", "pgdown":
			m.armedQuit = false
			m.pageScroll(msg.String() == "pgup")
			return m, nil

		default:
			m.armedQuit = false
			m.histIdx = len(m.hist) // typing abandons recall
			var cmd tea.Cmd
			m.ti, cmd = m.ti.Update(msg)
			cmds = append(cmds, cmd)
			m.sugHide = false // typing re-arms the popup
			m.sugIdx = 0
			m.updateSuggestions()
		}

	default:
		var cmd tea.Cmd
		m.ti, cmd = m.ti.Update(msg)
		cmds = append(cmds, cmd)
	}
	return m, tea.Batch(cmds...)
}

func (m *tuiModel) updateApproval(msg tea.KeyMsg, cmds *[]tea.Cmd) {
	approve := func(always bool) {
		if m.appr == nil {
			return
		}
		if always {
			if m.allowAll == nil {
				m.allowAll = map[string]bool{}
			}
			m.allowAll[m.appr.ev.Name] = true // this tool, this session
		}
		m.appr.ch <- true
		m.appr = nil
	}
	deny := func() {
		if m.appr != nil {
			m.appr.ch <- false
			m.appr = nil
		}
	}
	switch msg.String() {
	case "up", "left", "shift+tab":
		m.apprSel = (m.apprSel + 2) % 3
	case "down", "right", "tab":
		m.apprSel = (m.apprSel + 1) % 3
	case "enter":
		switch m.apprSel {
		case 1:
			deny()
		case 2:
			approve(true)
		default:
			approve(false)
		}
	case "y", "Y":
		approve(false)
	case "a", "A":
		approve(true)
	case "n", "N", "esc":
		deny()
	case "ctrl+c":
		deny()
		*cmds = append(*cmds, func() tea.Msg { return tea.Quit() })
	}
}

// submit handles Enter: slash commands locally, everything else through
// the session. While a prompt is running the line is parked in the queue
// instead of dropped — each prompt_end drains one (FIFO).
func (m *tuiModel) submit() tea.Cmd {
	// The @ file popup owns enter (#64): a pick inserts the path instead
	// of sending the half-typed line. No match → fall through and send
	// whatever the user typed (PromptBlocks resolves or ignores it).
	if m.atPopup() && len(m.sugs) > 0 {
		m.insertAtSelection()
		return nil
	}
	line := sanitizeText(strings.TrimSpace(m.ti.Value()))
	if line == "" {
		return nil
	}
	if strings.HasPrefix(line, "/") || line == ":q" {
		// Resolve through the popup: a unique match runs as-is (or by
		// prefix, "/st" → /stats); with several matches Enter runs the
		// one the popup has highlighted — tab-then-enter was one step
		// too many for choosing.
		if s := m.sugs; len(s) > 1 {
			idx := m.sugIdx
			if idx < 0 || idx >= len(s) {
				idx = 0
			}
			line = s[idx].cmd
			m.status = ""
		} else if len(s) == 1 {
			line = s[0].cmd
			m.status = "" // popup hint has served its purpose
		}
		m.ti.SetValue("")
		m.updateSuggestions()
		// Audit trail: the command lands in the transcript so ↑ can find
		// it again — output still answers in the temporary area below.
		// /clear wipes this line together with the rest.
		m.flushPend()
		m.curBlk = 0
		m.lines = append(m.lines, tline{kind: linePrompt, raw: line}, tline{kind: lineStyled, raw: ""})
		m.follow = true
		m.refresh()
		return m.slash(line)
	}
	if m.busy {
		// Park the draft: enter never drops a keystroke while a prompt
		// runs. The line goes straight to the session too (#2) — it lands
		// at the next turn gap of the running prompt; the local park is
		// only the display mirror (removed when queue_injected arrives).
		// If the run ends first, Enqueue's entry was dropped there and
		// kickQueued sends the parked line the old way — never both.
		m.ti.SetValue("")
		m.updateSuggestions()
		m.queued = append(m.queued, line)
		_ = m.sess.Enqueue(line) // idle/closed race: the parked copy still goes out at prompt_end
		m.status = tr(fmt.Sprintf("queued (%d) — injects at the next gap of the running prompt", len(m.queued)),
			fmt.Sprintf("已排队 (%d) — 将插入当前提问的下一个间隙", len(m.queued)))
		m.refresh()
		return nil
	}
	return m.sendPrompt(line)
}

// sendPrompt runs one plain line through the session: history row,
// transcript prompt, steps group, spinner. It is also the drain step of
// the busy queue (kickQueued).
func (m *tuiModel) sendPrompt(line string) tea.Cmd {
	m.ti.SetValue("")
	m.updateSuggestions()
	m.cmdOut = nil // a new round clears the temporary answer area
	m.hist = append(m.hist, line)
	m.histIdx = len(m.hist)
	m.curBlk = 0 // a fresh prompt line closes any dangling thinking block
	m.lines = append(m.lines, tline{kind: linePrompt, raw: line}, tline{kind: lineStyled, raw: ""})
	// Design §5 consumption (#53): an unpinned prompt consults the routing
	// table — when its class has earned a confident recommendation for
	// another model, THIS run rides it (SubmitOn) and the transcript says
	// so, exactly like the CLI's ⇄ line. No evidence or same model →
	// config model via plain Submit.
	model, entry, swapped := routingSwap(m.cfg.Model, line)
	m.runModel = model
	// 进化重定义: a confident seat also switches the session's agent
	// identity — its specialist space follows the prompt.
	adoptAgent(m.sess, entry)
	var err error
	if swapped {
		err = m.sess.SubmitOn(line, model)
		if err == nil {
			m.pushStyled(paint(cCyan, fmt.Sprintf("⇄ routing [%s] → %s · agent %s (conf %.2f, n=%d)",
				entry.Class, model, entry.Agent, entry.Confidence, entry.Samples)))
		}
	} else {
		err = m.sess.Submit(line)
	}
	if err != nil {
		m.pushStyled(paint(cRed, tr("error: ", "错误: ")+err.Error()))
		m.status = idleStatus
		m.refresh()
		return nil
	}
	// Task hour market (#54): a fresh task's price — the mean wall time
	// of recent successful peers in its class, from this machine's own
	// ledger. No history, no line.
	if est, ok := market.Open().Estimate(line); ok {
		src := tr(fmt.Sprintf("last %d", est.Samples), fmt.Sprintf("近 %d 单", est.Samples))
		if est.Fallback {
			src = tr(fmt.Sprintf("all classes, last %d", est.Samples),
				fmt.Sprintf("全部类别均值，近 %d 单", est.Samples))
		}
		m.pushStyled(paint(cDim, fmt.Sprintf(tr("⏱ est ~%s · %s (%s)",
			"⏱ 工时预估 ~%s · %s（%s）"), fmtDur(est.DurMS), est.Class, src)))
	}
	m.busy = true
	// Live agent badge (#50): classify now so the status line names the
	// working agent from the first second of the run, not at score time.
	m.curAgent = string(routing.Classify(line))
	// The prompt opens a titled group in the steps panel: the parent row
	// that says what the following steps are for (steps.go).
	m.startPromptStep(line)
	m.status = ""
	m.follow = true
	m.refresh()
	return m.spin.Tick
}

// queueInjected lands a line the session delivered mid-run at a turn gap
// (#2): the display mirror loses it (the session already has it in
// history), and the transcript gains the prompt row so the injection point
// is visible. FIFO order on both sides means the first equal string is the
// one that arrived — duplicate lines stay in step.
func (m *tuiModel) queueInjected(text string) {
	for i, q := range m.queued {
		if q == text {
			m.queued = append(m.queued[:i], m.queued[i+1:]...)
			break
		}
	}
	m.hist = append(m.hist, text)
	m.histIdx = len(m.hist)
	m.lines = append(m.lines, tline{kind: linePrompt, raw: sanitizeText(text)},
		tline{kind: lineStyled, raw: ""})
	m.follow = true
	m.status = tr("injected into the running prompt", "已插入当前提问的间隙")
}

// kickQueued sends the next parked prompt — one per call, so every
// prompt_end fires exactly the next line and the chain paces itself (an
// esc-interrupt lands here too). A Submit error drops that line and tries
// the following one. Reports whether a prompt is now running, so Update
// can re-arm the spinner.
func (m *tuiModel) kickQueued() bool {
	for len(m.queued) > 0 && !m.busy && m.pending == "" {
		line := m.queued[0]
		m.queued = m.queued[1:]
		m.sendPrompt(line)
		if m.busy {
			return true
		}
	}
	return false
}

func (m *tuiModel) slash(line string) tea.Cmd {
	head, args, _ := strings.Cut(line, " ")
	m.cmdOut = nil // each command answers into a freshly cleared area
	switch head {
	case "/quit", "/exit", ":q":
		return tea.Quit
	case "/stats":
		var b strings.Builder
		printStats(&b, m.sess.Stats(), m.metrics, m.sess.Hooks(),
			newGauge(m.cfg, m.sess.Stats(), m.sess.SubUsage()))
		m.pushCmd(strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")...)
	case "/clear":
		m.lines = nil
		m.pend = ""
		m.curBlk = 0
		m.cmdOut = nil
	case "/sessions":
		m.showSessions() // read-only: safe even while a prompt runs
	case "/resume", "/new":
		if m.pending != "" {
			m.status = tr(m.pending+" already in progress", m.pending+" 已在进行中")
			return nil
		}
		idx := 0 // /resume only: 0 = bare (picker), N = explicit slot
		if head == "/resume" {
			if a := strings.TrimSpace(args); a != "" {
				var ok bool
				if idx, ok = parseResumeIndex(a); !ok {
					m.pushCmd(paint(cYellow, tr("usage: /resume [N] — /sessions lists saved sessions",
						"用法: /resume [N] — /sessions 列出已保存会话")))
					m.refresh()
					return nil
				}
			}
		}
		if m.busy {
			// Smart path: interrupt the live prompt and act the moment
			// prompt_end lands. pending fires even when Interrupt misses
			// the tiny pre-latch window — the prompt simply finishes
			// first, then the action runs.
			m.pending = head
			m.pendingIdx = idx
			m.sess.Interrupt()
			m.status = tr(head[1:]+" after the current prompt…",
				head[1:]+" 将在当前提问结束后执行…")
			return nil
		}
		switch {
		case head == "/new":
			m.doNew()
		case idx > 0:
			m.doResume(idx)
		default:
			m.resumeAction() // bare /resume — picker when several saved
		}
	case "/memory":
		m.showMemory(args)
	case "/remember":
		return m.rememberNow()
	case "/rollback":
		arg := strings.TrimSpace(args)
		// "/rollback save [label]" marks the current history as an undo
		// point — reach for it before a risky stretch of work.
		if arg == "save" || strings.HasPrefix(arg, "save ") {
			label := strings.TrimSpace(strings.TrimPrefix(arg, "save"))
			info, err := m.sess.Checkpoint(label)
			if err != nil {
				m.pushCmd(paint(cRed, "checkpoint: "+err.Error()))
			} else {
				m.pushCmd(fmt.Sprintf("%s %s",
					paint(cGreen, "· checkpoint #"+fmt.Sprint(info.ID)+" saved"),
					paint(cDim, fmt.Sprintf("%s · %d msgs", info.Label, info.Messages))))
			}
			m.refresh()
			return nil
		}
		idx := 0 // 0 = list the checkpoints
		if arg != "" {
			var ok bool
			if idx, ok = parseResumeIndex(arg); !ok {
				m.pushCmd(paint(cYellow, "usage: /rollback [N] — lists, restores slot N; /rollback save [text] marks a point"))
				m.refresh()
				return nil
			}
		}
		if m.busy {
			// Restore needs a settled prompt (histMu spans the whole
			// drive): interrupt now, act the moment prompt_end lands —
			// the same deferral /resume uses.
			m.pending = "/rollback"
			m.pendingIdx = idx
			m.sess.Interrupt()
			m.status = tr("rollback after the current prompt…", "当前提问结束后回退…")
			return nil
		}
		m.rollbackAction(idx)
	case "/model":
		m.showModel(args)
	case "/mode":
		m.pushCmd(modeAction(m.sess, args))
		m.status = modeToast(m.sess.Mode())
	case "/think":
		m.toggleThinking()
	case "/route":
		m.showRoute(args)
	case "/exam":
		m.examCommand(args)
	case "/contest":
		m.contestCommand(args)
	case "/agents":
		m.agentsCommand(args)
	case "/market":
		m.marketCommand(args)
	case "/flow":
		return m.flowCommand(args)
	case "/timer":
		m.timerCommand(args)
	case "/steps":
		// Bare toggles; open/close/width N mirror the alt+ keys.
		switch sub, rest, _ := strings.Cut(strings.TrimSpace(args), " "); sub {
		case "":
			m.stepsOn = !m.stepsOn
			m.relayout()
			state := tr("on", "开")
			if !m.stepsOn {
				state = tr("off", "关")
			} else if !m.panelOn() {
				state = tr("on (terminal too narrow — needs 110+ columns)",
					"开（终端太窄 — 需要 110+ 列）")
			}
			m.status = tr("steps panel ", "步骤面板 ") + state
		case "open":
			m.stepsOn = true
			m.relayout()
			m.stepToggleOpen()
			if m.stepSel > 0 {
				m.status = tr(fmt.Sprintf("steps panel on — expanded #%d", m.stepSel),
					fmt.Sprintf("步骤面板开 — 已展开 #%d", m.stepSel))
			} else {
				m.status = tr("steps panel on", "步骤面板开")
			}
		case "close":
			m.stepOpen = false
			m.status = tr("steps collapsed", "步骤面板已收起")
		case "width":
			n, err := strconv.Atoi(strings.TrimSpace(rest))
			if err != nil || n < panelWMin || n > panelWMax {
				m.status = tr(fmt.Sprintf("usage: /steps width %d..%d", panelWMin, panelWMax),
					fmt.Sprintf("用法: /steps width %d..%d", panelWMin, panelWMax))
				return nil
			}
			m.panelW = n
			m.relayout()
			m.status = tr(fmt.Sprintf("panel width %d", n), fmt.Sprintf("面板宽度 %d", n))
		default:
			m.status = tr("usage: /steps [open|close|width N]", "用法: /steps [open|close|width N]")
		}
		m.refresh()
		return nil
	case "/evolve":
		// The keyless twin of alt+e — flips the right column to the
		// evolution board (per-class leaderboard + exam clock).
		m.evoOn = !m.evoOn
		m.relayout()
		if m.evoOn {
			if !m.panelOn() {
				m.status = tr("evolution panel on (terminal too narrow — needs 110+ columns)",
					"进化面板开（终端太窄 — 需要 110+ 列）")
			} else {
				m.status = tr("evolution panel on", "进化面板开")
			}
		} else {
			m.status = tr("evolution panel off", "进化面板关")
		}
		m.refresh()
		return nil
	case "/paste":
		// The keyless twin of alt+v — discoverable in the popup, and the
		// only way to paste in the plain REPL (main.go has its own case).
		if m.pasting {
			return nil
		}
		m.pasting = true
		m.status = tr("pasting image…", "正在粘贴图片…")
		return pasteCmd(m.workspace)
	case "/lang":
		if a := strings.TrimSpace(args); a == "" {
			m.status = langLine()
		} else if setLang(a) {
			m.status = langLine()
		} else {
			m.status = tr("usage: /lang zh|en", "用法: /lang zh|en")
		}
	case "/help":
		m.pushCmd(helpLines()...)
	default:
		m.pushCmd(paint(cRed, tr("unknown command: "+head, "未知命令: "+head)),
			paint(cDim, tr("try /help", "试试 /help")))
	}
	m.refresh()
	return nil
}

// helpLines is the /help body: the popup's descriptions in prose plus the
// key maps. Split by language so /lang covers it wholesale.
func helpLines() []string {
	if uiLang == "zh" {
		return []string{
			paint(cBold, "命令"),
			"  /stats   用量表：token、缓存命中、工具耗时",
			"  /clear   清空转录视图（会话历史保留）",
			"  /resume  恢复已保存会话 — 裸 /resume 打开选择器（/resume N 直达槽位 N）",
			"  /sessions 列出本工作区的已保存会话",
			"  /new     新会话：当前对话先归档",
			"  /memory  分层记忆：团队/个人/项目行，/memory N 切换热词，sync 同步",
			"  /remember 立即提炼本会话的收获进记忆（退出时也会自动跑）",
			"  /rollback 回退历史：裸命令列检查点，/rollback N 恢复，/rollback save [text] 打标记",
			"  /model   模型卡：/model current · /model list · /model <id>",
			"  /mode    执行模式：plan（只读）· bypass（自动批准）· default — shift+tab 循环切换",
			"  /think   展开/折叠思考块",
			"  /paste   把剪贴板图片附到下一条提问（等同 alt+v）",
			"  /steps   右侧操作审计面板：裸命令开关，/steps open|close|width N",
			"  /evolve  进化面板：分类榜单、考试时钟、空缺席位（等同 alt+e）",
			"  /route   任务分类器：裸命令显示证据表 + 模型画像，/route <text> 给任务路由",
			"  /exam    闭卷考试：裸命令按上一条提问分类，/exam <class> — 每 10 轮自动考（LYON_EXAM=off）",
			"  /contest 竞争上岗：全部候选同考 held-out 任务，赢家上岗并接管路由（/contest <class>）",
			"  /agents  候选名单：裸命令列出 + 占位，add <id> <model> [effort] 新增，rm <id> 删除",
			"  /market  任务工时市场：每单完成自动记账，新任务到达时按同类近期均值预估",
			"  /flow    工作流：list · show <slug> · match <text> · export <slug> · save <slug>（会冻结本会话）",
			"  /timer   定时器：裸命令列出（下次触发/状态/次数）· add <id> <once|every|cron> <规格> <command|prompt> <内容> · enable/disable/rm <id>",
			"  /lang    界面语言：zh | en",
			"  /help    本帮助",
			"  /quit    退出",
			paint(cDim, "输入 / 弹出命令列表 — ↑/↓ 选择，enter 执行 · tab 补全 · esc 关闭"),
			paint(cDim, "快捷键：pgup/pgdn 或滚轮滚动 · ↑/↓ 提问历史 · ctrl+t 思考折叠 · ctrl+o 展开全部折叠 · shift+tab 模式 · ctrl+c 中断 · esc 关闭 output/弹层 · alt+v 粘贴图片"),
			paint(cDim, "steps 面板：shift+alt+↑↓ 选步骤 · alt+enter 展开 · alt+o 实时输出（展开/折叠）· alt+←→ 打开/调宽（IDE 吞 alt 时用 shift+alt+方向键，或 ctrl+shift+←/→）"),
			paint(cDim, "进化面板：alt+e（或 /evolve）— 榜单、考试时钟 · 面板开着时评分/考试卡直接进面板"),
			paint(cDim, "批准卡片（shell/write/edit，默认开启）：y 一次 · a 本会话内放行该工具 · n 拒绝 · ↑↓/tab 选卡片 · enter 确认"),
			paint(cDim, "附件：输入 @ 弹出工作区文件列表选择插入；@img.png 随提问发送图片（空格路径加引号）"),
		}
	}
	return []string{
		paint(cBold, "commands"),
		"  /stats   usage table: tokens, cache hits, tool latencies",
		"  /clear   clear the transcript view (session history kept)",
		"  /resume  restore a saved session — bare /resume opens a picker (/resume N jumps to slot N)",
		"  /sessions list this workspace's saved sessions",
		"  /new     fresh session: current conversation archived first",
		"  /memory  layered memory: team/personal/project rows, /memory N toggles hot, sync",
		"  /remember distill this session's learnings into memory now (auto-runs on exit)",
		"  /rollback undo history: bare lists checkpoints, /rollback N restores, /rollback save [text] marks one",
		"  /model   model card: /model current · /model list · /model <id>",
		"  /mode    execution mode: plan (read-only) · bypass (auto-approve) · default — shift+tab cycles",
		"  /think   expand/collapse thinking blocks",
		"  /paste   attach the clipboard image to your next prompt (same as alt+v)",
		"  /steps   right-hand action audit panel: bare toggles, /steps open|close|width N",
		"  /evolve  evolution panel: per-class leaderboard, exam clock, open seats (same as alt+e)",
		"  /route   task classifier: bare shows the evidence table + model profile, /route <text> routes a task",
		"  /exam    closed-book exam: bare classifies from last prompt, /exam <class> — auto every 10 rounds (LYON_EXAM=off)",
		"  /contest seat competition: the whole candidate field vs held-out tasks — winner takes the seat and the routing (/contest <class>)",
		"  /agents  candidate field: bare lists entrants + seats, add <id> <model> [effort], rm <id>",
		"  /market  task hour market: every done prompt files its wall time; new tasks get a class-priced estimate",
		"  /flow    workflows: list · show <slug> · match <text> · export <slug> · save <slug> (freezes this session)",
		"  /timer   timers: bare lists next fire/status/runs · add <id> <once|every|cron> <spec> <command|prompt> <text> · enable/disable/rm <id>",
		"  /lang    interface language: zh | en",
		"  /help    this help",
		"  /quit    exit",
		paint(cDim, "typing / pops the command list — ↑/↓ choose, enter runs · tab completes · esc closes"),
		paint(cDim, "keys: pgup/pgdn or wheel scroll · ↑/↓ prompt history · ctrl+t thinking fold · ctrl+o expand all folds · shift+tab mode · ctrl+c interrupt · esc close output/overlays · alt+v paste image"),
		paint(cDim, "steps panel: shift+alt+↑↓ select · alt+enter expand · alt+o live output (expand/collapse) · alt+←→ opens/widens (if the IDE swallows alt, use shift+alt+arrows or ctrl+shift+←/→)"),
		paint(cDim, "evolution panel: alt+e (or /evolve) — leaderboard, exam clock · score/exam cards feed the panel while it is open"),
		paint(cDim, "approve card (shell/write/edit, on by default): y once · a allow this tool all session · n deny · ↑↓/tab choose · enter"),
		paint(cDim, "attach: typing @ pops the workspace file list to pick from; @img.png sends an image (quote spaces)"),
	}
}

// showMemory handles /memory and its subcommands: bare renders the
// numbered table, N toggles that row's hot tier, forget N deletes it,
// sync talks to the configured cloud (async — the UI stays live), reload
// re-reads files another agent may have edited.
func (m *tuiModel) showMemory(args string) {
	args = strings.TrimSpace(args)
	render := func(extra ...string) {
		lines, rows := memoryTable(m.workspace)
		m.memRows = rows
		m.pushCmd(append(extra, lines...)...)
	}
	switch {
	case args == "":
		render()
	case args == "sync":
		ws := m.workspace
		m.status = tr("memory sync…", "正在同步记忆…")
		step := m.pushAsyncStep(stepMemory, "memory.sync", "pushing/pulling cloud…")
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			pushed, pulled, err := syncMemory(ctx, ws)
			if p := tuiProg.Load(); p != nil {
				p.Send(memSyncDone{pushed: pushed, pulled: pulled, err: err, step: step})
			}
		}()
	case args == "reload":
		memory.Open(m.workspace).Reload()
		render(paint(cDim, "memory reloaded from disk"))
	case strings.HasPrefix(args, "forget"):
		idx, ok := parseRowArg(strings.TrimPrefix(args, "forget"))
		if !ok || idx > len(m.memRows) {
			m.status = tr("usage: /memory forget N — N comes from the table",
				"用法: /memory forget N — N 来自表格编号")
			return
		}
		f := m.memRows[idx-1]
		if err := memory.Open(m.workspace).Delete(f.Scope, f.Name); err != nil {
			m.status = tr("forget: ", "删除失败: ") + err.Error()
			return
		}
		render(paint(cDim, fmt.Sprintf("forgot (%s) %s", f.Scope, f.Name)))
	default:
		idx, ok := parseRowArg(args)
		if !ok || idx > len(m.memRows) {
			m.status = tr("usage: /memory [N | forget N | sync | reload]",
				"用法: /memory [N | forget N | sync | reload]")
			return
		}
		f := m.memRows[idx-1]
		hot, err := memory.Open(m.workspace).ToggleHot(f.Scope, f.Name)
		if err != nil {
			m.status = tr("hot: ", "热词切换失败: ") + err.Error()
			return
		}
		state := "cold"
		if hot {
			state = "hot"
		}
		render(paint(cDim, fmt.Sprintf("(%s) %s is now %s", f.Scope, f.Name, state)))
	}
	m.refresh()
}

// kickTitleCheck refreshes the session title asynchronously after a
// prompt: one small-model intent check, bounded, at most one in flight.
// The result shows up via headerLine() on the redraw titleUpdated triggers.
func (m *tuiModel) kickTitleCheck() {
	if m.titleBusy {
		m.titlePending = true // re-kick when the in-flight check lands
		return
	}
	m.titleBusy = true
	sess := m.sess
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _, _ = sess.RefreshTitle(ctx)
		if p := tuiProg.Load(); p != nil {
			p.Send(titleUpdated{})
		}
	}()
}

// titleUpdated nudges a redraw after an async title refresh lands.
type titleUpdated struct{}

// diagDone carries an async failure post-mortem back into the loop.
type diagDone struct {
	step int
	diag eval.Diagnosis
	err  error
}

// kickDiagnose post-mortems a failed prompt (design §1.3): one cheap
// subagent-tier call turning the trajectory into checkable conditions and
// a patch a retry would inherit. The panel's stepDiagnose row opens here
// and closes on diagDone; LYON_DIAGNOSE=off disables (mirrors LYON_TITLE).
//
// The post-mortem must COMPLETE: a deadline or a poisoned payload used to
// end the step as a bare error badge. evolve.DiagnoseWithFallback runs the
// bounded retry loop then the local fallback — diagDone always carries at
// least one condition.
func (m *tuiModel) kickDiagnose(fail string) {
	if strings.EqualFold(os.Getenv("LYON_DIAGNOSE"), "off") {
		return
	}
	prompt := ""
	if len(m.hist) > 0 {
		prompt = m.hist[len(m.hist)-1]
	}
	traj := m.diagTrajectory(prompt, fail)
	// Snapshot the step tail for the fallback post-mortem: this goroutine
	// reads it away from the model's own loop. The failed steps become
	// the local conditions evolve.DiagnoseWithFallback cites when the
	// diagnoser model is unavailable.
	var tail []evolve.LocalCond
	{
		t := m.steps
		if len(t) > 14 {
			t = t[len(t)-14:]
		}
		for _, s := range t {
			if s.status != stepError {
				continue
			}
			msg := s.err
			if msg == "" {
				msg = s.detail
			}
			tail = append(tail, evolve.LocalCond{
				Name: fmt.Sprintf("step #%d %s", s.n, s.name), Err: msg})
		}
	}
	step := m.pushAsyncStep(stepDiagnose, "diagnose", "post-mortem of the failed run…")
	sess := m.sess
	go func() {
		d := evolve.DiagnoseWithFallback(sess, traj, fail, tail)
		// Always a success payload: the step closes with conditions, never
		// a bare deadline/error badge (错因归纳 runs to completion).
		if p := tuiProg.Load(); p != nil {
			p.Send(diagDone{step: step, diag: d})
		}
	}()
}

// diagTrajectory builds the failed run's evidence for the diagnoser: the
// prompt, the error, and the recent step tail — the panel's own audit
// doubles as the post-mortem's input.
func (m *tuiModel) diagTrajectory(prompt, fail string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Prompt:\n%s\n\nError:\n%s\n\nRecent steps:\n", prompt, fail)
	tail := m.steps
	if len(tail) > 14 {
		tail = tail[len(tail)-14:]
	}
	stat := [...]string{"running", "ok", "failed"} // stepRunning/stepDone/stepError
	for _, s := range tail {
		st := "ok"
		if int(s.status) < len(stat) {
			st = stat[s.status]
		}
		if s.agent != "" {
			fmt.Fprintf(&b, "#%d %s@%s [%s] %s\n", s.n, s.name, s.agent, st, s.detail)
		} else {
			fmt.Fprintf(&b, "#%d %s [%s] %s\n", s.n, s.name, st, s.detail)
		}
		if s.status == stepError && s.err != "" {
			fmt.Fprintf(&b, "    error: %s\n", s.err)
		}
	}
	return b.String()
}

// rememberNow distills the session's learnings into memory on a
// goroutine: the model call takes seconds and the TUI must stay live.
// histMu waits out any in-flight drive; the result arrives as rememberDone.
func (m *tuiModel) rememberNow() tea.Cmd {
	if m.busy {
		m.status = tr("busy — /remember runs after the current prompt",
			"忙 — /remember 将在当前提问结束后执行")
		return nil
	}
	m.status = tr("distilling learnings into memory…", "正在把收获提炼进记忆…")
	step := m.pushAsyncStep(stepMemory, "memory.distill", "extracting durable facts…")
	sess := m.sess
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		rep, err := sess.DistillWithReport(ctx)
		if p := tuiProg.Load(); p != nil {
			p.Send(rememberDone{n: rep.Saved, rep: rep, err: err, step: step})
		}
	}()
	return nil
}

// runTUI drives the bubbletea program until the user quits. The event pump
// forwards engine events into the loop; Close semantics stay with cmdChat.
// snap seeds the transcript when the session was restored with `chat -c`.
func runTUI(cfg agent.Config, sess *agent.Session, metrics *tools.Metrics, snap *agent.Snapshot) int {
	m := newTUIModel(cfg, sess, metrics)
	m.replay(snap)
	m.applySteps(snapSteps(snap)) // startup restore: the saved audit panel
	if snap == nil {              // fresh session — surface the store for /resume
		if hint := resumeHint(cfg.Workspace, true); hint != "" { // picker wording
			m.pushStyled(paint(cDim, hint), "")
			m.refresh()
		}
	}
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	tuiProg.Store(p)
	defer tuiProg.Store(nil)
	dashStartRefreshers(m.workspace) // one goroutine per module — never the event loop

	// The pump keeps draining until the event channel closes — even after
	// the program quits, so a prompt finishing during exit never blocks on
	// a full buffer while saveChatSession waits for Export.
	go func() {
		for ev := range sess.Events() {
			p.Send(tuiEv{ev})
		}
	}()

	final, err := p.Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "tui:", err)
		return 1
	}
	// The exit path saves the conversation (saveChatSession → Export);
	// hand it the panel too so the next run resumes the audit trail, and
	// the step plan with it (#66).
	if tm, ok := final.(*tuiModel); ok {
		sess.SetUIState(stepsState(tm.steps))
		sess.SetTodos(todosState(tm.todos))
	}
	return 0
}

// snapSteps pulls the saved panel out of a possibly-nil snapshot.
func snapSteps(snap *agent.Snapshot) json.RawMessage {
	if snap == nil {
		return nil
	}
	return snap.Steps
}
