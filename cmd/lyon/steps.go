package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"lyon-agent/pkg/agent"
)

// The right-hand steps panel is the session's live action audit: every
// effective decision the model made — tool calls, server-side tools,
// subagents, compactions — lands here as a numbered step with a running /
// done / error state, so a long run can be reviewed without re-reading the
// transcript. Steps derive purely from the engine event stream; nothing
// extra is sent to the model.

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
)

// execStep is one audited action. toolID pairs with tool_use_start /
// server_tool_result events, execID with tool_exec_start/end; zero means
// the pairing has not happened yet.
type execStep struct {
	n      int
	kind   stepKind
	toolID string
	execID uint64
	name   string
	detail string
	status stepStatus
	durMS  int64
	err    string
	ts     time.Time
}

const (
	// panelWidth is the right column's width; the transcript takes the rest.
	panelWidth = 40
	// panelMinWidth hides the panel on narrow terminals — a cramped
	// transcript helps nobody. /steps cannot override the floor.
	panelMinWidth = 110
	// maxSteps caps the audit tail so an all-day session cannot grow the
	// panel state without bound; numbering keeps counting past the trim.
	maxSteps = 300
)

// panelOn reports whether the right column is drawn at the current width.
func (m *tuiModel) panelOn() bool {
	return m.stepsOn && m.width >= panelMinWidth
}

// relayout recomputes the two column widths and re-arms the viewport.
// Call after any change to width or the /steps toggle.
func (m *tuiModel) relayout() {
	m.tw, m.pw = m.width, 0
	if m.panelOn() {
		m.pw = panelWidth
		m.tw = m.width - panelWidth - 2 // space + │ gutter column
		if m.tw < 40 {
			m.tw, m.pw = m.width, 0
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

// stepEvent folds one engine event into the step list. It runs on the UI
// thread inside applyEvent, so no locking.
func (m *tuiModel) stepEvent(ev agent.Event) {
	switch ev.Type {
	case agent.EventToolUseStart:
		kind, detail := stepTool, shortJSON(ev.Input, 80)
		if ev.Server {
			kind = stepServer
		}
		m.addStep(kind, ev.ToolID, ev.ToolName, detail)

	case agent.EventToolExecStart:
		// The harness always follows a tool_use with exec events (the
		// observer fires before approval, and OnToolEnd is deferred), so
		// the pairing target normally exists; create one defensively.
		s := m.findToolStep(ev.ToolName)
		if s == nil {
			s = m.addStep(stepTool, "", ev.ToolName, shortJSON(ev.Input, 80))
		}
		s.execID = ev.ExecID
		if ev.Input != nil && s.detail == "" {
			s.detail = shortJSON(ev.Input, 80)
		}

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
		m.stepSeq++
		if len(m.steps) >= maxSteps {
			m.steps = m.steps[len(m.steps)/2:]
		}
		m.steps = append(m.steps, execStep{
			n: m.stepSeq, kind: stepSubagent, name: ev.ToolName,
			detail: sanitizeText(ev.Detail), status: stepDone,
			durMS: ev.DurationMS, ts: time.Now(),
		})

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
		// end event — interrupt, transport failure. Record it truthfully.
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
			default:
				s.err = "unfinished"
			}
		}
	}
}

// renderSteps produces exactly h rows of at most w cells: a header, then
// the newest steps that fit (oldest first, like the transcript's flow),
// then blanks. ANSI-aware so the column edge stays aligned.
func (m *tuiModel) renderSteps(w, h int) []string {
	if w <= 0 || h <= 0 {
		return nil
	}
	rows := make([]string, 0, h)
	header := paint(cBold, "steps")
	if n := len(m.steps); n > 0 {
		header += paint(cDim, fmt.Sprintf(" · %d", n))
	}
	rows = append(rows, fitRow(header, w))

	// Tail window: newest last, as many as fit under the header.
	lo := len(m.steps) - (h - 1)
	if lo < 0 {
		lo = 0
	}
	for _, s := range m.steps[lo:] {
		if len(rows) >= h {
			break
		}
		rows = append(rows, fitRow(m.stepLine(s, w), w))
	}
	for len(rows) < h {
		rows = append(rows, "")
	}
	return rows
}

// stepLine renders one step: #n · status glyph · name · detail · timing.
func (m *tuiModel) stepLine(s execStep, w int) string {
	var glyph, gcolor string
	switch s.status {
	case stepRunning:
		glyph, gcolor = "⟳", cYellow
	case stepDone:
		glyph, gcolor = "✓", cGreen
	default:
		glyph, gcolor = "✗", cRed
	}
	kindColor := cCyan
	switch s.kind {
	case stepServer:
		kindColor = cMagenta
	case stepSubagent:
		kindColor = cCyan
	case stepCompact:
		kindColor = cYellow
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

	head := fmt.Sprintf("%s %s %s %s",
		paint(cDim, fmt.Sprintf("#%d", s.n)),
		paint(gcolor, glyph),
		paint(kindColor, s.name),
		paint(cDim, timing),
	)
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
