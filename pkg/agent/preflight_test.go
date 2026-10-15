package agent

// Tests for the three-layer outbound-history defense (preflight.go):
// the typed seam (sanitizeMessages), the wire seam (guardPreflight /
// repairCitedWire), the RoundTripper guard (apiGuard), and the seam
// call sites (buildHistory, Restore, Export). The incident being
// defended against: an empty stream records {"content":[]} with no role,
// and every later POST then dies with 400 messages[N].role is required.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"lyon-agent/pkg/hooks"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// parseWire returns a wire body's messages array as per-entry maps.
func parseWire(t *testing.T, body []byte) []map[string]json.RawMessage {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatalf("body does not parse: %v\n%s", err, body)
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(top["messages"], &raw); err != nil {
		t.Fatalf("messages array: %v\n%s", err, body)
	}
	out := make([]map[string]json.RawMessage, len(raw))
	for i, r := range raw {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(r, &m); err != nil {
			t.Fatalf("messages[%d] not an object: %v", i, err)
		}
		out[i] = m
	}
	return out
}

// assertWireClean fails t when any message lacks an API-legal role or content.
func assertWireClean(t *testing.T, body []byte) {
	t.Helper()
	for i, m := range parseWire(t, body) {
		if !wireRoleOK(m) {
			t.Errorf("messages[%d] has no legal role: %s\n%s", i, m["role"], body)
		}
		if !wireContentOK(m) {
			t.Errorf("messages[%d] has empty content: %s\n%s", i, m["content"], body)
		}
	}
}

// textOf joins a message's text blocks (string content included).
func textOf(t *testing.T, m map[string]json.RawMessage) string {
	t.Helper()
	raw, ok := m["content"]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range blocks {
		var txt string
		if json.Unmarshal(blk["text"], &txt) == nil {
			b.WriteString(txt)
		}
	}
	return b.String()
}

// guardStub is a scripted http.RoundTripper: it records every request body
// and answers from reply(n), the 1-based call number. mockapi cannot script
// 400 envelopes, so the guard's reactive layer is tested against this.
type guardStub struct {
	bodies [][]byte
	reply  func(n int) (status int, body string)
}

func (s *guardStub) RoundTrip(req *http.Request) (*http.Response, error) {
	var b []byte
	if req.Body != nil {
		b, _ = io.ReadAll(req.Body)
		req.Body.Close()
	}
	s.bodies = append(s.bodies, b)
	code, body := s.reply(len(s.bodies))
	return &http.Response{
		StatusCode: code,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}, nil
}

// postMessages builds a POST /v1/messages request carrying body.
func postMessages(t *testing.T, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://api.example.com/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// okReply answers 200 for every call.
func okReply(int) (int, string) { return http.StatusOK, `{"ok":true}` }

// ---------------------------------------------------------------------------
// typed seam: sanitizeMessages
// ---------------------------------------------------------------------------

// TestSanitizeMessagesCleanPassThrough: a legal history comes back as the
// SAME slice — the common case allocates nothing and cannot drift.
func TestSanitizeMessagesCleanPassThrough(t *testing.T) {
	in := []anthropic.BetaMessageParam{
		anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("q")),
		{
			Role:    anthropic.BetaMessageParamRoleAssistant,
			Content: []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaTextBlock("a")},
		},
	}
	out, changed := sanitizeMessages(in)
	if changed {
		t.Fatal("clean input reported as changed")
	}
	if len(out) == 0 || &out[0] != &in[0] {
		t.Fatal("clean input must come back as the original slice, not a copy")
	}
}

// TestSanitizeMessagesIncidentShape: the exact messages[69] shape —
// assistant tool_use, user tool_result, the role-less empty ghost, then the
// next user turn. The ghost is dropped and the two users merge with the
// tool_result staying first; the input slice is never mutated.
func TestSanitizeMessagesIncidentShape(t *testing.T) {
	ghost := anthropic.BetaMessageParam{Content: []anthropic.BetaContentBlockParamUnion{}}
	in := []anthropic.BetaMessageParam{
		{
			Role: anthropic.BetaMessageParamRoleAssistant,
			Content: []anthropic.BetaContentBlockParamUnion{
				{OfThinking: &anthropic.BetaThinkingBlockParam{Thinking: "hmm", Signature: "sig"}},
				{OfToolUse: &anthropic.BetaToolUseBlockParam{ID: "toolu_1", Name: "run_shell", Input: map[string]any{"command": "ls"}}},
			},
		},
		anthropic.NewBetaUserMessage(anthropic.NewBetaToolResultBlock("toolu_1", "file.txt", false)),
		ghost,
		anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("requirements text")),
	}
	out, changed := sanitizeMessages(in)
	if !changed {
		t.Fatal("poisoned history reported as clean")
	}
	if len(out) != 2 {
		t.Fatalf("want 2 messages after drop+merge, got %d: %#v", len(out), out)
	}
	if out[0].Role != anthropic.BetaMessageParamRoleAssistant || len(out[0].Content) != 2 {
		t.Fatalf("assistant turn damaged: role=%s content=%d", out[0].Role, len(out[0].Content))
	}
	merged := out[1]
	if merged.Role != anthropic.BetaMessageParamRoleUser || len(merged.Content) != 2 {
		t.Fatalf("merged user turn: role=%s content=%d", merged.Role, len(merged.Content))
	}
	if merged.Content[0].OfToolResult == nil {
		t.Fatal("tool_result must stay ahead of the text that follows it")
	}
	if merged.Content[1].OfText == nil || merged.Content[1].OfText.Text != "requirements text" {
		t.Fatalf("merged text lost: %+v", merged.Content[1])
	}
	// The live history was not written through.
	if len(in) != 4 || in[2].Role != "" || len(in[1].Content) != 1 {
		t.Fatalf("sanitizeMessages mutated its input: %d entries", len(in))
	}
}

// TestSanitizeMessagesRoleInference: a content-bearing but role-less entry
// takes the role opposite its nearest kept predecessor; a leading one
// defaults to user. Then alternation is restored by the merge.
func TestSanitizeMessagesRoleInference(t *testing.T) {
	orphan := anthropic.BetaMessageParam{Content: []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaTextBlock("orphan")}}
	in := []anthropic.BetaMessageParam{
		anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("q")),
		orphan,
		{
			Role:    anthropic.BetaMessageParamRoleAssistant,
			Content: []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaTextBlock("a")},
		},
	}
	out, changed := sanitizeMessages(in)
	if !changed {
		t.Fatal("role-less entry reported as clean")
	}
	// orphan infers assistant (after a user) and merges with the next assistant.
	if len(out) != 2 {
		t.Fatalf("want 2 messages, got %d: role0=%s role1=%s", len(out), out[0].Role, out[1].Role)
	}
	if out[1].Role != anthropic.BetaMessageParamRoleAssistant {
		t.Fatalf("orphan role = %s, want assistant", out[1].Role)
	}

	// Leading orphan: no predecessor → user.
	lead := []anthropic.BetaMessageParam{
		{Content: []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaTextBlock("first")}},
		anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("second")),
	}
	out2, _ := sanitizeMessages(lead)
	if len(out2) != 1 || out2[0].Role != anthropic.BetaMessageParamRoleUser {
		t.Fatalf("leading orphan: len=%d role=%s", len(out2), out2[0].Role)
	}
	if len(out2[0].Content) != 2 {
		t.Fatalf("merged leading content: %d blocks", len(out2[0].Content))
	}
}

// TestSanitizeMessagesDropsEmptyContentOnly: an entry with content but no
// role survives (repaired); an entry with a role but no content is dropped —
// there is nothing to preserve — and the drop closes the alternation hole
// (the two users around it merge into one).
func TestSanitizeMessagesDropsEmptyContentOnly(t *testing.T) {
	in := []anthropic.BetaMessageParam{
		anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("q")),
		{Role: anthropic.BetaMessageParamRoleAssistant}, // role, no blocks
		anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("a")),
	}
	out, changed := sanitizeMessages(in)
	if !changed {
		t.Fatal("empty-content entry reported as clean")
	}
	if len(out) != 1 {
		t.Fatalf("drop must leave one merged user turn, got %d", len(out))
	}
	for i, m := range out {
		if !validRole(m.Role) || len(m.Content) == 0 {
			t.Fatalf("out[%d] still malformed: role=%q content=%d", i, m.Role, len(m.Content))
		}
	}
	if len(out[0].Content) != 2 {
		t.Fatalf("merged turn should keep both texts: %d blocks", len(out[0].Content))
	}
}

// ---------------------------------------------------------------------------
// wire seam: guardPreflight
// ---------------------------------------------------------------------------

// TestGuardPreflightHealthyByteIdentical: a legal body is passed through
// without re-encoding — the guard can never churn a good payload.
func TestGuardPreflightHealthyByteIdentical(t *testing.T) {
	body := `{"model":"m","max_tokens":16,"messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":[{"type":"text","text":"yo"}]},` +
		`{"role":"user","content":"again"}]}`
	out, changed := guardPreflight([]byte(body))
	if changed {
		t.Fatal("healthy body reported as changed")
	}
	if string(out) != body {
		t.Fatalf("healthy body re-encoded:\n got %s\nwant %s", out, body)
	}
}

// TestGuardPreflightRepairsIncidentBody: the messages[69] wire shape —
// {"content":[]} between two users — is dropped and the hole closed by a
// merge before the body ever leaves the process.
func TestGuardPreflightRepairsIncidentBody(t *testing.T) {
	body := `{"model":"m","messages":[` +
		`{"role":"assistant","content":[{"type":"text","text":"done"}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]},` +
		`{"content":[]},` +
		`{"role":"user","content":"requirements"}]}`
	out, changed := guardPreflight([]byte(body))
	if !changed {
		t.Fatal("poisoned body reported as clean")
	}
	msgs := parseWire(t, out)
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages after drop+merge, got %d:\n%s", len(msgs), out)
	}
	assertWireClean(t, out)
	// tool_result first, then the wrapped string content.
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(msgs[1]["content"], &blocks); err != nil || len(blocks) != 2 {
		t.Fatalf("merged content: %v blocks (%v)\n%s", len(blocks), err, out)
	}
	var b0, b1 string
	_ = json.Unmarshal(blocks[0]["type"], &b0)
	_ = json.Unmarshal(blocks[1]["text"], &b1)
	if b0 != "tool_result" || b1 != "requirements" {
		t.Fatalf("merge order: block0 type=%q block1 text=%q", b0, b1)
	}
}

// TestGuardPreflightPassthrough: non-JSON, message-less, and messages-typed
// bodies are the caller's problem — the guard hands them through untouched.
func TestGuardPreflightPassthrough(t *testing.T) {
	cases := map[string]string{
		"not json":      `this is not json`,
		"no messages":   `{"model":"m","max_tokens":16}`,
		"messages null": `{"model":"m","messages":null}`,
		"messages num":  `{"model":"m","messages":42}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			out, changed := guardPreflight([]byte(body))
			if changed {
				t.Fatalf("pass-through case reported as changed: %s", out)
			}
			if string(out) != body {
				t.Fatalf("pass-through case altered:\n got %s\nwant %s", out, body)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// reactive layer: citation parsing + repair
// ---------------------------------------------------------------------------

func TestCitedMessageField(t *testing.T) {
	cases := []struct {
		msg   string
		field string
		idx   int
		ok    bool
	}{
		{"messages[69].role is required", "role", 69, true},
		{"messages.7.content is empty", "content", 7, true},
		{"messages[12].role must be one of: user, assistant", "role", 12, true},
		{"messages[1].content is required when tool_use ids are present", "content", 1, true},
		// deeper paths are beyond the role/content mandate — pass through:
		{"messages[1].content[0].text is invalid", "", 0, false},
		{"max_tokens is too small", "", 0, false},
		{"requests remaining per minute", "", 0, false},
	}
	for _, c := range cases {
		field, idx, ok := citedMessageField(c.msg)
		if ok != c.ok || (ok && (field != c.field || idx != c.idx)) {
			t.Errorf("citedMessageField(%q) = (%q,%d,%v), want (%q,%d,%v)",
				c.msg, field, idx, ok, c.field, c.idx, c.ok)
		}
	}
}

func TestCitedRepairable(t *testing.T) {
	cases := []struct {
		name string
		body string
		ok   bool
	}{
		{"invalid_request with cite", `{"type":"error","error":{"type":"invalid_request_error","message":"messages[69].role is required"}}`, true},
		{"typeless error with cite", `{"error":{"message":"messages[3].content is required"}}`, true},
		{"other error type", `{"type":"error","error":{"type":"overloaded_error","message":"messages[69].role is required"}}`, false},
		{"unciteable invalid_request", `{"error":{"type":"invalid_request_error","message":"max_tokens is too small"}}`, false},
		{"not json", `boom`, false},
		{"empty", `{}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, ok := citedRepairable([]byte(c.body))
			if ok != c.ok {
				t.Fatalf("citedRepairable(%s) = %v, want %v", c.body, ok, c.ok)
			}
		})
	}
}

// TestRepairCitedWire: the repair rules keyed off the server's citation.
func TestRepairCitedWire(t *testing.T) {
	t.Run("empty content dropped and hole closed", func(t *testing.T) {
		body := `{"model":"m","messages":[` +
			`{"role":"user","content":"a"},` +
			`{"content":[]},` +
			`{"role":"user","content":"b"}]}`
		out, ok := repairCitedWire([]byte(body), 1, "role")
		if !ok {
			t.Fatal("repair refused to change the body")
		}
		msgs := parseWire(t, out)
		if len(msgs) != 1 || msgs[0]["role"] == nil {
			t.Fatalf("want one merged user message, got %d:\n%s", len(msgs), out)
		}
		if txt := textOf(t, msgs[0]); !strings.Contains(txt, "a") || !strings.Contains(txt, "b") {
			t.Fatalf("merge lost text: %q", txt)
		}
	})

	t.Run("missing role inferred from predecessor", func(t *testing.T) {
		body := `{"model":"m","messages":[` +
			`{"role":"user","content":"q"},` +
			`{"content":[{"type":"text","text":"orphan"}]}]}`
		out, ok := repairCitedWire([]byte(body), 1, "role")
		if !ok {
			t.Fatal("repair refused to change the body")
		}
		msgs := parseWire(t, out)
		var r string
		_ = json.Unmarshal(msgs[1]["role"], &r)
		if r != "assistant" {
			t.Fatalf("inferred role = %q, want assistant", r)
		}
	})

	t.Run("role rewrite when the server rejects it", func(t *testing.T) {
		body := `{"model":"m","messages":[` +
			`{"role":"user","content":"q"},` +
			`{"role":"system","content":[{"type":"text","text":"persona"}]},` +
			`{"role":"user","content":"next"}]}`
		out, ok := repairCitedWire([]byte(body), 1, "role")
		if !ok {
			t.Fatal("repair refused to change the body")
		}
		msgs := parseWire(t, out)
		if len(msgs) != 3 {
			t.Fatalf("no merge expected: %d messages", len(msgs))
		}
		var r string
		_ = json.Unmarshal(msgs[1]["role"], &r)
		if r != "assistant" {
			t.Fatalf("role = %q, want assistant", r)
		}
	})

	t.Run("no identical resend when repair cannot differ", func(t *testing.T) {
		body := `{"model":"m","messages":[{"role":"user","content":"q"}]}`
		out, ok := repairCitedWire([]byte(body), 0, "role")
		if ok {
			t.Fatalf("repair claims a change the body did not take: %s", out)
		}
		if string(out) != body {
			t.Fatalf("body altered despite ok=false:\n got %s\nwant %s", out, body)
		}
	})

	t.Run("server-rejected content dropped", func(t *testing.T) {
		body := `{"model":"m","messages":[` +
			`{"role":"user","content":"hi"},` +
			`{"role":"assistant","content":[{"type":"text","text":"bad"}]},` +
			`{"role":"user","content":"again"}]}`
		out, ok := repairCitedWire([]byte(body), 1, "content")
		if !ok {
			t.Fatal("repair refused to change the body")
		}
		msgs := parseWire(t, out)
		if len(msgs) != 1 {
			t.Fatalf("drop must trigger the merge: got %d messages", len(msgs))
		}
		// The rejected assistant turn is gone; the two users merged around it.
		if txt := textOf(t, msgs[0]); txt != "hiagain" {
			t.Fatalf("merged text = %q", txt)
		}
	})

	t.Run("index out of range", func(t *testing.T) {
		body := `{"model":"m","messages":[{"role":"user","content":"q"}]}`
		if _, ok := repairCitedWire([]byte(body), 5, "role"); ok {
			t.Fatal("out-of-range index must not change the body")
		}
		if _, ok := repairCitedWire([]byte(body), -1, "role"); ok {
			t.Fatal("negative index must not change the body")
		}
	})

	t.Run("unparseable body", func(t *testing.T) {
		if _, ok := repairCitedWire([]byte(`nope`), 0, "role"); ok {
			t.Fatal("non-JSON body must pass through")
		}
	})
}

// ---------------------------------------------------------------------------
// apiGuard: the RoundTripper in front of every POST
// ---------------------------------------------------------------------------

// TestApiGuardPreflightsBeforePOST: the poisoned ghost never reaches the
// wire — the server sees a clean body on the very first request.
func TestApiGuardPreflightsBeforePOST(t *testing.T) {
	poison := `{"model":"m","messages":[` +
		`{"role":"user","content":"q"},` +
		`{"content":[]},` +
		`{"role":"user","content":"more"}]}`
	stub := &guardStub{reply: okReply}
	g := &apiGuard{inner: stub}
	resp, err := g.RoundTrip(postMessages(t, poison))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(stub.bodies) != 1 {
		t.Fatalf("want 1 request, got %d", len(stub.bodies))
	}
	if bytes.Equal(stub.bodies[0], []byte(poison)) {
		t.Fatal("poisoned body went out unchanged")
	}
	assertWireClean(t, stub.bodies[0])
	msgs := parseWire(t, stub.bodies[0])
	if len(msgs) != 1 {
		t.Fatalf("drop+merge should yield 1 message, got %d:\n%s", len(msgs), stub.bodies[0])
	}
}

// TestApiGuard400RepairsAndResends: a 400 citing messages[N].content is
// answered with a CORRECTED body — different from the first, legal, accepted.
func TestApiGuard400RepairsAndResends(t *testing.T) {
	healthy := `{"model":"m","messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":[{"type":"text","text":"yo"}]},` +
		`{"role":"user","content":"again"}]}`
	stub := &guardStub{reply: func(n int) (int, string) {
		if n == 1 {
			return http.StatusBadRequest, `{"type":"error","error":{"type":"invalid_request_error","message":"messages[1].content is required"}}`
		}
		return http.StatusOK, `{"ok":true}`
	}}
	g := &apiGuard{inner: stub}
	resp, err := g.RoundTrip(postMessages(t, healthy))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("corrected payload rejected: status=%d body=%s", resp.StatusCode, b)
	}
	if len(stub.bodies) != 2 {
		t.Fatalf("want exactly 2 requests, got %d", len(stub.bodies))
	}
	if bytes.Equal(stub.bodies[0], stub.bodies[1]) {
		t.Fatal("resent payload is identical to the failed one")
	}
	if !bytes.Equal(stub.bodies[0], []byte(healthy)) {
		t.Fatal("first send altered a healthy body")
	}
	assertWireClean(t, stub.bodies[1])
	if n := len(parseWire(t, stub.bodies[1])); n != 1 {
		t.Fatalf("repair should collapse to the merged user turn, got %d", n)
	}
}

// TestApiGuardRewritesRejectedRole: server rejects a role this guard
// considers legal → the cited entry is rewritten from its neighbours and
// the corrected payload is accepted.
func TestApiGuardRewritesRejectedRole(t *testing.T) {
	body := `{"model":"m","messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"system","content":[{"type":"text","text":"persona"}]},` +
		`{"role":"user","content":"again"}]}`
	stub := &guardStub{reply: func(n int) (int, string) {
		if n == 1 {
			return http.StatusBadRequest, `{"type":"error","error":{"type":"invalid_request_error","message":"messages[1].role is unsupported"}}`
		}
		return http.StatusOK, `{"ok":true}`
	}}
	g := &apiGuard{inner: stub}
	resp, err := g.RoundTrip(postMessages(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rewritten payload rejected: status=%d", resp.StatusCode)
	}
	if len(stub.bodies) != 2 {
		t.Fatalf("want 2 requests, got %d", len(stub.bodies))
	}
	msgs := parseWire(t, stub.bodies[1])
	var r string
	_ = json.Unmarshal(msgs[1]["role"], &r)
	if r != "assistant" {
		t.Fatalf("cited role rewritten to %q, want assistant", r)
	}
}

// TestApiGuardBoundedResends: a server that always cites messages[0] can
// never trap the request in a loop — at most 1+guardMaxRepairs sends, all
// with payloads that differ from each other, and the final 400 is handed
// back with its body intact.
func TestApiGuardBoundedResends(t *testing.T) {
	four := `{"model":"m","messages":[` +
		`{"role":"user","content":"a"},` +
		`{"role":"assistant","content":[{"type":"text","text":"b"}]},` +
		`{"role":"user","content":"c"},` +
		`{"role":"assistant","content":[{"type":"text","text":"d"}]}]}`
	stub := &guardStub{reply: func(int) (int, string) {
		return http.StatusBadRequest, `{"type":"error","error":{"type":"invalid_request_error","message":"messages[0].content is invalid"}}`
	}}
	g := &apiGuard{inner: stub}
	resp, err := g.RoundTrip(postMessages(t, four))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if len(stub.bodies) != 1+guardMaxRepairs {
		t.Fatalf("sent %d bodies, want at most %d", len(stub.bodies), 1+guardMaxRepairs)
	}
	for i := range stub.bodies {
		for j := i + 1; j < len(stub.bodies); j++ {
			if bytes.Equal(stub.bodies[i], stub.bodies[j]) {
				t.Fatalf("requests %d and %d are identical", i+1, j+1)
			}
		}
	}
	ebody, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(ebody), "is invalid") {
		t.Fatalf("final 400 body lost: %s", ebody)
	}
}

// TestApiGuardNeverResendsIdentical: when the repair cannot produce a
// different payload (role already equals the inferred one), the guard stops
// after the first send instead of re-POSTing the same bytes.
func TestApiGuardNeverResendsIdentical(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"q"}]}`
	stub := &guardStub{reply: func(int) (int, string) {
		return http.StatusBadRequest, `{"type":"error","error":{"type":"invalid_request_error","message":"messages[0].role is required"}}`
	}}
	g := &apiGuard{inner: stub}
	resp, err := g.RoundTrip(postMessages(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if len(stub.bodies) != 1 {
		t.Fatalf("identical payload resent: %d requests", len(stub.bodies))
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want the original 400 surfaced", resp.StatusCode)
	}
	ebody, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(ebody), "role is required") {
		t.Fatalf("400 body not restored: %s", ebody)
	}
}

// TestApiGuardPassesThroughForeign400: 400s beyond the guard's mandate
// (no citation, wrong error type, deeper path) surface untouched, one request.
func TestApiGuardPassesThroughForeign400(t *testing.T) {
	cases := map[string]string{
		"unciteable message":  `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens is too small"}}`,
		"overloaded":          `{"type":"error","error":{"type":"overloaded_error","message":"messages[1].role is required"}}`,
		"deeper content path": `{"error":{"type":"invalid_request_error","message":"messages[1].content[0].text is invalid"}}`,
	}
	for name, ebody := range cases {
		t.Run(name, func(t *testing.T) {
			healthy := `{"model":"m","messages":[{"role":"user","content":"q"}]}`
			stub := &guardStub{reply: func(int) (int, string) {
				return http.StatusBadRequest, ebody
			}}
			g := &apiGuard{inner: stub}
			resp, err := g.RoundTrip(postMessages(t, healthy))
			if err != nil {
				t.Fatal(err)
			}
			if len(stub.bodies) != 1 {
				t.Fatalf("foreign 400 resent: %d requests", len(stub.bodies))
			}
			got, _ := io.ReadAll(resp.Body)
			if string(got) != ebody {
				t.Fatalf("400 body altered:\n got %s\nwant %s", got, ebody)
			}
		})
	}
}

// TestApiGuardPassesThroughOtherRequests: GETs, non-Messages paths and
// body-less POSTs never enter the guard's validation path.
func TestApiGuardPassesThroughOtherRequests(t *testing.T) {
	stub := &guardStub{reply: func(int) (int, string) { return http.StatusOK, `{"ok":true}` }}
	g := &apiGuard{inner: stub}

	// GET /v1/messages — no body, nothing to validate.
	get, err := http.NewRequest(http.MethodGet, "https://api.example.com/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.RoundTrip(get); err != nil {
		t.Fatal(err)
	}
	if len(stub.bodies) != 1 || stub.bodies[0] != nil {
		t.Fatalf("GET recorded %d bodies (first=%q)", len(stub.bodies), stub.bodies[0])
	}

	// POST elsewhere: poisoned body passes through BYTES-IDENTICAL.
	poison := `{"messages":[{"content":[]}]}`
	other, err := http.NewRequest(http.MethodPost, "https://api.example.com/v1/embeddings", strings.NewReader(poison))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.RoundTrip(other); err != nil {
		t.Fatal(err)
	}
	if string(stub.bodies[1]) != poison {
		t.Fatalf("foreign path body altered: %s", stub.bodies[1])
	}

	// POST /v1/messages with no body at all.
	nobody, err := http.NewRequest(http.MethodPost, "https://api.example.com/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.RoundTrip(nobody); err != nil {
		t.Fatal(err)
	}
	if len(stub.bodies) != 3 {
		t.Fatalf("want 3 requests, got %d", len(stub.bodies))
	}
}

// TestApiGuardAuditTrail: every preflight and every 400-repair decision is
// greppable in the audit log — the visible trail for the 日志机制 requirement.
func TestApiGuardAuditTrail(t *testing.T) {
	home := tempDir(t)
	t.Setenv("LYON_HOME", home)
	hm := hooks.New("guard-audit-test")
	t.Cleanup(hm.Close)

	stub := &guardStub{reply: func(n int) (int, string) {
		switch n {
		case 1:
			return http.StatusOK, `{"ok":true}`
		case 2:
			return http.StatusBadRequest, `{"type":"error","error":{"type":"invalid_request_error","message":"messages[1].role is unsupported"}}`
		default:
			return http.StatusOK, `{"ok":true}`
		}
	}}
	g := &apiGuard{inner: stub, hooks: hm}

	// 1) preflight repair fires its audit line.
	poison := `{"model":"m","messages":[{"role":"user","content":"q"},{"content":[]},{"role":"user","content":"more"}]}`
	if resp, err := g.RoundTrip(postMessages(t, poison)); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("preflight request: resp=%v err=%v", resp, err)
	}
	// 2) 400 → repair → resend fires its own audit line.
	sys := `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"text","text":"p"}]},{"role":"user","content":"again"}]}`
	if resp, err := g.RoundTrip(postMessages(t, sys)); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("repair request: resp=%v err=%v", resp, err)
	}

	hm.Flush()
	audit, err := os.ReadFile(filepath.Join(home, "audit", time.Now().Format("2006-01-02")+".jsonl"))
	if err != nil {
		t.Fatalf("audit log: %v", err)
	}
	for _, want := range []string{
		`"event":"api_guard"`,
		"preflight: malformed messages repaired before POST",
		"400: repaired messages[1].role and resent (attempt 1)",
	} {
		if !strings.Contains(string(audit), want) {
			t.Errorf("audit missing %q:\n%s", want, audit)
		}
	}
}

// ---------------------------------------------------------------------------
// seam call sites: buildHistory / Restore / Export
// ---------------------------------------------------------------------------

// TestBuildHistorySanitizesSeed: a poisoned restored history is repaired
// before the runner is ever built from it — drop the ghost, merge the users,
// append the prompt, and file the repair in the audit log.
func TestBuildHistorySanitizesSeed(t *testing.T) {
	home := tempDir(t)
	t.Setenv("LYON_HOME", home)
	eng := NewEngine(Config{Workspace: tempDir(t)}, nil)
	t.Cleanup(eng.Close)
	sess := eng.NewSession(nil)
	t.Cleanup(sess.Close)

	sess.seed = []anthropic.BetaMessageParam{
		anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("original question")),
		{Content: []anthropic.BetaContentBlockParamUnion{}}, // the incident entry
		anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("follow-up detail")),
	}
	sess.pendingToolResults = nil

	hist := sess.buildHistory("", "next prompt")
	if len(hist) == 0 {
		t.Fatal("buildHistory returned nothing")
	}
	if fixed, ok := sanitizeMessages(hist); ok {
		t.Fatalf("buildHistory emitted a malformed history: role=%q content=%d", fixed[0].Role, len(fixed[0].Content))
	}
	var joined strings.Builder
	for _, m := range hist {
		for _, b := range m.Content {
			if b.OfText != nil {
				joined.WriteString(b.OfText.Text)
				joined.WriteString("\n")
			}
		}
	}
	for _, want := range []string{"original question", "follow-up detail", "next prompt"} {
		if !strings.Contains(joined.String(), want) {
			t.Errorf("history lost %q:\n%s", want, joined.String())
		}
	}

	eng.Hooks().Flush()
	audit, err := os.ReadFile(filepath.Join(home, "audit", time.Now().Format("2006-01-02")+".jsonl"))
	if err != nil {
		t.Fatalf("audit log: %v", err)
	}
	if !strings.Contains(string(audit), `"event":"history_repair"`) ||
		!strings.Contains(string(audit), "buildHistory before POST") {
		t.Errorf("buildHistory repair not audited:\n%s", audit)
	}
}

// TestRestoreExportSanitizeHistory: neither direction lets a malformed
// entry through — Restore never seeds the poisoned history, Export never
// writes it to disk — and both leave the caller's slice untouched.
func TestRestoreExportSanitizeHistory(t *testing.T) {
	eng := NewEngine(Config{Workspace: tempDir(t)}, nil)
	t.Cleanup(eng.Close)
	sess := eng.NewSession(nil)
	t.Cleanup(sess.Close)

	poison := []anthropic.BetaMessageParam{
		anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("keep me")),
		{Content: []anthropic.BetaContentBlockParamUnion{}},
	}

	// Restore: repaired on the way in.
	sess.Restore(Snapshot{Workspace: eng.cfg.Workspace, Messages: poison})
	if fixed, ok := sanitizeMessages(sess.seed); ok {
		t.Fatalf("Restore seeded a malformed history: role=%q content=%d", fixed[0].Role, len(fixed[0].Content))
	}
	if len(sess.seed) != 1 || sess.seed[0].Role != anthropic.BetaMessageParamRoleUser {
		t.Fatalf("restored seed: %#v", sess.seed)
	}
	if len(poison) != 2 {
		t.Fatalf("Restore mutated the snapshot's slice: %#v", poison)
	}

	// Export: repaired on the way out, live seed untouched.
	sess.runner = nil
	sess.seed = append([]anthropic.BetaMessageParam(nil), poison...)
	snap := sess.Export()
	if fixed, ok := sanitizeMessages(snap.Messages); ok {
		t.Fatalf("Export carried a malformed entry: role=%q content=%d", fixed[0].Role, len(fixed[0].Content))
	}
	if len(snap.Messages) != 1 || snap.Messages[0].Role != anthropic.BetaMessageParamRoleUser {
		t.Fatalf("exported messages: %#v", snap.Messages)
	}
	if len(sess.seed) != 2 {
		t.Fatalf("Export mutated the live seed: %#v", sess.seed)
	}
}
