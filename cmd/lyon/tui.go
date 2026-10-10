package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	isatty "github.com/mattn/go-isatty"

	"lyon-agent/pkg/agent"
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
}

// tuiModel is the full-screen chat: scrollable transcript viewport on top,
// status line, single-line input at the bottom.
type tuiModel struct {
	sess      *agent.Session
	metrics   *tools.Metrics
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

	// Slash popup: sugs holds the commands the current input matches,
	// sugIdx the highlighted row, sugHide set by esc until typing resumes.
	sugs    []sugItem
	sugIdx  int
	sugHide bool

	// Session picker, opened by a bare /resume when the store holds several
	// sessions: pick lists them newest-first, pickIdx the highlighted slot.
	// It renders through the same bottom popup as the slash list and owns
	// the keyboard until enter (restore) or esc (cancel) closes it.
	pick    []agent.SnapshotRef
	pickIdx int

	busy      bool
	armedQuit bool
	// pending is a slash action deferred until prompt_end lands ("/resume"
	// or "/new"): it interrupted an in-flight prompt, and the restore/reset
	// must never race a live drive holding histMu. pendingIdx carries the
	// 1-based slot a deferred /resume should restore; 0 = bare /resume
	// (the picker / single-session path runs at dispatch time).
	pending    string
	pendingIdx int
	status     string
	appr       *approveReq

	hist    []string
	histIdx int // len(hist) = not recalling; up decrements, down increments
}

func newTUIModel(cfg agent.Config, sess *agent.Session, metrics *tools.Metrics) *tuiModel {
	ti := textinput.New()
	ti.Prompt = "❯ "
	ti.PromptStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	ti.Placeholder = "ask anything — /help for commands"
	ti.PlaceholderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	ti.CharLimit = 8192
	ti.Focus()

	sp := spinner.New(spinner.WithSpinner(spinner.Dot))
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("212"))

	m := &tuiModel{
		sess:      sess,
		metrics:   metrics,
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
	}
	m.resetTranscript()
	m.updateSuggestions()
	m.refresh()
	return m
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
}

func (m *tuiModel) Init() tea.Cmd { return m.ti.Focus() }

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
	for _, msg := range snap.Messages {
		for _, b := range msg.Content {
			switch {
			case b.OfText != nil:
				text := sanitizeText(b.OfText.Text)
				switch {
				case strings.HasPrefix(text, "[persistent-memory]"):
					m.pushStyled(paint(cDim, "⌘ memory restored from a previous session"))
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
				// Restored thinking needs a block id to fold by: pushText
				// tags lines with curBlk, which is 0 throughout replay, and
				// blk==0 means "ungrouped" — renderTranscript only folds
				// blk>0 runs. Without a fresh id here, /resume leaves
				// thinking permanently expanded and ctrl+t does nothing.
				m.thinkSeq++
				m.curBlk = m.thinkSeq
				m.pushText(lineThink, b.OfThinking.Thinking)
				m.curBlk = 0

			case b.OfToolUse != nil:
				in, _ := json.Marshal(b.OfToolUse.Input)
				m.pushStyled(fmt.Sprintf("%s %s %s", paint(cCyan, "⚒ tool_use"),
					b.OfToolUse.Name, paint(cDim, shortJSON(in, 160))))

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
				m.pushStyled(paint(cDim, "🖼 image"))
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

// applyEvent translates one engine event into transcript lines. Mirrors
// render()'s plain-mode styling, minus the immediate stdout writes.
func (m *tuiModel) applyEvent(ev agent.Event) {
	switch ev.Type {
	case agent.EventThinkingDelta:
		m.appendDelta(lineThink, ev.Thinking)

	case agent.EventTextDelta:
		m.appendDelta(lineText, ev.Text)

	case agent.EventToolUseStart:
		m.flushPend()
		if ev.Server {
			m.pushStyled(fmt.Sprintf("%s %s %s", paint(cMagenta, "🌐 server"), ev.ToolName,
				paint(cDim, shortJSON(ev.Input, 160))))
		} else {
			m.pushStyled(fmt.Sprintf("%s %s %s", paint(cCyan, "⚒ tool_use"), ev.ToolName,
				paint(cDim, ev.ToolID)))
		}

	case agent.EventServerToolResult:
		m.flushPend()
		marker := paint(cMagenta, "⤷ results")
		if ev.IsError {
			marker = paint(cRed, "⤷ error")
		}
		m.pushStyled(fmt.Sprintf("%s %s %s", marker, ev.ToolName, paint(cDim, shortJSON(ev.Input, 240))))

	case agent.EventToolExecStart:
		m.flushPend()
		m.pushStyled(fmt.Sprintf("%s #%d %s %s", paint(cCyan, "▸ exec"), ev.ExecID, ev.ToolName,
			paint(cDim, shortJSON(ev.Input, 160))))

	case agent.EventToolExecEnd:
		m.flushPend()
		if ev.IsError {
			m.pushStyled(fmt.Sprintf("%s #%d %s %s %s", paint(cRed, "✘ done"), ev.ExecID, ev.ToolName,
				paint(cDim, fmt.Sprintf("%dms", ev.DurationMS)), paint(cRed, sanitizeText(ev.Err))))
		} else {
			m.pushStyled(fmt.Sprintf("%s #%d %s %s", paint(cGreen, "✔ done"), ev.ExecID, ev.ToolName,
				paint(cDim, fmt.Sprintf("%dms", ev.DurationMS))))
		}

	case agent.EventTurnEnd:
		m.flushPend()
		u := ev.Usage
		if u == nil {
			u = &agent.Usage{}
		}
		m.pushStyled("",
			fmt.Sprintf("%s in=%d out=%d cache_read=%d cache_create=%d stop=%s %s",
				paint(cYellow, fmt.Sprintf("[turn %d]", ev.Turn)),
				u.Input, u.Output, u.CacheRead, u.CacheCreation,
				ev.StopReason, paint(cDim, fmt.Sprintf("%dms", ev.DurationMS))))

	case agent.EventCompaction:
		m.flushPend()
		line := sanitizeText(ev.Detail)
		if ev.Usage != nil {
			line = fmt.Sprintf("%s (in=%d out=%d)", ev.Detail, ev.Usage.Input, ev.Usage.Output)
		}
		m.pushStyled("", fmt.Sprintf("%s %s", paint(cYellow, "📦 compaction"), paint(cDim, line)))

	case agent.EventSubagentEnd:
		m.flushPend()
		u := ev.Usage
		if u == nil {
			u = &agent.Usage{}
		}
		m.pushStyled(fmt.Sprintf("%s %s in=%d out=%d cache_read=%d %s %s",
			paint(cCyan, "🧠 subagent"), ev.ToolName,
			u.Input, u.Output, u.CacheRead, sanitizeText(ev.Detail),
			paint(cDim, fmt.Sprintf("%dms", ev.DurationMS))))

	case agent.EventError:
		m.flushPend()
		m.pushStyled("", paint(cRed, "error: "+sanitizeText(ev.Err)))

	case agent.EventPromptEnd:
		m.flushPend()
		m.busy = false
		m.armedQuit = false
		// Surface completion as a system toast — long runs are watched by
		// notification, not by staring at the transcript.
		switch {
		case ev.Err == agent.ErrInterrupted.Error():
			notifyDone("lyon: interrupted", "the run was stopped — history repaired")
		case ev.Err != "":
			notifyDone("lyon: run failed", truncNotif(ev.Err, 160))
		default:
			notifyDone("lyon: run finished",
				fmt.Sprintf("%d API calls complete — press enter for the next prompt", ev.Iterations))
		}
		if ev.Err == agent.ErrInterrupted.Error() {
			m.pushStyled(paint(cYellow, "⏹ interrupted"), paint(cDim, "history repaired — keep chatting"))
			m.status = idleStatus
		} else if ev.Err != "" {
			m.pushStyled(paint(cRed, "✗ prompt failed"), paint(cDim, sanitizeText(ev.Err)))
			m.status = idleStatus
		} else {
			m.pushStyled(paint(cGreen, "✓ done"), paint(cDim, fmt.Sprintf("%d API calls", ev.Iterations)))
			m.status = idleStatus
		}
		if m.pending != "" { // deferred /resume or /new: prompt has ended
			act, idx := m.pending, m.pendingIdx
			m.pending, m.pendingIdx = "", 0
			switch {
			case act == "/new":
				m.doNew()
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
		m.pushStyled(paint(cRed, "resume: "+err.Error()))
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
		m.pushStyled(paint(cYellow, "no saved session for this workspace"))
	case idx < 1 || idx > len(refs):
		m.pushStyled(paint(cYellow, fmt.Sprintf("no session #%d — /sessions lists %d", idx, len(refs))))
	default:
		loaded, err := agent.LoadSnapshotFrom(refs[idx-1].Path)
		switch {
		case err != nil:
			m.pushStyled(paint(cRed, "resume: "+err.Error()))
		case loaded == nil:
			m.pushStyled(paint(cYellow, "that session is empty"))
		default:
			m.sess.Restore(*loaded)
			m.pend = ""
			m.curBlk = 0
			m.follow = true
			m.resetTranscript()
			m.replay(loaded)
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
		m.pushStyled(paint(cRed, "resume: "+err.Error()))
		m.refresh()
	case len(refs) == 0:
		m.pushStyled(paint(cYellow, "no saved session for this workspace"))
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
		m.vp, _ = m.vp.Update(msg)
		m.follow = m.vp.AtBottom()
	}
}

// showSessions lists the workspace's session store so /resume N has a
// target. Read-only — runs even while a prompt is in flight.
func (m *tuiModel) showSessions() {
	refs, err := agent.ListSnapshots(m.workspace)
	m.flushPend()
	switch {
	case err != nil:
		m.pushStyled(paint(cRed, "sessions: "+err.Error()))
	case len(refs) == 0:
		m.pushStyled(paint(cYellow, "no saved sessions for this workspace"))
	default:
		m.pushStyled(paint(cBold, fmt.Sprintf("saved sessions (%d, newest first)", len(refs))))
		for i, r := range refs {
			m.pushStyled(fmt.Sprintf("  %2d  %s  %d messages %s", i+1,
				r.SavedAt.Format("2006-01-02 15:04"), r.Messages,
				paint(cDim, fmt.Sprintf("— /resume %d", i+1))))
		}
		if len(refs) > 1 {
			m.pushStyled(paint(cDim, "/resume opens the picker — ↑/↓ choose"))
		} else {
			m.pushStyled(paint(cDim, "/resume alone restores the newest"))
		}
	}
	m.refresh()
}

// doNew wipes the conversation — engine state and transcript — back to a
// fresh session. The live conversation is archived first (unless it
// duplicates the store), so nothing resumable dies with /new; later saves
// rotate rather than overwrite, which is why browsing old sessions needs
// /sessions + /resume N instead of a single slot.
func (m *tuiModel) doNew() {
	archived, err := m.sess.ArchiveIfNew()
	m.sess.Reset()
	m.pend = ""
	m.curBlk = 0
	m.follow = true
	m.resetTranscript()
	notice := "new session — history cleared"
	switch {
	case err != nil:
		m.pushStyled(paint(cRed, "archive: "+err.Error()))
	case archived:
		notice += ", previous conversation archived (/sessions to browse)"
	}
	m.pushStyled(paint(cDim, notice), "")
	m.status = idleStatus
	m.refresh()
}

const idleStatus = "enter send · esc interrupt · pgup/pgdn scroll · ctrl+t think · /help"

// sugItem is one row of the slash-command popup.
type sugItem struct{ cmd, desc string }

// slashCommands is the popup's source of truth — keep it in sync with
// slash(): every entry must be a command that slash() actually handles.
var slashCommands = []sugItem{
	{"/stats", "usage table — tokens, cache hits, tool latencies"},
	{"/clear", "clear the transcript view (history kept)"},
	{"/resume", "restore a saved session — bare /resume opens a picker"},
	{"/sessions", "list this workspace's saved sessions"},
	{"/new", "start a fresh session (current one archived)"},
	{"/think", "expand/collapse thinking blocks"},
	{"/help", "list commands and keys"},
	{"/quit", "exit"},
}

// refresh rebuilds the viewport content from the transcript.
func (m *tuiModel) refresh() {
	m.vp.SetContent(m.renderTranscript())
	if m.follow {
		m.vp.GotoBottom()
	}
}

// suggestions matches the current input against the command list. The popup
// shows while the input is one bare slash token ("/stats", "/th", …); a
// space ends it (no command takes arguments) and esc hides it until typing.
func (m *tuiModel) suggestions() []sugItem {
	if m.sugHide || m.appr != nil {
		return nil
	}
	v := m.ti.Value()
	if !strings.HasPrefix(v, "/") || strings.ContainsAny(v, " \t") {
		return nil
	}
	out := make([]sugItem, 0, len(slashCommands))
	for _, c := range slashCommands {
		if strings.HasPrefix(c.cmd, v) {
			out = append(out, c)
		}
	}
	return out
}

// updateSuggestions refreshes the popup cache and shrinks the viewport by
// exactly the rows the popup will draw, so nothing is pushed off-screen.
// Call it after every change to the input.
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
	vh := m.height - 3 - len(m.suggestionRows()) // header + status + input
	if vh < 1 {
		vh = 1
	}
	m.vp.Height = vh
	if m.follow {
		m.vp.GotoBottom()
	}
}

// suggestionRows renders the popup rows: matches (selected one highlighted),
// then a key hint while more than one command matches. While the session
// picker is open it owns the same space instead.
func (m *tuiModel) suggestionRows() []string {
	if m.appr != nil { // approval overlay owns the bottom
		return nil
	}
	if len(m.pick) > 0 {
		return m.pickRows()
	}
	if len(m.sugs) == 0 {
		return nil
	}
	rows := make([]string, 0, len(m.sugs)+1)
	for i, s := range m.sugs {
		if i == m.sugIdx {
			rows = append(rows, paint(cCyan, "❯ "+s.cmd)+paint(cDim, "  "+s.desc))
		} else {
			rows = append(rows, paint(cCyan, "  "+s.cmd)+paint(cDim, "  "+s.desc))
		}
	}
	if len(m.sugs) > 1 {
		rows = append(rows, paint(cDim, "  tab complete · ↑/↓ choose · esc close"))
	}
	return rows
}

// pickRows renders the session picker: a header, one row per saved session
// (newest first, the highlighted one marked), and the key hint.
func (m *tuiModel) pickRows() []string {
	rows := make([]string, 0, len(m.pick)+2)
	rows = append(rows, paint(cBold, fmt.Sprintf("resume — saved sessions (%d, newest first)", len(m.pick))))
	for i, r := range m.pick {
		head := fmt.Sprintf("%2d  %s  %d messages", i+1,
			r.SavedAt.Format("2006-01-02 15:04"), r.Messages)
		if i == m.pickIdx {
			rows = append(rows, paint(cCyan, "❯ "+head))
		} else {
			rows = append(rows, "  "+head)
		}
	}
	rows = append(rows, paint(cDim, "  ↑/↓ choose · enter restore · esc close"))
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
	w := m.width
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
		if l.kind == lineThink && l.blk > 0 {
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
	if m.pend != "" {
		// The partial line is styled but must not toggle fence state — it
		// will, exactly once, when it commits as a full line. A pending
		// thinking line belongs to the block still streaming, so it always
		// renders: the live stream is the progress indicator. A pending
		// fence delimiter renders nothing — the committed line will toggle
		// and vanish, so drawing the partial first would only flash it.
		if m.pendKind != lineText || !strings.HasPrefix(strings.TrimSpace(m.pend), "```") {
			emit(m.stylePending(inFence))
		}
	}
	return b.String()
}

// thinkBlockOpen reports whether a block shows its body: the global toggle,
// or the run still streaming (closed by the next non-thinking line).
func (m *tuiModel) thinkBlockOpen(group []tline) bool {
	return m.thinkOpen || group[0].blk == m.curBlk
}

// thinkHeader is the fold marker — "▸ thinking · N lines" plus a one-line
// preview while folded (▸ folded, ▾ open).
func (m *tuiModel) thinkHeader(group []tline, open bool) string {
	arrow := "▸"
	if open {
		arrow = "▾"
	}
	unit := "lines"
	if len(group) == 1 {
		unit = "line"
	}
	s := paint(cDim+cMagenta, arrow+" thinking") +
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
		m.status = "thinking expanded — ctrl+t folds again"
	} else {
		m.status = "thinking folded — ctrl+t expands"
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
func sanitizeText(s string) string {
	if !strings.ContainsAny(s, "\x1b\r\t\x7f") && !hasCtl(s) {
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
	return b.String()
}

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
	rows := m.suggestionRows()
	vh := m.height - 3 - len(rows) // header + status + input
	if vh < 1 {
		vh = 1
	}
	m.vp.Height = vh
	m.vp.SetYOffset(m.vp.YOffset) // re-clamp: Height may have just shrunk
	var b strings.Builder
	b.WriteString(m.clip(m.header))
	b.WriteByte('\n')
	b.WriteString(m.vp.View())
	b.WriteByte('\n')
	b.WriteString(m.clip(m.statusLine()))
	b.WriteByte('\n')
	for _, r := range rows {
		b.WriteString(m.clip(r))
		b.WriteByte('\n')
	}
	b.WriteString(m.clip(m.inputLine()))
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
		return paint(cYellow, fmt.Sprintf("approve? %s %s  [y=yes n=no]",
			m.appr.ev.Name, shortJSON(m.appr.ev.Input, 120)))
	}
	if m.busy {
		s := m.spin.View() + " working…"
		if m.status != "" && m.status != idleStatus {
			s += paint(cDim, "  "+m.status)
		} else {
			s += paint(cDim, "  esc interrupt")
		}
		return s
	}
	return paint(cDim, m.status)
}

func (m *tuiModel) inputLine() string {
	if m.appr != nil {
		return paint(cGreen, "y") + paint(cDim, "=approve  ") +
			paint(cRed, "n") + paint(cDim, "=deny")
	}
	return m.ti.View()
}

func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.vp.Width = msg.Width
		// prompt (2 cells) + value field + cursor (1 cell) must fit the
		// row exactly; -2 left the cursor one cell past the right edge.
		m.ti.Width = msg.Width - 3
		m.updateSuggestions() // sets vp.Height: header + status + input + popup
		m.refresh()

	case tuiEv:
		m.applyEvent(msg.ev)
		m.refresh()

	case spinner.TickMsg:
		if !m.busy {
			break
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		cmds = append(cmds, cmd)

	case approveReq:
		// First-come overlay; apprMu upstream guarantees no overlap.
		m.appr = &msg
		// The user is probably not watching the TUI — raise a system box.
		notifyDecision("lyon: approval needed",
			truncNotif(msg.ev.Name+" "+string(msg.ev.Input), 200))

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
				if m.sess.Interrupt() {
					m.status = "interrupting… (ctrl+c again quits)"
				}
				m.armedQuit = true
				return m, nil
			}
			return m, tea.Quit

		case "esc":
			if m.busy && m.sess.Interrupt() {
				m.status = "interrupting…"
				return m, nil
			}
			if len(m.sugs) > 0 { // idle: close the popup first
				m.sugHide = true
				m.updateSuggestions()
			}
			return m, nil

		case "tab":
			m.armedQuit = false
			if len(m.sugs) > 0 && m.sugIdx < len(m.sugs) {
				m.ti.SetValue(m.sugs[m.sugIdx].cmd)
				m.ti.CursorEnd()
				m.updateSuggestions()
			}
			return m, nil

		case "ctrl+t":
			m.armedQuit = false
			m.toggleThinking()
			return m, nil

		case "enter":
			m.armedQuit = false
			return m, m.submit()

		case "up":
			m.armedQuit = false
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

		case "pgup", "pgdown":
			m.armedQuit = false
			m.vp, _ = m.vp.Update(msg)
			m.follow = m.vp.AtBottom()
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
	deny := func() {
		if m.appr != nil {
			m.appr.ch <- false
			m.appr = nil
		}
	}
	switch msg.String() {
	case "y", "Y", "enter":
		if m.appr != nil {
			m.appr.ch <- true
			m.appr = nil
		}
	case "n", "N", "esc":
		deny()
	case "ctrl+c":
		deny()
		*cmds = append(*cmds, func() tea.Msg { return tea.Quit() })
	}
}

// submit handles Enter: slash commands locally, everything else through
// the session. While a prompt is running the draft stays in the input.
func (m *tuiModel) submit() tea.Cmd {
	line := sanitizeText(strings.TrimSpace(m.ti.Value()))
	if line == "" {
		return nil
	}
	if strings.HasPrefix(line, "/") || line == ":q" {
		// Resolve through the popup: a unique match runs as-is (or by
		// prefix, "/st" → /stats), an ambiguous one waits for tab/↑↓
		// instead of dying as "unknown command".
		if s := m.sugs; len(s) > 1 {
			m.status = fmt.Sprintf("%d commands match — tab to complete, ↑/↓ to choose", len(s))
			return nil
		} else if len(s) == 1 {
			line = s[0].cmd
			m.status = "" // popup hint has served its purpose
		}
		m.ti.SetValue("")
		m.updateSuggestions()
		return m.slash(line)
	}
	if m.busy {
		m.status = "busy — esc interrupts the current prompt"
		return nil
	}

	m.ti.SetValue("")
	m.updateSuggestions()
	m.hist = append(m.hist, line)
	m.histIdx = len(m.hist)
	m.curBlk = 0 // a fresh prompt line closes any dangling thinking block
	m.lines = append(m.lines, tline{kind: linePrompt, raw: line}, tline{kind: lineStyled, raw: ""})
	if err := m.sess.Submit(line); err != nil {
		m.pushStyled(paint(cRed, "error: "+err.Error()))
		m.status = idleStatus
		m.refresh()
		return nil
	}
	m.busy = true
	m.status = ""
	m.follow = true
	m.refresh()
	return m.spin.Tick
}

func (m *tuiModel) slash(line string) tea.Cmd {
	head, args, _ := strings.Cut(line, " ")
	switch head {
	case "/quit", "/exit", ":q":
		return tea.Quit
	case "/stats":
		var b strings.Builder
		printStats(&b, m.sess.Stats(), m.metrics, m.sess.Hooks())
		m.flushPend()
		m.pushStyled("")
		m.pushStyled(strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")...)
	case "/clear":
		m.lines = nil
		m.pend = ""
		m.curBlk = 0
	case "/sessions":
		m.showSessions() // read-only: safe even while a prompt runs
	case "/resume", "/new":
		if m.pending != "" {
			m.status = m.pending + " already in progress"
			return nil
		}
		idx := 0 // /resume only: 0 = bare (picker), N = explicit slot
		if head == "/resume" {
			if a := strings.TrimSpace(args); a != "" {
				var ok bool
				if idx, ok = parseResumeIndex(a); !ok {
					m.pushStyled(paint(cYellow, "usage: /resume [N] — /sessions lists saved sessions"))
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
			m.status = head[1:] + " after the current prompt…"
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
	case "/think":
		m.toggleThinking()
	case "/help":
		m.flushPend()
		m.pushStyled(
			paint(cBold, "commands"),
			"  /stats   usage table: tokens, cache hits, tool latencies",
			"  /clear   clear the transcript view (session history kept)",
			"  /resume  restore a saved session — bare /resume opens a picker (/resume N jumps to slot N)",
			"  /sessions list this workspace's saved sessions",
			"  /new     fresh session: current conversation archived first",
			"  /think   expand/collapse thinking blocks",
			"  /help    this help",
			"  /quit    exit",
			paint(cDim, "typing / pops the command list — tab completes · ↑/↓ choose · esc closes"),
			paint(cDim, "keys: pgup/pgdn or wheel scroll · ↑/↓ prompt history · ctrl+t thinking fold · esc interrupt"),
			paint(cDim, "attach: @img.png sends an image with the prompt (quote spaces: @\"my shot.png\")"),
		)
	default:
		m.pushStyled(paint(cRed, "unknown command: "+head), paint(cDim, "try /help"))
	}
	m.refresh()
	return nil
}

// runTUI drives the bubbletea program until the user quits. The event pump
// forwards engine events into the loop; Close semantics stay with cmdChat.
// snap seeds the transcript when the session was restored with `chat -c`.
func runTUI(cfg agent.Config, sess *agent.Session, metrics *tools.Metrics, snap *agent.Snapshot) int {
	m := newTUIModel(cfg, sess, metrics)
	m.replay(snap)
	if snap == nil { // fresh session — surface the store for /resume
		if hint := resumeHint(cfg.Workspace, true); hint != "" { // picker wording
			m.pushStyled(paint(cDim, hint), "")
			m.refresh()
		}
	}
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	tuiProg.Store(p)
	defer tuiProg.Store(nil)

	// The pump keeps draining until the event channel closes — even after
	// the program quits, so a prompt finishing during exit never blocks on
	// a full buffer while saveChatSession waits for Export.
	go func() {
		for ev := range sess.Events() {
			p.Send(tuiEv{ev})
		}
	}()

	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "tui:", err)
		return 1
	}
	return 0
}
