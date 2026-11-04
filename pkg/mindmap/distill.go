package mindmap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/hooks"
)

// Distiller bounds — §8's cost contract: one subagent-tier call per
// prompt_end, input ≈ the transcript tail (6-8k tokens), output ≤1.5k.
const (
	// distillMaxTokens is the output cap (§8: ≤1.5k). A diff is small;
	// the budget leaves room for the model's visible reasoning.
	distillMaxTokens int64 = 1500
	// transcriptTailRunes bounds the rendered transcript handed to the
	// model. The tail is what changed; older rounds already landed in
	// earlier diffs (≈6-8k tokens of mixed EN/CJK).
	transcriptTailRunes = 10000
	// msgClipRunes keeps one message from eating the whole tail budget.
	msgClipRunes = 600
)

// distillTimeout bounds one distiller call. A var so tests can shrink it;
// production value matches evolve's post-mortem leg (60s).
var distillTimeout = 60 * time.Second

// distillSystemPrompt is the §6 discipline in one place: JSON-only diff
// output, the hat limits, admission, prune-first, id reuse, tombstone
// and pinned rules. The server re-enforces every numeric limit on
// arrival (mindmap.go) — the prompt is the cheap half of the contract.
const distillSystemPrompt = `You are the mind-map distiller for an ongoing conversation. Maintain a compact outline of what the conversation CURRENTLY understands — a living tree of conclusions, not a summary, not a transcript.

Reply with ONE JSON object only — no prose, no code fences:
{"title":"...","subtitle":"...","ops":[...]}

ops you may emit (all optional; emit {"ops":[]} when nothing changed):
- {"op":"add","id":"<new-slug-id>","parent_id":"<existing-id-or-empty>","label":"...","kind":"branch|decision|fact|open_question","source_turn":<int — first turn this topic appeared>}
- {"op":"rewrite","id":"<existing-id>","label":"..."} — rephrase a node whose wording went stale
- {"op":"park","id":"<existing-id>"} — a refuted or abandoned branch: park it, never delete
- {"op":"set_parent","id":"<existing-id>","parent_id":"<existing-id-or-empty>"}
- {"op":"merge","id":"<survivor-id>","source_ids":["<id>",...]} — fold duplicate branches into one

Hard limits (server-enforced; violating ops are dropped):
- depth ≤ 4 (root depth 0), total ≤ 60 alive nodes, ≤ 8 alive children per branch, label ≤ 24 characters.
- kind ∈ branch|decision|fact|open_question. Never emit status/state/pinned/tombstone — the server owns those.

Discipline:
- Admission: only write content that carries a conclusion, decision, fact, or open question. Greetings and process talk never become nodes.
- Parallel phrasing: siblings must read as peers — all children of one parent (and all branches at one level) use ONE sentence template: all noun phrases, or all "topic: point", or all verb phrases. Never mix templates within a sibling set. When you rewrite/add/remove any child, normalize the WHOLE sibling set under that parent to the same template in the same diff.
- Prune over grow: the map reflects the current understanding. When new information refutes a branch, PARK it — do not keep both versions side by side.
- ID stability: ALWAYS reuse ids from the inventory — an unchanged branch keeps its id so the map never flickers. New nodes get fresh slug ids like b-<ascii>. Never re-add an id that already exists, and never add a label similar to an existing one (the server merges or drops it).
- Deleted labels listed in the inventory must NEVER reappear in any form.
- pinned=yes nodes are the user's: you may only add children BENEATH them; never rewrite / park / move / merge them (the server drops such ops and audits a violation).
- No fabrication: every node must be grounded in the transcript. Do not invent conclusions the conversation did not reach. Labels are short phrases in the conversation's own language.
- title/subtitle: set only when the framing is stale; subtitle should read as "lyon's current understanding — correct it if wrong", never as a verdict.`

// Hook is the prompt_end seam (§2): same shape as evolve.Hook — gate,
// claim the seat's single distill slot, then hand the actual work to its
// own goroutine so the hub tap returns immediately. LYON_MINDMAP=off
// makes it a complete no-op: zero goroutines, zero calls, zero events
// (D2).
func Hook(sess *agent.Session, ev agent.Event) {
	if ev.Type != agent.EventPromptEnd || sess == nil || Off() {
		return
	}
	sid := sess.SeatID()
	if sid == "" {
		return
	}
	m := Open()
	if !m.beginDistill(sid) {
		return // a previous round is still in flight — skip, never queue (C1)
	}
	go func() {
		defer m.endDistill(sid)
		m.distill(sess, sid)
	}()
}

// distill is one full round: export transcript → inventory (captures the
// §5 base revision) → one Ask → parse → ApplyDistill → audit → publish.
// Every failure path is a silent skip with one audit line (D1): no half
// nodes, no revision tick, nothing the user must react to.
func (m *Manager) distill(sess *agent.Session, sid string) {
	snap := sess.Export()
	if len(snap.Messages) == 0 {
		return
	}
	transcript := renderTranscript(snap.Messages, transcriptTailRunes)
	if strings.TrimSpace(transcript) == "" {
		return
	}
	base, inv, err := m.Inventory(sid)
	if err != nil {
		m.fire(sess, Audit{
			Event: EventDistill, SessionID: sid, IsError: true,
			Err: err.Error(), Detail: "distill skipped (inventory): " + err.Error(),
		})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), distillTimeout)
	defer cancel()
	reply, err := sess.Ask(ctx, distillSystemPrompt, buildUserPrompt(snap.Title, base, inv, transcript), distillMaxTokens)
	if err != nil {
		// timeout / transport / 4xx — D1: skip the round silently
		m.fire(sess, Audit{
			Event: EventDistill, SessionID: sid, IsError: true,
			Err: err.Error(), Detail: "distill skipped (ask failed)",
		})
		return
	}
	diff, err := ParseDiff(reply)
	if err != nil {
		m.fire(sess, Audit{
			Event: EventDistill, SessionID: sid, IsError: true,
			Err: err.Error(), Detail: "distill skipped (bad JSON)",
		})
		return
	}
	// ApplyDistill publishes EVENT_MINDMAP_UPDATE itself when the diff
	// lands — the seam lives with the state commit, not with this caller.
	res := m.ApplyDistill(sid, base, diff)
	for _, a := range res.Audits {
		m.fire(sess, a)
	}
}

// fire routes one audit record through the session's hooks manager —
// never through pkg/hooks constants: these event names are plain strings
// owned by this package. A nil hooks manager (bare test session) is a
// no-op, not a panic.
func (m *Manager) fire(sess *agent.Session, a Audit) {
	if sess == nil {
		return
	}
	h := sess.Hooks()
	if h == nil {
		return
	}
	h.Fire(hooks.Record{
		Event:     a.Event,
		Detail:    a.Detail,
		SessionID: a.SessionID,
		IsError:   a.IsError,
		Err:       a.Err,
	})
}

// buildUserPrompt assembles the round's user message: the map's current
// framing, the node inventory (with the base revision the reply is
// judged against), and the transcript tail.
func buildUserPrompt(title string, base int, inv, transcript string) string {
	t := strings.TrimSpace(title)
	if t == "" {
		t = "(untitled)"
	}
	return fmt.Sprintf("Map revision: %d\nTitle: %s\n\n%s\nTranscript (role-tagged tail):\n%s",
		base, t, inv, transcript)
}

// renderTranscript turns the exported conversation into a turn-tagged
// tail: user-text messages bump the turn counter (source_turn points
// back at these), tool results stay out (they carry no conversational
// turn and would drown the tail), each message clips to msgClipRunes and
// the whole render keeps only the last budget runes.
func renderTranscript(msgs []anthropic.BetaMessageParam, budget int) string {
	type line struct {
		turn int
		role string
		text string
	}
	var lines []line
	turn := 0
	for _, m := range msgs {
		role := string(m.Role)
		var parts []string
		hasUserText := false
		for _, blk := range m.Content {
			switch {
			case blk.OfText != nil && strings.TrimSpace(blk.OfText.Text) != "":
				parts = append(parts, strings.TrimSpace(blk.OfText.Text))
				if role == string(anthropic.BetaMessageParamRoleUser) {
					hasUserText = true
				}
			case blk.OfToolUse != nil:
				parts = append(parts, "[tool "+blk.OfToolUse.Name+"]")
			case blk.OfImage != nil:
				parts = append(parts, "(image)")
			}
			// OfToolResult: skipped on purpose — see doc comment.
		}
		if len(parts) == 0 {
			continue
		}
		if hasUserText {
			turn++
		}
		lines = append(lines, line{
			turn: turn,
			role: role,
			text: clipRunes(strings.Join(parts, " "), msgClipRunes),
		})
	}
	if len(lines) == 0 {
		return ""
	}
	// Keep the tail: walk back until the budget is spent.
	used := 0
	start := len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		used += len([]rune(lines[i].text)) + 32
		if used > budget && i < len(lines)-1 {
			break
		}
		start = i
	}
	var b strings.Builder
	if start > 0 {
		b.WriteString("(earlier turns elided)\n")
	}
	for _, l := range lines[start:] {
		fmt.Fprintf(&b, "[turn %d] %s: %s\n", l.turn, l.role, l.text)
	}
	return strings.TrimSpace(b.String())
}

// ParseDiff pulls the diff object out of the model's reply, tolerating
// code fences and stray prose (same tolerance as pkg/agent's fact
// parser). A reply that yields no JSON is an error — the caller audits
// it as D1 and skips the round; it never invents a diff.
func ParseDiff(reply string) (Diff, error) {
	s := strings.TrimSpace(reply)
	if s == "" {
		return Diff{}, errors.New("empty reply")
	}
	s = strings.ReplaceAll(s, "```json", "")
	s = strings.ReplaceAll(s, "```", "")
	// A bare ops array (older/looser framing) still counts — check before
	// the object path, since an array also contains '{'.
	if strings.HasPrefix(s, "[") {
		var ops []Op
		if err := json.Unmarshal([]byte(s), &ops); err == nil {
			return Diff{Ops: ops}, nil
		}
	}
	if start := strings.IndexByte(s, '{'); start >= 0 {
		if end := strings.LastIndexByte(s, '}'); end > start {
			var d Diff
			if err := json.Unmarshal([]byte(s[start:end+1]), &d); err != nil {
				return Diff{}, fmt.Errorf("bad diff JSON: %w", err)
			}
			return d, nil
		}
	}
	return Diff{}, errors.New("no JSON object in reply")
}
