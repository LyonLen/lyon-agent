package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/anthropics/anthropic-sdk-go"

	"lyon-agent/pkg/routing"
)

// Planning pre-pass (BACKLOG "规划 subagent", HANDOFF A-7): before a
// prompt's first API call, a cheap-tier call drafts the subtask
// breakdown. The plan rides three ways — a synthetic todo_write
// exec-start/end pair (every renderer's steps panel opens immediately),
// SetTodos (Export/resume carry it), and a turn-boundary system note (the
// main model starts executing with structure instead of spending its
// first turn planning).
//
// Contract — 方案A: wait before the first API call, bounded by
// plannerBudget, fail-open. Timeout, interrupt, garbage output, or an
// empty list all degrade to "the prompt runs exactly as before".
//
// Gate — LYON_PLANNER=off | auto | on (default auto):
//   - off: never;
//   - auto: the first substantial prompt of a session only — openness ≥
//     plannerOpenness (routing.Openness: design/migrate/…) or a long spec
//     (≥ plannerSpecRunes runes). Once per session: follow-ups keep the
//     plan the model maintains itself via todo_write;
//   - on: every prompt (the planner sees the recent conversation tail so
//     mid-session re-plans know the progress).
const (
	// plannerOpenness is the auto gate's routing threshold: tasks that
	// read open-ended earn a plan; mechanical one-liners do not.
	plannerOpenness = 3
	// plannerSpecRunes is the auto gate's length alternative: a spec
	// that long is multi-step whatever its vocabulary.
	plannerSpecRunes = 160
	// plannerMaxItems caps the drafted list (the renderer allows 20;
	// a pre-plan should stay a glanceable skeleton).
	plannerMaxItems = 10
	// plannerItemRunes caps one item's content.
	plannerItemRunes = 160
	// plannerTailCap bounds the recent-conversation tail handed to the
	// planner (runes, kept from the END — the latest turns matter).
	plannerTailCap = 3000
	// plannerMaxTokens bounds the planner's reply: 10 short items.
	plannerMaxTokens = 1200
)

// plannerBudget bounds one planning call end to end; the harvest never
// waits longer — the prompt proceeds without a plan. A var (not const)
// so tests can shrink it for the deadline path.
var plannerBudget = 4 * time.Second

// plannerMode reads LYON_PLANNER: "on" / "off" / anything else is the
// default "auto" (same env-gate pattern as LYON_DISTILL).
func plannerMode() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LYON_PLANNER"))) {
	case "on":
		return "on"
	case "off":
		return "off"
	default:
		return "auto"
	}
}

// planItem mirrors one todo_write entry (pkg/tools/todos.go shape) —
// the payload the renderers already parse.
type planItem struct {
	Content string `json:"content"`
	Status  string `json:"status"`
}

// plannerResult is what the planning goroutine hands back; err is folded
// into nil items at harvest (fail-open), kept for test diagnostics.
type plannerResult struct {
	items []planItem
	err   error
}

// startPlanner gates and launches the planning call. Returns nil when the
// gate is closed (the common path — zero cost). Called from drive under
// histMu, so reading runner/seed here is race-free; s.planned is written
// only by this single runner goroutine.
func (s *Session) startPlanner(pctx context.Context, prompt string) <-chan plannerResult {
	mode := plannerMode()
	if mode == "off" {
		return nil
	}
	trimmed := strings.TrimSpace(prompt)
	if trimmed == "" {
		return nil
	}
	if mode == "auto" {
		if s.planned ||
			(routing.Openness(trimmed) < plannerOpenness &&
				utf8.RuneCountInString(trimmed) < plannerSpecRunes) {
			return nil
		}
	}
	s.planned = true // auto: once per session; "on" re-plans every prompt

	// Recent conversation tail: the previous runner's closed history (or
	// the restored seed) — first prompt has neither, tail stays "".
	tail := ""
	if s.runner != nil {
		tail = clipLastRunes(renderHistory(s.runner.Messages()), plannerTailCap)
	} else if s.seed != nil {
		tail = clipLastRunes(renderHistory(s.seed), plannerTailCap)
	}

	ctx, cancel := context.WithTimeout(pctx, plannerBudget)
	ch := make(chan plannerResult, 1)
	go func() {
		defer cancel()
		items, err := s.planTasks(ctx, trimmed, tail)
		ch <- plannerResult{items: items, err: err}
	}()
	return ch
}

// harvestPlan waits for the planning call — bounded by the planner's own
// deadline (the goroutine reports promptly when ctx cancels the HTTP
// round trip) with a belt-and-braces timer on top, and by pctx for
// interrupts. Any non-success yields nil: fail-open, always.
func harvestPlan(ch <-chan plannerResult, pctx context.Context) []planItem {
	if ch == nil {
		return nil
	}
	select {
	case r := <-ch:
		if r.err != nil || len(r.items) == 0 {
			return nil
		}
		return r.items
	case <-pctx.Done():
		return nil
	case <-time.After(plannerBudget + time.Second):
		return nil
	}
}

// planTasks is the cheap-tier call itself: task (+ optional recent tail)
// in, JSON todo list out. Errors and unparseable replies come back as
// err/empty — the harvest turns both into "no plan".
func (s *Session) planTasks(ctx context.Context, prompt, tail string) ([]planItem, error) {
	user := prompt
	if tail != "" {
		user = "Recent conversation (tail):\n" + tail + "\n\nTask:\n" + prompt
	}
	params := anthropic.BetaMessageNewParams{
		Model:     s.eng.subagentModel(),
		MaxTokens: plannerMaxTokens,
		System:    []anthropic.BetaTextBlockParam{{Text: plannerSystemPrompt}},
		Messages: []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(
			anthropic.NewBetaTextBlock(user))},
		// JSON-out pre-pass: the budget is text-sized — thinking off
		// (same starvation as AskModel on reasoning gateways).
		Thinking: anthropic.BetaThinkingConfigParamUnion{
			OfDisabled: &anthropic.BetaThinkingConfigDisabledParam{},
		},
	}
	resp, err := s.eng.client.Beta.Messages.New(ctx, params)
	if err != nil {
		return nil, err
	}
	var parts []string
	for _, b := range resp.Content {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, b.Text)
		}
	}
	return parsePlannerTodos(strings.Join(parts, "\n")), nil
}

// plannerSystemPrompt instructs the drafting pass. JSON-only output
// keeps parsing trivial; "trivial tasks plan nothing" keeps the gate
// honest when mode=on fires on a one-liner.
const plannerSystemPrompt = `You are a planning pre-pass for an AI coding agent. Given a task (optionally with a recent conversation tail), draft the subtask breakdown the agent should execute.

Reply with a JSON object only — no prose, no code fences:
{"todos":[{"content":"short action phrase","status":"pending"}]}

Rules:
- 3 to 8 items in execution order; each content is ONE short actionable step (a few words), never a paragraph.
- Status is "pending" for every item (the harness marks the first as in_progress).
- Do not answer the task itself — only plan it.
- If the task is trivial or single-step, reply {"todos":[]}.`

// parsePlannerTodos pulls the todo list out of the reply, tolerating
// code fences and stray prose. Accepts {"todos":[…]} or a bare […].
func parsePlannerTodos(text string) []planItem {
	s := strings.TrimSpace(text)
	s = strings.ReplaceAll(s, "```json", "")
	s = strings.ReplaceAll(s, "```", "")
	if i := strings.IndexByte(s, '{'); i >= 0 {
		if j := strings.LastIndexByte(s, '}'); j > i {
			var obj struct {
				Todos []planItem `json:"todos"`
			}
			// Only claim the object when it actually carried todos —
			// a bare [{…}] array also contains a {...} substring, and
			// claiming that would starve the array branch below.
			if err := json.Unmarshal([]byte(s[i:j+1]), &obj); err == nil && len(obj.Todos) > 0 {
				return normalizePlanItems(obj.Todos)
			}
		}
	}
	if i := strings.IndexByte(s, '['); i >= 0 {
		if j := strings.LastIndexByte(s, ']'); j > i {
			var arr []planItem
			if err := json.Unmarshal([]byte(s[i:j+1]), &arr); err == nil {
				return normalizePlanItems(arr)
			}
		}
	}
	return nil
}

// normalizePlanItems enforces the renderer's contract: non-empty
// contents, known statuses, at most one in_progress, capped length and
// count — and, when nothing is marked, the first item becomes
// in_progress because work starts now.
func normalizePlanItems(items []planItem) []planItem {
	out := make([]planItem, 0, len(items))
	seenActive := false
	for _, it := range items {
		c := strings.TrimSpace(it.Content)
		if c == "" {
			continue
		}
		if r := []rune(c); len(r) > plannerItemRunes {
			c = string(r[:plannerItemRunes]) + "…"
		}
		st := it.Status
		switch st {
		case "pending", "in_progress", "completed":
		default:
			st = "pending"
		}
		if st == "in_progress" {
			if seenActive {
				st = "pending" // exactly one in_progress
			} else {
				seenActive = true
			}
		}
		out = append(out, planItem{Content: c, Status: st})
		if len(out) >= plannerMaxItems {
			break
		}
	}
	if len(out) > 0 && !seenActive {
		out[0].Status = "in_progress"
	}
	return out
}

// planNoteText renders the turn-boundary system note the main model
// reads: the structure plus the license to revise it.
func planNoteText(items []planItem) string {
	var b strings.Builder
	b.WriteString("Planned subtask breakdown (drafted by a planning pass before your turn — keep todo_write current as work progresses, and revise the plan freely if you learn better):\n")
	for i, it := range items {
		mark := ""
		switch it.Status {
		case "in_progress":
			mark = " (in progress)"
		case "completed":
			mark = " (done)"
		}
		fmt.Fprintf(&b, "%d. %s%s\n", i+1, it.Content, mark)
	}
	return b.String()
}

// adoptPlan surfaces the draft: the synthetic todo_write pair opens the
// steps panel (renderers key on tool_name, no tool_use_start needed),
// and SetTodos carries the list into Export so /resume rebuilds the
// panel. The conversation never sees a fake tool_use — only the note.
func (s *Session) adoptPlan(items []planItem) {
	payload, err := json.Marshal(struct {
		Todos []planItem `json:"todos"`
	}{Todos: items})
	if err != nil {
		return // marshal of plain strings cannot fail; stay fail-open
	}
	execID := s.seq.Add(1)
	s.emit(Event{
		Type:     EventToolExecStart,
		ExecID:   execID,
		ToolName: "todo_write",
		Input:    payload,
		TS:       time.Now(),
	})
	s.emit(Event{
		Type:     EventToolExecEnd,
		ExecID:   execID,
		ToolName: "todo_write",
		TS:       time.Now(),
	})
	s.SetTodos(payload)
}

// clipLastRunes keeps the last cap runes of s (the recent turns —
// distinct from bgshell's head-clipping clipTail), prefixed with an
// elision marker when cut.
func clipLastRunes(s string, cap int) string {
	r := []rune(s)
	if len(r) <= cap {
		return s
	}
	return "(earlier context elided)\n" + string(r[len(r)-cap:])
}

// joinNotes merges the planner note with any queued system note (memory
// acks, agent briefs) for the same turn boundary — plan first, it is the
// dominant context.
func joinNotes(plan, queued string) string {
	plan, queued = strings.TrimSpace(plan), strings.TrimSpace(queued)
	switch {
	case plan == "":
		return queued
	case queued == "":
		return plan
	default:
		return plan + "\n\n" + queued
	}
}
