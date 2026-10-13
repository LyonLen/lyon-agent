package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"lyon-agent/pkg/agent"
)

// The right-hand steps panel is the session's live action audit, shaped
// like the conversation's task tree: every prompt opens a titled group row
// (the "why" behind what follows), tool executions sit indented under it,
// and run_subagent becomes a task node whose inner executions (Event.Sub)
// nest one level deeper — so a viewer can always tell which problem a
// stretch of steps belongs to and what the new work was for. Steps derive
// purely from the engine event stream; nothing extra is sent to the model.
//
// Interaction: alt+↑/↓ walk the history (window follows the selection,
// past the newest returns to live-follow), alt+enter expands the selected
// step's full operation, alt+←/→ resizes the panel; /steps toggles and
// /steps open|close|width N mirrors the keys as commands.

type stepStatus uint8

const (
	stepRunning stepStatus = iota
	stepDone
	stepError
)

type stepKind uint8

const (
	stepTool stepKind = iota
	stepServer
	stepSubagent
	stepCompact
	stepPrompt   // a prompt round: the titled parent of the steps after it
	stepMemory   // memory injection / distill / sync — the hot tier at work
	stepDiagnose // 错因归纳: the async post-mortem of a failed prompt
	stepEval     // eval machinery: exams, workflow freezing — the §1/§3 meta layer
)

// execStep is one audited action. toolID pairs with tool_use_start /
// server_tool_result events, execID with tool_exec_start/end; zero means
// the pairing has not happened yet. under links a subagent's inner
// execution to its task node (0 = top level). agent is the model badge:
// which model did this work (prompt groups carry the main tier, task
// nodes the subagent tier) — "who" for a multi-agent session.
type execStep struct {
	n      int
	kind   stepKind
	toolID string
	execID uint64
	name   string
	detail string
	agent  string
	status stepStatus
	durMS  int64
	err    string
	ts     time.Time
	under  int
}

const (
	// panelWidth is the default right column's width; the transcript takes
	// the rest. alt+←/→ moves it within [panelWMin, panelWMax].
	panelWidth = 40
	panelWMin  = 28
	panelWMax  = 72
	// panelMinWidth hides the panel on narrow terminals — a cramped
	// transcript helps nobody. /steps cannot override the floor.
	panelMinWidth = 110
	// maxSteps caps the audit tail so an all-day session cannot grow the
	// panel state without bound; numbering keeps counting past the trim.
	maxSteps = 300
	// expandMaxRows caps how many rows one expanded step may eat, so a
	// long command can never swallow the whole panel.
	expandMaxRows = 6
)

// stepDTO is execStep's persistable shape: the struct's fields are
// unexported by design (the panel is renderer-private), so snapshots get
// an explicit mirror instead of loosening that. Numeric kind/status keep
// the blob compact; the encoding is private to cmd/lyon.
type stepDTO struct {
	N      int        `json:"n"`
	Kind   stepKind   `json:"k"`
	ToolID string     `json:"tid,omitempty"`
	ExecID uint64     `json:"xid,omitempty"`
	Name   string     `json:"name,omitempty"`
	Detail string     `json:"detail,omitempty"`
	Agent  string     `json:"agent,omitempty"`
	Status stepStatus `json:"st"`
	DurMS  int64      `json:"ms,omitempty"`
	Err    string     `json:"err,omitempty"`
	TS     time.Time  `json:"ts,omitempty"`
	Under  int        `json:"under,omitempty"`
}

func (s execStep) MarshalJSON() ([]byte, error) {
	return json.Marshal(stepDTO{
		N: s.n, Kind: s.kind, ToolID: s.toolID, ExecID: s.execID,
		Name: s.name, Detail: s.detail, Agent: s.agent, Status: s.status,
		DurMS: s.durMS, Err: s.err, TS: s.ts, Under: s.under,
	})
}

func (s *execStep) UnmarshalJSON(b []byte) error {
	var d stepDTO
	if err := json.Unmarshal(b, &d); err != nil {
		return err
	}
	*s = execStep{
		n: d.N, kind: d.Kind, toolID: d.ToolID, execID: d.ExecID,
		name: d.Name, detail: d.Detail, agent: d.Agent, status: d.Status,
		durMS: d.DurMS, err: d.Err, ts: d.TS, under: d.Under,
	}
	return nil
}

// stepsState captures the panel for Snapshot.Steps. A row still running at
// save time was in flight when the exit path interrupted the prompt — it
// closes as interrupted, so a restored panel never shows a phantom spinner.
func stepsState(steps []execStep) json.RawMessage {
	if len(steps) == 0 {
		return nil
	}
	out := make([]execStep, len(steps))
	copy(out, steps)
	for i := range out {
		if out[i].status == stepRunning {
			out[i].status = stepError
			out[i].err = "interrupted"
		}
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return raw
}

// applySteps loads a saved panel back after /resume or startup restore.
// Missing or broken blobs keep the fresh-restart behavior (no panel rows);
// stepSeq resumes past the restored numbering so new rows never collide.
func (m *tuiModel) applySteps(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var steps []execStep
	if err := json.Unmarshal(raw, &steps); err != nil || len(steps) == 0 {
		return false
	}
	m.steps = steps
	m.stepSeq = steps[len(steps)-1].n
	m.stepSel = 0
	m.stepOpen = false
	return true
}

// panelOn reports whether the right column is drawn at the current width.
// Either panel requests it: the steps audit or the evolution board.
func (m *tuiModel) panelOn() bool {
	return (m.stepsOn || m.evoOn) && m.width >= panelMinWidth
}

// relayout recomputes the two column widths and re-arms the viewport.
// Call after any change to width or the /steps toggle. The panel width
// is user-adjustable but never at the cost of a usable transcript column.
func (m *tuiModel) relayout() {
	m.tw, m.pw = m.width, 0
	if m.panelOn() {
		pw := m.panelW
		if pw <= 0 {
			pw = panelWidth // zero-value default (tests, fresh models)
		}
		if m.width-pw-2 < 40 { // keep a readable transcript column
			pw = m.width - 42
		}
		if pw >= panelWMin {
			m.pw = pw
			m.tw = m.width - pw - 2 // space + │ gutter column
		}
	}
	m.vp.Width = m.tw
}

// addStep appends one step in the running state, trimming the audit tail.
func (m *tuiModel) addStep(kind stepKind, toolID, name, detail string) *execStep {
	m.stepSeq++
	if len(m.steps) >= maxSteps {
		m.steps = m.steps[len(m.steps)/2:] // keep the recent half, not the last one
	}
	m.steps = append(m.steps, execStep{
		n: m.stepSeq, kind: kind, toolID: toolID,
		name: name, detail: detail,
		status: stepRunning, ts: time.Now(),
	})
	return &m.steps[len(m.steps)-1]
}

// findToolStep returns the earliest running tool step matching name that
// has no exec yet — FIFO, so parallel calls of one tool pair in decision
// order — falling back to any running step of that name.
func (m *tuiModel) findToolStep(name string) *execStep {
	var fallback *execStep
	for i := range m.steps {
		s := &m.steps[i]
		if s.kind != stepTool || s.name != name || s.status != stepRunning {
			continue
		}
		if s.execID == 0 {
			return s
		}
		if fallback == nil {
			fallback = s
		}
	}
	return fallback
}

// activeSubagent returns the running run_subagent step (the task node its
// inner executions nest under), searching from the newest backwards.
func (m *tuiModel) activeSubagent() *execStep {
	for i := len(m.steps) - 1; i >= 0; i-- {
		s := &m.steps[i]
		if s.name == "run_subagent" && s.status == stepRunning &&
			(s.kind == stepTool || s.kind == stepSubagent) {
			return s
		}
	}
	return nil
}

// startPromptStep opens a new prompt group — one titled parent row that
// answers "what problem are these steps solving". The title is the
// prompt itself, collapsed to a single line.
func (m *tuiModel) startPromptStep(prompt string) {
	title := strings.Join(strings.Fields(prompt), " ")
	if title == "" {
		return
	}
	m.stepSeq++
	if len(m.steps) >= maxSteps {
		m.steps = m.steps[len(m.steps)/2:]
	}
	m.steps = append(m.steps, execStep{
		n: m.stepSeq, kind: stepPrompt, detail: title,
		agent:  m.cfg.Model, // badge: which model is answering this round
		status: stepDone, ts: time.Now(),
	})
}

// pushAsyncStep opens a running row for an operation that finishes off
// the event stream (memory distill, cloud sync, failure diagnosis). The
// returned number is the handle finishStep resolves later.
func (m *tuiModel) pushAsyncStep(kind stepKind, name, detail string) int {
	return m.addStep(kind, "", name, detail).n
}

// finishStep resolves an async step by its number: status, final detail
// and error text. A trimmed-out step (ancient session) is a silent no-op.
func (m *tuiModel) finishStep(n int, status stepStatus, detail, errStr string) {
	s := m.stepBy(n)
	if s == nil {
		return
	}
	s.status = status
	s.durMS = time.Since(s.ts).Milliseconds()
	if detail != "" {
		s.detail = detail
	}
	if errStr != "" {
		s.err = errStr
	}
}

// stepBy resolves a step by its number (the handle async messages carry);
// nil once trimmed out of the audit tail.
func (m *tuiModel) stepBy(n int) *execStep {
	for i := range m.steps {
		if m.steps[i].n == n {
			return &m.steps[i]
		}
	}
	return nil
}

// stepByKind returns the newest step of a kind, or nil.
func (m *tuiModel) stepByKind(k stepKind) *execStep {
	for i := len(m.steps) - 1; i >= 0; i-- {
		if m.steps[i].kind == k {
			return &m.steps[i]
		}
	}
	return nil
}

// stepEvent folds one engine event into the step list. It runs on the UI
// thread inside applyEvent, so no locking.
func (m *tuiModel) stepEvent(ev agent.Event) {
	switch ev.Type {
	case agent.EventToolUseStart:
		kind, detail := stepTool, stepArgPreview(ev.ToolName, ev.Input)
		if ev.Server {
			kind = stepServer
		}
		s := m.addStep(kind, ev.ToolID, ev.ToolName, detail)
		if ev.ToolName == "run_subagent" {
			// Task nodes wear the subagent tier's badge from birth; the
			// end event confirms the model actually used.
			s.agent = m.cfg.SubagentModel
		}

	case agent.EventToolExecStart:
		var s *execStep
		if ev.Sub {
			// Inside a run_subagent: never pair with a main-flow step of
			// the same name — hang this execution off the active task.
			s = m.addStep(stepTool, "", ev.ToolName, stepArgPreview(ev.ToolName, ev.Input))
			if p := m.activeSubagent(); p != nil {
				s.under = p.n
			}
		} else {
			// The harness always follows a tool_use with exec events (the
			// observer fires before approval, and OnToolEnd is deferred), so
			// the pairing target normally exists; create one defensively.
			s = m.findToolStep(ev.ToolName)
			if s == nil {
				s = m.addStep(stepTool, "", ev.ToolName, stepArgPreview(ev.ToolName, ev.Input))
			}
			if ev.Input != nil && s.detail == "" {
				s.detail = stepArgPreview(ev.ToolName, ev.Input)
			}
		}
		s.execID = ev.ExecID

	case agent.EventToolExecEnd:
		var s *execStep
		for i := range m.steps {
			if m.steps[i].execID == ev.ExecID && m.steps[i].status == stepRunning {
				s = &m.steps[i]
				break
			}
		}
		if s == nil {
			s = m.findToolStep(ev.ToolName)
		}
		if s == nil {
			return
		}
		s.durMS = ev.DurationMS
		if ev.IsError {
			s.status = stepError
			s.err = sanitizeText(ev.Err)
		} else {
			s.status = stepDone
		}

	case agent.EventServerToolResult:
		for i := range m.steps {
			s := &m.steps[i]
			if s.toolID == ev.ToolID && s.status == stepRunning {
				if ev.IsError {
					s.status = stepError
					s.err = "server tool error"
				} else {
					s.status = stepDone
				}
				return
			}
		}

	case agent.EventSubagentEnd:
		// The run_subagent step (created at tool_use) becomes the parent
		// task node; its inner execs already hang under it via `under`.
		p := m.activeSubagent()
		if p == nil {
			// Defensive: use_start never arrived. Record a standalone node.
			m.stepSeq++
			if len(m.steps) >= maxSteps {
				m.steps = m.steps[len(m.steps)/2:]
			}
			m.steps = append(m.steps, execStep{
				n: m.stepSeq, kind: stepSubagent, name: "run_subagent",
				detail: sanitizeText(ev.Detail), status: stepDone,
				durMS: ev.DurationMS, ts: time.Now(),
			})
			return
		}
		p.kind = stepSubagent
		p.status = stepDone
		p.durMS = ev.DurationMS
		if ev.ToolName != "" {
			p.agent = ev.ToolName // the end event names the model that ran it
		}
		if ev.Detail != "" {
			p.detail += " · " + ev.Detail // task title · "3 API calls"
		}

	case agent.EventMemoryInjected:
		// The hot tier rides into the first message of the session — the
		// panel shows the moment injection actually happened.
		detail := sanitizeText(ev.Detail)
		if detail == "" {
			detail = "hot memory injected"
		}
		s := m.addStep(stepMemory, "", "memory.inject", detail)
		s.status = stepDone // injection is instantaneous — born finished

	case agent.EventCompaction:
		status, errStr := stepDone, ""
		detail := sanitizeText(ev.Detail)
		if strings.HasPrefix(detail, "compaction failed") || strings.Contains(detail, "no text") {
			status, errStr = stepError, detail
		}
		m.stepSeq++
		if len(m.steps) >= maxSteps {
			m.steps = m.steps[len(m.steps)/2:]
		}
		m.steps = append(m.steps, execStep{
			n: m.stepSeq, kind: stepCompact, name: "compact",
			detail: detail, status: status, err: errStr,
			durMS: ev.DurationMS, ts: time.Now(),
		})

	case agent.EventPromptEnd:
		// Anything still running when the prompt closes never got its
		// end event — interrupt, budget cut, transport failure. Record
		// it truthfully.
		for i := range m.steps {
			s := &m.steps[i]
			if s.status != stepRunning {
				continue
			}
			s.status = stepError
			switch {
			case ev.Err == agent.ErrInterrupted.Error():
				s.err = "interrupted"
			case ev.Err != "":
				s.err = "aborted: " + sanitizeText(ev.Err)
			case ev.StopReason == "max_iterations":
				s.err = "over budget"
			default:
				s.err = "unfinished"
			}
		}
	}
}

// ── human-readable arguments ──────────────────────────────────────────

// stepArgPreview renders tool input as a one-line human summary instead
// of raw JSON: the command, query, URL, path or task the model actually
// asked for. Document-order fallback keeps it deterministic; only an
// input that survives none of that falls back to truncated JSON.
func stepArgPreview(name string, input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(input, &obj); err != nil {
		return oneLine(string(input))
	}
	if k := preferArgKey(name); k != "" {
		if s := argString(obj[k]); s != "" {
			return oneLine(s)
		}
	}
	for _, k := range []string{"command", "path", "url", "query", "pattern", "task", "title", "prompt"} {
		if s := argString(obj[k]); s != "" {
			return oneLine(s)
		}
	}
	// First string VALUE in document order (unknown schemas, MCP tools).
	// Decoder tokens hand object keys as plain strings too, so track
	// key/value position: after '{' or ',' a string is a key, after a
	// key comes its value — nested '[' flips values back on.
	dec := json.NewDecoder(bytes.NewReader(input))
	expectValue := false
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case string:
			if expectValue {
				if strings.TrimSpace(t) != "" {
					return oneLine(t)
				}
				expectValue = false // empty value: the next string is a key
			} else {
				expectValue = true // a key: the next string is its value
			}
		case json.Delim:
			expectValue = t == '['
		default: // number/bool/null: a value — the next string is a key
			expectValue = false
		}
	}
	return shortJSON(input, 80)
}

// preferArgKey names the field that best answers "what is this call
// doing" for each built-in tool.
func preferArgKey(name string) string {
	switch name {
	case "run_shell":
		return "command"
	case "web_search":
		return "query"
	case "web_fetch":
		return "url"
	case "run_subagent":
		return "task"
	case "grep", "glob":
		return "pattern"
	case "save_memory", "forget_memory":
		return "title"
	case "read_memory":
		return "query"
	}
	return "" // file tools fall through to the generic key list (path)
}

func argString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// oneLine collapses whitespace runs (newlines, tabs, double spaces) so a
// multi-line command reads as a single panel row.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// ── colors & rows ─────────────────────────────────────────────────────

// stepColor distinguishes tool categories at a glance: shell green,
// subagent blue, writes yellow, web/MCP magenta, reads and unknowns cyan.
func stepColor(s execStep) string {
	switch s.kind {
	case stepPrompt:
		return cCyan
	case stepCompact:
		return cYellow
	case stepSubagent:
		return cBlue
	case stepMemory:
		return cYellow // memory is a write, like save_memory
	case stepDiagnose:
		return cMagenta // meta-analysis: its own family
	case stepEval:
		return cCyan // exam / flow-freeze: assessment, another meta family
	}
	switch s.name {
	case "run_shell":
		return cGreen
	case "run_subagent":
		return cBlue
	case "write_file", "edit_file", "save_memory", "forget_memory":
		return cYellow
	case "web_search", "web_fetch":
		return cMagenta
	}
	if strings.HasPrefix(s.name, "mcp_") {
		return cMagenta
	}
	return cCyan
}

// stepDepth is the indent level: prompt titles at 0, main-flow steps at
// 1, a subagent's inner executions at 2.
func (m *tuiModel) stepDepth(s execStep) int {
	switch {
	case s.kind == stepPrompt:
		return 0
	case s.under != 0:
		return 2
	default:
		return 1
	}
}

// reverseRow marks the selected row (guarded like paint for NO_COLOR).
func reverseRow(s string) string {
	if !color || s == "" {
		return s
	}
	return "\x1b[7m" + s + "\x1b[27m"
}

// renderSteps produces exactly h rows of at most w cells: a header (with
// the selection marker), then the visible window of steps — live tail by
// default, or centered on the selection (expanded block kept whole) —
// then blanks. ANSI-aware so the column edge stays aligned.
func (m *tuiModel) renderSteps(w, h int) []string {
	if w <= 0 || h <= 0 {
		return nil
	}
	header := paint(cBold, tr("steps", "步骤"))
	if n := len(m.steps); n > 0 {
		header += paint(cDim, fmt.Sprintf(" · %d", n))
	}
	if m.stepSel > 0 {
		header += paint(cYellow, fmt.Sprintf(" · @#%d", m.stepSel))
	}

	// Build every row; the selected step may span several (expanded).
	type prow struct {
		n    int
		text string
	}
	var rows []prow
	for _, s := range m.steps {
		sel := m.stepSel != 0 && s.n == m.stepSel
		lines := []string{m.stepLine(s, w)}
		if sel && m.stepOpen {
			lines = append(lines, m.stepExpandRows(s, w)...)
		}
		for _, ln := range lines {
			t := fitRow(ln, w)
			if sel {
				t = reverseRow(t)
			}
			rows = append(rows, prow{n: s.n, text: t})
		}
	}

	// Header on top, a key-hint footer at the bottom (the evolution
	// panel already had one; steps didn't — #41), the step window
	// between them.
	avail := h - 1
	footer := ""
	if h >= 2 {
		avail = h - 2
		// 35 cells EN / 34 ZH — fits the default 40-col panel whole;
		// the alt+ prefix carries over from the row, only the first key
		// spells it out.
		footer = fitRow(paint(cDim, tr("alt+↑↓ select · enter open · ←→ width",
			"alt+↑↓ 选择 · enter 展开 · ←→ 调宽")), w)
	}
	if avail <= 0 {
		return []string{fitRow(header, w)}
	}
	lo := 0
	if len(rows) > avail {
		lo = len(rows) - avail // live follow: newest at the bottom
		if m.stepSel > 0 {
			idx := -1
			for i, r := range rows {
				if r.n == m.stepSel {
					idx = i
					break
				}
			}
			if idx >= 0 {
				block := 1
				for idx+block < len(rows) && rows[idx+block].n == m.stepSel {
					block++
				}
				if block > avail {
					block = avail
				}
				lo = idx - (avail - block) // selection block sits at the bottom
				if max := len(rows) - avail; lo > max {
					lo = max
				}
				if lo < 0 {
					lo = 0
				}
			}
		}
	}

	out := make([]string, 0, h)
	out = append(out, fitRow(header, w))
	end := lo + avail
	if end > len(rows) {
		end = len(rows)
	}
	for _, r := range rows[lo:end] {
		out = append(out, r.text)
	}
	for len(out) < h {
		out = append(out, "")
	}
	if footer != "" {
		out[h-1] = footer
	}
	return out
}

// stepLine renders one step: indent · #n · status glyph · name · detail ·
// timing. Prompt groups render as a bold titled rule instead.
func (m *tuiModel) stepLine(s execStep, w int) string {
	indent := strings.Repeat("  ", m.stepDepth(s))

	if s.kind == stepPrompt {
		pre := indent + paint(cDim, fmt.Sprintf("#%d", s.n)) + " "
		if s.agent == "" {
			if avail := w - ansi.StringWidth(pre); avail > 2 {
				pre += paint(cBold, ansi.Truncate(sanitizeText(s.detail), avail, "…"))
			}
			return pre
		}
		// Model badge hugs the right edge: titles vary in length, the
		// badge stays put.
		badge := paint(cDim, "["+shortModel(s.agent)+"]")
		avail := w - ansi.StringWidth(pre) - ansi.StringWidth(badge)
		if avail > 2 {
			title := ansi.Truncate(sanitizeText(s.detail), avail, "…")
			pre += paint(cBold, title) + strings.Repeat(" ", avail-ansi.StringWidth(title))
		}
		return pre + badge
	}

	var glyph, gcolor string
	switch s.status {
	case stepRunning:
		// » not ⟳: rotation glyphs fall back to emoji fonts that draw
		// double-width, wrapping a full panel row (#46).
		glyph, gcolor = "»", cYellow
	case stepDone:
		glyph, gcolor = "✓", cGreen
	default:
		glyph, gcolor = "✗", cRed
	}

	var timing string
	switch s.status {
	case stepRunning:
		el := time.Since(s.ts).Round(time.Second)
		if el < time.Second {
			el = time.Second
		}
		timing = el.String()
	case stepDone:
		timing = fmtDur(s.durMS)
	default:
		timing = "err"
	}

	head := fmt.Sprintf("%s%s %s %s",
		indent,
		paint(cDim, fmt.Sprintf("#%d", s.n)),
		paint(gcolor, glyph),
		paint(stepColor(s), s.name),
	)
	if s.agent != "" {
		// Model badge — who did this (task nodes; prompt groups render
		// theirs right-aligned in the branch above).
		head += " " + paint(cDim, "["+shortModel(s.agent)+"]")
	}
	head += " " + paint(cDim, timing)
	// Detail (or the error) fills what is left of the row.
	if rest := w - ansi.StringWidth(head) - 1; rest > 2 {
		tail := s.detail
		if s.status == stepError && s.err != "" {
			tail = s.err
		}
		if tail != "" {
			head += " " + paint(cDim, ansi.Truncate(sanitizeText(tail), rest, "…"))
		}
	}
	return head
}

// stepExpandRows renders the body rows under the selected step's first
// line: the full operation text, the full error when any, and the start
// time — capped so a huge command cannot eat the panel.
func (m *tuiModel) stepExpandRows(s execStep, w int) []string {
	indent := strings.Repeat("  ", m.stepDepth(s)+1)
	avail := w - len(indent)
	if avail < 8 {
		avail = 8
	}
	var out []string
	add := func(text, style string) {
		for _, ln := range wrapPlain(text, avail) {
			out = append(out, indent+paint(style, ln))
		}
	}
	if d := sanitizeText(s.detail); d != "" {
		add(d, cDim)
	}
	if s.status == stepError && s.err != "" {
		add(sanitizeText(s.err), cRed)
	}
	add("started "+s.ts.Format("15:04:05"), cDim)
	if len(out) > expandMaxRows {
		out = out[:expandMaxRows]
		last := out[expandMaxRows-1]
		if ansi.StringWidth(last)+2 > w { // the marker must not overflow
			last = ansi.Truncate(last, w-2, "")
		}
		out[expandMaxRows-1] = last + paint(cDim, " …")
	}
	return out
}

// wrapPlain hard-wraps plain (ANSI-free) text at w columns, honoring
// existing newlines.
func wrapPlain(s string, w int) []string {
	if w < 4 {
		w = 4
	}
	var out []string
	for _, para := range strings.Split(s, "\n") {
		runes := []rune(para)
		if len(runes) == 0 {
			out = append(out, "")
			continue
		}
		for len(runes) > 0 {
			n := w
			if len(runes) < n {
				n = len(runes)
			}
			out = append(out, string(runes[:n]))
			runes = runes[n:]
		}
	}
	if len(out) == 0 {
		out = append(out, "")
	}
	return out
}

// ── interaction ───────────────────────────────────────────────────────

// stepMove walks the audit history. First alt+up selects the newest step;
// further moves walk older ones. Moving past the newest returns to
// live-follow (stepSel = 0); the render window follows the selection.
func (m *tuiModel) stepMove(dir int) {
	if len(m.steps) == 0 {
		return
	}
	if m.stepSel == 0 {
		if dir > 0 {
			return // already following the live tail
		}
		m.stepSel = m.steps[len(m.steps)-1].n
		return
	}
	pos := -1
	for i, s := range m.steps {
		if s.n == m.stepSel {
			pos = i
			break
		}
	}
	if pos < 0 { // trimmed out of the audit tail
		m.stepSel = 0
		m.stepOpen = false
		return
	}
	np := pos + dir
	if np < 0 {
		return
	}
	if np >= len(m.steps) {
		m.stepSel = 0 // walked past the newest → back to live
		m.stepOpen = false
		return
	}
	m.stepSel = m.steps[np].n
}

// stepToggleOpen expands/collapses the selected step; with nothing
// selected it selects the newest (the natural first thing to inspect).
func (m *tuiModel) stepToggleOpen() {
	if len(m.steps) == 0 {
		return
	}
	if m.stepSel == 0 {
		m.stepSel = m.steps[len(m.steps)-1].n
		m.stepOpen = true
		return
	}
	m.stepOpen = !m.stepOpen
}

// stepResize grows/shrinks the panel, clamped; the transcript re-flows
// through relayout.
func (m *tuiModel) stepResize(dw int) {
	if m.panelW <= 0 {
		m.panelW = panelWidth
	}
	m.panelW += dw
	if m.panelW < panelWMin {
		m.panelW = panelWMin
	}
	if m.panelW > panelWMax {
		m.panelW = panelWMax
	}
	m.relayout()
	m.status = tr(fmt.Sprintf("panel width %d", m.panelW),
		fmt.Sprintf("面板宽度 %d", m.panelW))
}

// fitRow truncates then space-pads one panel row to exactly w cells.
func fitRow(s string, w int) string {
	if ansi.StringWidth(s) > w {
		s = ansi.Truncate(s, w, "…")
	}
	if pad := w - ansi.StringWidth(s); pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	return s
}

// shortModel makes a model id badge-sized: the "claude-" family prefix
// carries no information ("claude-opus-5-5" is opus-5-5 to a reader of
// this panel), and a runaway id is clipped so it cannot eat the row.
func shortModel(id string) string {
	id = strings.TrimPrefix(id, "claude-")
	if r := []rune(id); len(r) > 18 {
		return string(r[:18]) + "…"
	}
	return id
}

// fmtDur compresses milliseconds the way a glance wants to read them.
func fmtDur(ms int64) string {
	switch {
	case ms < 1000:
		return fmt.Sprintf("%dms", ms)
	case ms < 60_000:
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	default:
		return fmt.Sprintf("%dm%ds", ms/60_000, (ms%60_000)/1000)
	}
}
