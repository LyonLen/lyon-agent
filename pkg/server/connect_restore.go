package server

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"
	"github.com/anthropics/anthropic-sdk-go"

	lyonv1 "lyon-agent/gen/go/lyon/v1"
	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/tools"
)

// RestoreSession re-opens a stored snapshot as a live session (web plan
// Phase A — "历史点不进来"). The history never round-trips a separate
// GetHistory RPC: synthSnapshotEvents turns it into hub events and
// injectAll rings them, so the client's ordinary
// WatchEvents(from_seq=-1) replays the whole conversation and keeps
// going live — one wire shape for fresh and restored sessions alike.
func (c *sessionService) RestoreSession(_ context.Context, req *connect.Request[lyonv1.RestoreSessionRequest]) (*connect.Response[lyonv1.RestoreSessionResponse], error) {
	path, err := snapshotPathInsideHome(req.Msg.Path)
	if err != nil {
		return nil, err
	}
	snap, err := agent.LoadSnapshotFrom(path)
	if err != nil {
		// The path was legal; the bytes weren't — corrupt or unreadable
		// snapshot content is data damage, not a request the caller can
		// fix by editing arguments.
		return nil, connect.NewError(connect.CodeDataLoss, err)
	}
	if snap == nil {
		// Missing or empty (LoadSnapshotFrom's nil,nil): nothing to resume.
		return nil, connect.NewError(connect.CodeNotFound,
			fmt.Errorf("no snapshot at %s", path))
	}

	// Idempotent restore: the same snapshot maps to ONE live session.
	// Double-clicking a SAVED row focuses that session instead of minting
	// a fresh copy per click (the "四个会话都是同一个" bug).
	c.s.restoreMu.Lock()
	defer c.s.restoreMu.Unlock()
	if id, ok := c.s.restored[path]; ok {
		if ref, err := c.s.store.Ref(id); err == nil {
			return connect.NewResponse(&lyonv1.RestoreSessionResponse{
				Session:          protoSession(c.s, ref),
				RestoredMessages: int32(len(snap.Messages)),
			}), nil
		}
		delete(c.s.restored, path) // session was deleted meanwhile
	}

	sess := c.s.eng.NewSession(c.s.obs)
	// No in-flight prompt on a brand-new session — Restore's settle
	// precondition is trivially met.
	sess.Restore(*snap)
	id, err := c.s.store.Create(sess)
	if err != nil {
		sess.Close()
		return nil, rpcErr(err)
	}
	h, err := c.s.store.Hub(id)
	if err != nil {
		// Just created; only a reap could have raced this far.
		return nil, rpcErr(err)
	}
	h.injectAll(synthSnapshotEvents(snap.Messages))
	c.s.restored[path] = id

	ref, err := c.s.store.Ref(id)
	if err != nil {
		return nil, rpcErr(err)
	}
	return connect.NewResponse(&lyonv1.RestoreSessionResponse{
		Session:          protoSession(c.s, ref),
		RestoredMessages: int32(len(snap.Messages)),
	}), nil
}

// snapshotPathInsideHome validates a client-supplied snapshot path by
// lexical containment inside $LYON_HOME/sessions — the dashDay precedent:
// traversal fails InvalidArgument before anything touches disk. The
// expected input is a path ListSnapshots itself handed out, so same-root
// comparison is enough; symlink games would only reach other snapshots
// the caller can already list.
func snapshotPathInsideHome(p string) (string, error) {
	bad := func(why string) (string, error) {
		return "", connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("snapshot path %q: %s", p, why))
	}
	if strings.TrimSpace(p) == "" {
		return "", connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("path is required"))
	}
	home, err := dashHome()
	if err != nil {
		return "", connect.NewError(connect.CodeInternal, err)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return bad("not a usable path")
	}
	rel, err := filepath.Rel(filepath.Join(home, "sessions"), abs)
	if err != nil || rel == "." || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return bad("outside $LYON_HOME/sessions")
	}
	if !strings.HasSuffix(rel, ".json") {
		return bad("not a snapshot file (.json)")
	}
	return abs, nil
}

// synthUse is what a stored tool_use remembers for its paired result.
type synthUse struct {
	name  string
	input json.RawMessage
}

// synthSnapshotEvents replays stored history as hub events — the web
// transcript's entire view of a restored conversation. Ordering mirrors a
// live run: assistant tool_use opens a card (the snapshot carries the
// full input, which live streams only learn at exec time), and the paired
// user tool_result drives exec start → output → end so the card settles
// into done/failed. Where the agent contract has no event type
// (user_message) the emission rides the type_name forward-compat channel.
// Un-renderable blocks (images, documents, system rows) are skipped — the
// live stream shows no row for them either.
func synthSnapshotEvents(msgs []anthropic.BetaMessageParam) []agent.Event {
	var out []agent.Event
	uses := map[string]synthUse{}      // tool_use id → pending execution
	serverNames := map[string]string{} // server tool_use id → name
	nextExec := uint64(1)

	marshal := func(v any) json.RawMessage {
		b, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		return b
	}

	for _, m := range msgs {
		switch m.Role {
		case anthropic.BetaMessageParamRoleUser:
			for _, b := range m.Content {
				switch {
				case b.OfText != nil:
					// Memory injections ride the user message as a leading
					// text block; they are context, not conversation — never
					// replay them as a user_message.
					if b.OfText.Text != "" && !tools.IsInjection(b.OfText.Text) {
						out = append(out, agent.Event{Type: evUserMessage, Text: b.OfText.Text})
					}
				case b.OfToolResult != nil:
					out = append(out, synthToolResultExec(b.OfToolResult, uses, &nextExec)...)
				}
			}
		case anthropic.BetaMessageParamRoleAssistant:
			for _, b := range m.Content {
				switch {
				case b.OfThinking != nil:
					if b.OfThinking.Thinking != "" {
						out = append(out, agent.Event{Type: agent.EventThinkingDelta, Thinking: b.OfThinking.Thinking})
					}
				case b.OfText != nil:
					if b.OfText.Text != "" {
						out = append(out, agent.Event{Type: agent.EventTextDelta, Text: b.OfText.Text})
					}
				case b.OfToolUse != nil:
					tu := b.OfToolUse
					input := marshal(tu.Input)
					uses[tu.ID] = synthUse{name: tu.Name, input: input}
					out = append(out, agent.Event{
						Type:     agent.EventToolUseStart,
						ToolID:   tu.ID,
						ToolName: tu.Name,
						Input:    input,
					})
				case b.OfServerToolUse != nil:
					st := b.OfServerToolUse
					name := string(st.Name)
					serverNames[st.ID] = name
					out = append(out, agent.Event{
						Type:     agent.EventToolUseStart,
						ToolID:   st.ID,
						ToolName: name,
						Input:    marshal(st.Input),
						Server:   true,
					})
				case b.OfWebSearchToolResult != nil:
					sr := b.OfWebSearchToolResult
					name := serverNames[sr.ToolUseID]
					if name == "" {
						name = "web_search"
					}
					out = append(out, agent.Event{
						Type:     agent.EventServerToolResult,
						ToolID:   sr.ToolUseID,
						ToolName: name,
						Input:    marshal(sr.Content),
						IsError:  sr.Content.OfError != nil,
					})
				case b.OfWebFetchToolResult != nil:
					fr := b.OfWebFetchToolResult
					name := serverNames[fr.ToolUseID]
					if name == "" {
						name = "web_fetch"
					}
					out = append(out, agent.Event{
						Type:     agent.EventServerToolResult,
						ToolID:   fr.ToolUseID,
						ToolName: name,
						Input:    marshal(fr.Content),
					})
				}
			}
		}
	}
	return out
}

// synthToolResultExec turns one stored tool_result into the exec lifecycle
// its paired tool_use card is waiting on: start (with the remembered
// input) → output → end. Results without a paired tool_use (history
// truncated mid-execution) are skipped rather than orphaned. Output is
// clipped like the TUI's resume view (200 runes); error results carry the
// text on the error line instead.
func synthToolResultExec(tr *anthropic.BetaToolResultBlockParam, uses map[string]synthUse, next *uint64) []agent.Event {
	use, ok := uses[tr.ToolUseID]
	if !ok {
		return nil
	}
	delete(uses, tr.ToolUseID)
	execID := *next
	*next++
	text := toolResultText(tr)
	out := []agent.Event{
		{
			Type:     agent.EventToolExecStart,
			ToolID:   tr.ToolUseID,
			ExecID:   execID,
			ToolName: use.name,
			Input:    use.input,
		},
	}
	if tr.IsError.Value {
		out = append(out, agent.Event{
			Type:     agent.EventToolExecEnd,
			ExecID:   execID,
			ToolName: use.name,
			IsError:  true,
			Err:      clipRunes(text, 400),
		})
		return out
	}
	out = append(out,
		agent.Event{Type: agent.EventToolExecOutput, ExecID: execID, ToolName: use.name, Text: clipRunes(text, 200)},
		agent.Event{Type: agent.EventToolExecEnd, ExecID: execID, ToolName: use.name},
	)
	return out
}

// toolResultText concatenates the text parts of a tool_result; images and
// documents contribute no transcript line (matches the live stream).
func toolResultText(tr *anthropic.BetaToolResultBlockParam) string {
	var b strings.Builder
	for _, c := range tr.Content {
		if t := c.OfText; t != nil {
			b.WriteString(t.Text)
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// clipRunes bounds a replayed excerpt to n runes — the TUI's resume view
// truncates tool results the same way.
func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
