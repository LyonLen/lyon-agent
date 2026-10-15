package agent

// Outbound-history validation — the messages[69].role incident (2026-10-15).
// The runner can accumulate an entry with no role and no content (an empty
// stream response records as BetaMessage.ToParam() with Role "" and a
// non-nil empty Content, which the SDK's omitzero tags render as
// {"content":[]} — no role key at all), and one such entry fails EVERY later
// request with 400 invalid_request_error. Three layers repair or drop
// malformed entries instead of re-sending the identical payload:
//
//   - sanitizeMessages runs at the typed seams (buildHistory, the prompt-end
//     sweep, Restore/Export) so the poisoned entry never reaches disk or the
//     next runner.
//   - apiGuard validates the wire body before EVERY POST; on a 400 citing
//     messages[N].role/.content it repairs that entry and resends the
//     corrected payload — never the identical one.
//   - kickDiagnose (cmd/lyon) falls back to a local post-mortem so the
//     diagnosis step always completes with checkable conditions.
//
// The rules are the API's own: role must be user/assistant/system, content
// must carry at least one block, and user/assistant turns must alternate.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"

	"lyon-agent/pkg/hooks"
	"lyon-agent/pkg/logx"
)

// ---------------------------------------------------------------------------
// Typed seam: []anthropic.BetaMessageParam
// ---------------------------------------------------------------------------

// validRole reports whether r is a role the Messages API accepts.
func validRole(r anthropic.BetaMessageParamRole) bool {
	switch r {
	case anthropic.BetaMessageParamRoleUser,
		anthropic.BetaMessageParamRoleAssistant,
		anthropic.BetaMessageParamRoleSystem:
		return true
	}
	return false
}

// sanitizeMessages repairs or drops entries the API would reject: empty
// content is dropped (an entry with no blocks carries nothing to preserve),
// an invalid role is rewritten from the neighbours, and adjacent same-role
// user/assistant turns are merged so a drop cannot break alternation.
// Returns the cleaned slice and whether anything changed; on a clean input
// the original slice comes back untouched (no allocation, byte-identical).
func sanitizeMessages(msgs []anthropic.BetaMessageParam) ([]anthropic.BetaMessageParam, bool) {
	if len(msgs) == 0 {
		return msgs, false
	}
	out := make([]anthropic.BetaMessageParam, 0, len(msgs))
	changed := false
	for _, m := range msgs {
		if len(m.Content) == 0 {
			changed = true // empty entry: drop, there is nothing to repair
			continue
		}
		if !validRole(m.Role) {
			m.Role = inferRole(out)
			changed = true
		}
		out = append(out, m)
	}
	if merged, did := mergeAdjacent(out); did {
		out, changed = merged, true
	}
	if !changed {
		return msgs, false
	}
	return out, true
}

// inferRole answers the role an entry between kept neighbours most likely
// carried: the opposite of the nearest preceding user/assistant turn,
// defaulting to user when nothing precedes it.
func inferRole(kept []anthropic.BetaMessageParam) anthropic.BetaMessageParamRole {
	for i := len(kept) - 1; i >= 0; i-- {
		switch kept[i].Role {
		case anthropic.BetaMessageParamRoleUser:
			return anthropic.BetaMessageParamRoleAssistant
		case anthropic.BetaMessageParamRoleAssistant:
			return anthropic.BetaMessageParamRoleUser
		}
	}
	return anthropic.BetaMessageParamRoleUser
}

// mergeAdjacent folds consecutive same-role user/assistant messages into the
// first one (content concatenated in order — a tool_result stays ahead of
// the text that follows it). System messages are exempt: they may sit
// anywhere without breaking alternation. Never mutates the input slice or
// the Content backing arrays it shares with the live runner history.
func mergeAdjacent(msgs []anthropic.BetaMessageParam) ([]anthropic.BetaMessageParam, bool) {
	if len(msgs) < 2 {
		return msgs, false
	}
	changed := false
	out := make([]anthropic.BetaMessageParam, 0, len(msgs))
	for _, m := range msgs {
		if n := len(out); n > 0 && m.Role == out[n-1].Role &&
			(m.Role == anthropic.BetaMessageParamRoleUser ||
				m.Role == anthropic.BetaMessageParamRoleAssistant) {
			prev := out[n-1]
			// Copy the header's Content before appending: the element came
			// from runner.Params (or seed) and shares its backing array.
			prev.Content = append([]anthropic.BetaContentBlockParamUnion(nil), prev.Content...)
			prev.Content = append(prev.Content, m.Content...)
			out[n-1] = prev
			changed = true
			continue
		}
		out = append(out, m)
	}
	if !changed {
		return msgs, false
	}
	return out, true
}

// auditHistoryRepair files one audit line when a seam had to repair the
// history — the observable trail for the 抢锁/日志 requirement: every repair
// is greppable in ~/.lyon/audit.
func (s *Session) auditHistoryRepair(where string) {
	if s.eng == nil || s.eng.hooks == nil {
		return
	}
	s.eng.hooks.Fire(hooks.Record{
		Event:  hooks.EvHistoryRepair,
		Detail: where,
	})
}

// ---------------------------------------------------------------------------
// Wire seam: JSON body of every POST /v1/messages
// ---------------------------------------------------------------------------

// guardPreflight validates every message of an Anthropic-protocol POST body
// and repairs or drops malformed entries. Returns the (possibly new) body
// and whether anything changed. Bodies that do not parse as JSON, carry no
// messages array, or are already valid pass through byte-identical — the
// common case never re-encodes.
func guardPreflight(body []byte) ([]byte, bool) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return body, false
	}
	raw, ok := top["messages"]
	if !ok {
		return body, false
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return body, false
	}
	fixed, changed := sanitizeWireMessages(arr)
	if !changed {
		return body, false
	}
	m, ok := jsonRaw(fixed)
	if !ok {
		return body, false
	}
	top["messages"] = m
	out, err := json.Marshal(top)
	if err != nil {
		return body, false
	}
	return out, true
}

// sanitizeWireMessages mirrors sanitizeMessages at the JSON level.
func sanitizeWireMessages(arr []json.RawMessage) ([]json.RawMessage, bool) {
	if len(arr) == 0 {
		return arr, false
	}
	changed := false
	out := make([]json.RawMessage, 0, len(arr))
	for _, raw := range arr {
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) != nil {
			changed = true // not even an object — unrecoverable, drop
			continue
		}
		if !wireContentOK(m) {
			changed = true // empty/missing content carries nothing to preserve
			continue
		}
		if !wireRoleOK(m) {
			role := inferWireRole(out)
			b, ok := jsonRaw(role)
			if !ok {
				out = append(out, raw)
				continue
			}
			m["role"] = b
			mb, ok := jsonRaw(m)
			if !ok {
				out = append(out, raw)
				continue
			}
			out = append(out, mb)
			changed = true
			continue
		}
		out = append(out, raw)
	}
	if merged, did := mergeWireAdjacent(out); did {
		out, changed = merged, true
	}
	if !changed {
		return arr, false
	}
	return out, true
}

// wireRoleOK reports whether the entry carries a role the API accepts.
func wireRoleOK(m map[string]json.RawMessage) bool {
	r, ok := wireRoleString(m)
	if !ok {
		return false
	}
	return r == "user" || r == "assistant" || r == "system"
}

func wireRoleString(m map[string]json.RawMessage) (string, bool) {
	var r string
	if err := json.Unmarshal(m["role"], &r); err != nil {
		return "", false
	}
	return r, true
}

// wireRoleOf parses an entry's role straight from its raw JSON.
func wireRoleOf(raw json.RawMessage) (string, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return "", false
	}
	return wireRoleString(m)
}

// wireContentOK reports whether content is a non-empty string or array —
// exactly what the API requires of every message.
func wireContentOK(m map[string]json.RawMessage) bool {
	raw, ok := m["content"]
	if !ok || len(raw) == 0 {
		return false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s) != ""
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil {
		return len(arr) > 0
	}
	return false // null / number / object — not a legal content
}

// inferWireRole mirrors inferRole over raw entries.
func inferWireRole(kept []json.RawMessage) string {
	for i := len(kept) - 1; i >= 0; i-- {
		r, ok := wireRoleOf(kept[i])
		if !ok {
			continue
		}
		switch r {
		case "user":
			return "assistant"
		case "assistant":
			return "user"
		}
	}
	return "user"
}

// mergeWireAdjacent mirrors mergeAdjacent over raw entries. String content
// is wrapped into a text block before concatenation (the API accepts either
// form, not a mixture inside one message).
func mergeWireAdjacent(arr []json.RawMessage) ([]json.RawMessage, bool) {
	changed := false
	out := make([]json.RawMessage, 0, len(arr))
	for _, raw := range arr {
		if n := len(out); n > 0 {
			pr, ok1 := wireRoleOf(out[n-1])
			cr, ok2 := wireRoleOf(raw)
			if ok1 && ok2 && pr == cr && (pr == "user" || pr == "assistant") {
				var pm, cm map[string]json.RawMessage
				if json.Unmarshal(out[n-1], &pm) == nil && json.Unmarshal(raw, &cm) == nil {
					pa, a1 := wireContentArray(pm)
					ca, a2 := wireContentArray(cm)
					if a1 && a2 {
						combined := make([]json.RawMessage, 0, len(pa)+len(ca))
						combined = append(combined, pa...)
						combined = append(combined, ca...)
						if b, ok := jsonRaw(combined); ok {
							pm["content"] = b
							if nb, ok := jsonRaw(pm); ok {
								out[n-1] = nb
								changed = true
								continue
							}
						}
					}
				}
			}
		}
		out = append(out, raw)
	}
	if !changed {
		return arr, false
	}
	return out, true
}

// wireContentArray returns content as an array of blocks: the array form
// verbatim, a string form wrapped into one text block, false otherwise.
func wireContentArray(m map[string]json.RawMessage) ([]json.RawMessage, bool) {
	raw, ok := m["content"]
	if !ok {
		return nil, false
	}
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		return arr, true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		blk, ok := jsonRaw(map[string]any{"type": "text", "text": s})
		if !ok {
			return nil, false
		}
		return []json.RawMessage{blk}, true
	}
	return nil, false
}

// jsonRaw marshals v, reporting ok — a failed re-encode makes the guard keep
// the original entry; it never turns a body into a worse one.
func jsonRaw(v any) (json.RawMessage, bool) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	return b, true
}

// ---------------------------------------------------------------------------
// Reactive layer: 400 → repair the cited entry → resend corrected
// ---------------------------------------------------------------------------

// reCitedField matches the server's per-field citations — messages[69].role
// or messages.69.content — but NOT deeper paths such as messages[69].content[0]
// (a malformed block is beyond this guard's role/content mandate, so those
// 400s pass through untouched).
var reCitedField = regexp.MustCompile(`messages(?:\[(\d+)\]|\.(\d+))\.(role|content)([^\[]|$)`)

// citedMessageField extracts the first repairable citation from an error
// message: which message index, which field.
func citedMessageField(msg string) (field string, idx int, ok bool) {
	m := reCitedField.FindStringSubmatch(msg)
	if m == nil {
		return "", 0, false
	}
	s := m[1]
	if s == "" {
		s = m[2]
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return "", 0, false
	}
	return m[3], n, true
}

// citedRepairable reports whether a raw 400 body asks for a repair this
// guard can perform: an invalid_request_error (or a typeless error) that
// cites messages[N].role or messages[N].content at the root.
func citedRepairable(ebody []byte) (field string, idx int, ok bool) {
	var env struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(ebody, &env) != nil {
		return "", 0, false
	}
	if env.Error.Type != "" && env.Error.Type != "invalid_request_error" {
		return "", 0, false
	}
	return citedMessageField(env.Error.Message)
}

// repairCitedWire applies the server's own diagnosis to the body just sent.
// The cited entry is dropped when its content is empty, its role is
// rewritten from the preceding neighbours when the server rejects the role,
// and it is dropped when the server rejects content this guard thought
// fine. The alternation merge closes any hole a drop leaves, so the resend
// both differs from the original and stays a legal conversation. Returns
// the new body and whether it actually changed.
func repairCitedWire(body []byte, idx int, field string) ([]byte, bool) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return body, false
	}
	raw, ok := top["messages"]
	if !ok {
		return body, false
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil || idx < 0 || idx >= len(arr) {
		return body, false
	}
	var m map[string]json.RawMessage
	parseErr := json.Unmarshal(arr[idx], &m)
	changed := false
	switch {
	case parseErr != nil || !wireContentOK(m):
		// Nothing worth keeping — remove the entry wholesale.
		arr = append(arr[:idx], arr[idx+1:]...)
		changed = true
	case field == "role":
		cur, _ := wireRoleString(m)
		next := inferWireRole(arr[:idx])
		if next == cur {
			return body, false // cannot differ from what already failed
		}
		if b, ok := jsonRaw(next); ok {
			m["role"] = b
			if mb, ok := jsonRaw(m); ok {
				arr[idx] = mb
				changed = true
			}
		}
	default: // field == "content" and the guard saw content: server disagrees → drop
		arr = append(arr[:idx], arr[idx+1:]...)
		changed = true
	}
	if !changed {
		return body, false
	}
	if merged, did := mergeWireAdjacent(arr); did {
		arr = merged
	}
	messages, ok := jsonRaw(arr)
	if !ok {
		return body, false
	}
	top["messages"] = messages
	out, err := json.Marshal(top)
	if err != nil {
		return body, false
	}
	if bytes.Equal(out, body) {
		return body, false
	}
	return out, true
}

// ---------------------------------------------------------------------------
// The guard itself: a RoundTripper in front of every Messages API call
// ---------------------------------------------------------------------------

// guardMaxRepairs bounds the 400 → repair → resend loop per request: the
// server cites one entry at a time, and each round must prove it changed
// the payload before another attempt is made.
const guardMaxRepairs = 3

// apiGuard wraps the transport that carries every Anthropic POST: it
// preflights the message array before the request leaves the process and
// recovers from 400s that cite a malformed message by repairing the entry
// and resending the corrected body. Every decision lands in the audit log.
type apiGuard struct {
	inner http.RoundTripper
	hooks *hooks.Manager // may be nil (tests)
}

// RoundTrip implements http.RoundTripper.
func (g *apiGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodPost || req.Body == nil ||
		req.URL == nil || !strings.Contains(req.URL.Path, "/v1/messages") {
		return g.inner.RoundTrip(req)
	}
	body, err := io.ReadAll(req.Body)
	req.Body.Close()
	if err != nil {
		return nil, err
	}
	cur, changed := guardPreflight(body)
	if changed {
		g.audit("preflight: malformed messages repaired before POST", false)
	}
	sent := [][]byte{cur}
	resp, err := g.send(req, cur)
	if err != nil {
		logx.Warn("apiguard", "messages api transport error", "url", req.URL.String(), "err", err)
		return nil, err
	}
	if resp.StatusCode != http.StatusBadRequest {
		return resp, nil
	}

	// 400: read the error once so the response can be handed back intact
	// when it turns out to be beyond this guard's mandate.
	ebody, rerr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if rerr != nil {
		resp.Body = io.NopCloser(bytes.NewReader(nil))
		return resp, nil
	}
	last, lastBody := resp, ebody
	for i := 0; i < guardMaxRepairs; i++ {
		field, idx, ok := citedRepairable(lastBody)
		if !ok {
			break
		}
		next, ok := repairCitedWire(cur, idx, field)
		if !ok || bytes.Equal(next, cur) {
			break
		}
		dupe := false
		for _, s := range sent {
			if bytes.Equal(s, next) {
				dupe = true
				break
			}
		}
		if dupe {
			break // never resend a payload this request already sent
		}
		g.audit(fmt.Sprintf("400: repaired messages[%d].%s and resent (attempt %d)", idx, field, i+1), false)
		r2, err2 := g.send(req, next)
		if err2 != nil {
			logx.Warn("apiguard", "messages api transport error during repair resend", "url", req.URL.String(), "err", err2)
			return r2, err2
		}
		sent = append(sent, next)
		cur = next
		if r2.StatusCode != http.StatusBadRequest {
			return r2, nil // corrected payload accepted (or a different error to surface)
		}
		nb, e := io.ReadAll(r2.Body)
		r2.Body.Close()
		if e != nil {
			r2.Body = io.NopCloser(bytes.NewReader(nil))
			return r2, nil
		}
		last, lastBody = r2, nb
	}
	// Repairs exhausted (or the 400 is something else): return the last
	// 400 with its body restored so the SDK reports it to the caller.
	last.Body = io.NopCloser(bytes.NewReader(lastBody))
	snippet := string(lastBody)
	if len(snippet) > 400 {
		snippet = snippet[:400]
	}
	logx.Warn("apiguard", "messages api 400 not repaired by guard", "attempts", len(sent), "body", snippet)
	return last, nil
}

// send dispatches a copy of req carrying body — fresh readers for both
// Body and GetBody so SDK-level retries see the repaired payload too.
func (g *apiGuard) send(req *http.Request, body []byte) (*http.Response, error) {
	r2 := req.Clone(req.Context())
	r2.Body = io.NopCloser(bytes.NewReader(body))
	r2.ContentLength = int64(len(body))
	r2.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	return g.inner.RoundTrip(r2)
}

// audit files the decision under LYON_AUDIT and echoes it on stderr when
// LYON_DEBUG is set — the visible trail for the 日志机制 requirement.
func (g *apiGuard) audit(detail string, isErr bool) {
	if os.Getenv("LYON_DEBUG") != "" {
		log.New(os.Stderr, "[guard] ", 0).Print(detail)
	}
	if g.hooks != nil {
		g.hooks.Fire(hooks.Record{
			Event:   hooks.EvAPIGuard,
			Detail:  detail,
			IsError: isErr,
		})
	}
}
