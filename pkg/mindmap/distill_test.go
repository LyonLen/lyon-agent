package mindmap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"lyon-agent/internal/mockapi"
	"lyon-agent/pkg/agent"
)

// newDistillSession wires a seated session against an already-running
// mock: throwaway LYON_HOME (scratchHome's settle cleanup), engine,
// two-message seed. Cleanup order matters: engine Close (registered
// after scratchHome) runs before the directory sweep, so the hooks
// writer cannot recreate audit files mid-delete.
func newDistillSession(t *testing.T, mock *mockapi.Server) (*Manager, *agent.Session, string) {
	t.Helper()
	scratchHome(t)
	t.Setenv("ANTHROPIC_BASE_URL", mock.URL())
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "mindmap-test-token")
	eng := agent.NewEngine(agent.DefaultConfig(), nil)
	t.Cleanup(eng.Close)
	sess := eng.NewSession(nil)
	t.Cleanup(sess.Close)
	sid := "distillseat000001"
	sess.SetSeat(sid, time.Now())
	sess.Restore(agent.Snapshot{Messages: []anthropic.BetaMessageParam{
		anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("我们讨论一下存储方案，最后决定用 SQLite。")),
		{Role: anthropic.BetaMessageParamRoleAssistant, Content: []anthropic.BetaContentBlockParamUnion{
			{OfText: &anthropic.BetaTextBlockParam{Text: "好，那就用 SQLite，开启 WAL。"}},
		}},
	}})
	return New(), sess, sid
}

// newDistillEnv is newDistillSession plus a fresh scripted mock.
func newDistillEnv(t *testing.T, script []mockapi.Turn) (*Manager, *agent.Session, *mockapi.Server, string) {
	t.Helper()
	mock := mockapi.New(mockapi.Config{Script: script})
	t.Cleanup(mock.Close)
	m, sess, sid := newDistillSession(t, mock)
	return m, sess, mock, sid
}

// auditText returns today's audit JSONL for the scratch home ("" when
// nothing was written).
func auditText(t *testing.T) string {
	t.Helper()
	path := filepath.Join(os.Getenv("LYON_HOME"), "audit", time.Now().Format("2006-01-02")+".jsonl")
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// ── ParseDiff ─────────────────────────────────────────────────────

func TestParseDiff(t *testing.T) {
	cases := []struct {
		name    string
		reply   string
		wantOps int
		wantErr bool
	}{
		{"plain object", `{"ops":[{"op":"park","id":"a"}]}`, 1, false},
		{"fenced", "```json\n{\"ops\":[{\"op\":\"add\",\"id\":\"x\",\"label\":\"l\"}]}\n```", 1, false},
		{"prose around", `Sure! Here is the diff: {"title":"T","ops":[]} done.`, 0, false},
		{"bare array", `[{"op":"park","id":"a"}]`, 1, false},
		{"empty ops", `{"ops":[]}`, 0, false},
		{"garbage", `sorry, no json today`, 0, true},
		{"empty reply", ``, 0, true},
		{"bad json body", `{"ops": [oops]}`, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := ParseDiff(tc.reply)
			if tc.wantErr {
				if err == nil {
					t.Fatal("want error, got none")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(d.Ops) != tc.wantOps {
				t.Errorf("ops = %d, want %d", len(d.Ops), tc.wantOps)
			}
		})
	}
}

// ── renderTranscript ──────────────────────────────────────────────

func TestRenderTranscript(t *testing.T) {
	text := func(role anthropic.BetaMessageParamRole, parts ...string) anthropic.BetaMessageParam {
		m := anthropic.BetaMessageParam{Role: role}
		for _, p := range parts {
			m.Content = append(m.Content, anthropic.BetaContentBlockParamUnion{
				OfText: &anthropic.BetaTextBlockParam{Text: p},
			})
		}
		return m
	}
	toolResultBlock := anthropic.NewBetaToolResultTextBlockParam("toolu_1", "file contents", false)
	toolResult := anthropic.BetaMessageParam{
		Role: anthropic.BetaMessageParamRoleUser,
		Content: []anthropic.BetaContentBlockParamUnion{
			{OfToolResult: &toolResultBlock},
		},
	}
	msgs := []anthropic.BetaMessageParam{
		text(anthropic.BetaMessageParamRoleUser, "第一个问题"),
		text(anthropic.BetaMessageParamRoleAssistant, "第一个回答"),
		toolResult, // must not bump the turn counter
		text(anthropic.BetaMessageParamRoleUser, "第二个问题"),
	}
	out := renderTranscript(msgs, 10000)
	if !strings.Contains(out, "[turn 1] user: 第一个问题") {
		t.Errorf("first user turn missing:\n%s", out)
	}
	if !strings.Contains(out, "[turn 2] user: 第二个问题") {
		t.Errorf("tool-result message must not consume a turn:\n%s", out)
	}
	if strings.Contains(out, "file contents") {
		t.Errorf("tool results stay out of the tail:\n%s", out)
	}

	// Budget: only the tail survives, marked as elided.
	big := make([]anthropic.BetaMessageParam, 0, 20)
	for i := 0; i < 20; i++ {
		big = append(big, text(anthropic.BetaMessageParamRoleUser, strings.Repeat("字", 500)))
	}
	tail := renderTranscript(big, 2000)
	if !strings.Contains(tail, "(earlier turns elided)") {
		t.Error("budget overflow must elide the head")
	}
	if len([]rune(tail)) > 4000 {
		t.Errorf("tail too large: %d runes", len([]rune(tail)))
	}
}

// ── meta-channel plumbing never reaches the model (Image #58/#59) ──
//
// The web client synthesizes map-edit notices and selection markers
// inside real user messages. They must not turn the tail into edit-log
// meta (which the distiller then admitted as nodes), while the turn
// counter keeps consuming their slots so source_turn jump anchors stay
// aligned with the web feed's user-block count.

func TestRenderTranscriptMetaStripped(t *testing.T) {
	txt := func(role anthropic.BetaMessageParamRole, s string) anthropic.BetaMessageParam {
		return anthropic.BetaMessageParam{Role: role, Content: []anthropic.BetaContentBlockParamUnion{
			{OfText: &anthropic.BetaTextBlockParam{Text: s}},
		}}
	}
	notice := "我在思维导图上做了改动：删除「场景层壳」。\n" +
		"【系统 · 图上自动提问纪律】先用一句话复述你对这次改动的理解，只问一个最关键的问题；不要执行任何工具。"
	selAsk := "[导图选区 3 个节点|a,b,c] 把措辞统一成「用户：诉求」\n" +
		"【系统 · 选区提问纪律】你在回答一个「基于导图选区」的提问。规则：\n1. 需要改图时：只输出一个 ```map_ops 代码块。"
	msgs := []anthropic.BetaMessageParam{
		txt(anthropic.BetaMessageParamRoleUser, "真实问题一"),
		txt(anthropic.BetaMessageParamRoleAssistant, "自然的回答"),
		txt(anthropic.BetaMessageParamRoleUser, notice), // turn 2 — counted, not rendered
		txt(anthropic.BetaMessageParamRoleUser, selAsk), // turn 3 — marker+suffix stripped
		txt(anthropic.BetaMessageParamRoleUser, "真实问题四"),
	}
	out := renderTranscript(msgs, 10000)

	for _, want := range []string{"[turn 1] user: 真实问题一", "把措辞统一成「用户：诉求」", "[turn 4] user: 真实问题四"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{"我在思维导图上做了改动", "自动提问纪律", "导图选区", "map_ops", "【系统 ·"} {
		if strings.Contains(out, bad) {
			t.Errorf("meta wrapper %q leaked into the tail:\n%s", bad, out)
		}
	}
	// "[turn 4]" in the wants above IS the turn-slot assertion: the
	// dropped notice (turn 2) and stripped ask (turn 3) must still be
	// counted, otherwise source_turn anchors drift against the feed.
}

// The prompt is the second half of the admission contract — pin the
// subject-based exclusion so it cannot silently regress to the old
// conclusion-shaped-only rule that let map-meta through.

func TestDistillPromptMetaAdmission(t *testing.T) {
	for _, want := range []string{
		"ABOUT THE SUBJECT",
		"its subject is the map",
		"park them this round",
	} {
		if !strings.Contains(distillSystemPrompt, want) {
			t.Errorf("distillSystemPrompt missing admission clause %q", want)
		}
	}
}

// ── D1: bad JSON / failed ask → silent skip, one audit line ───────

func TestD1BadJSONSkipped(t *testing.T) {
	m, sess, mock, sid := newDistillEnv(t, []mockapi.Turn{
		{Text: []string{"sorry, no json today"}},
	})
	// Seed one node so "nothing changed" is observable.
	seed := m.ApplyDistill(sid, 0, Diff{Ops: []Op{
		{Op: "add", ID: "root", Label: "主题", Kind: "branch"},
	}})
	if !seed.Changed {
		t.Fatal("seed must land")
	}
	revBefore, nodesBefore, _ := m.State(sid)

	m.distill(sess, sid) // synchronous: the Hook goroutine wrapper is thin

	revAfter, nodesAfter, _ := m.State(sid)
	if revAfter != revBefore {
		t.Errorf("revision advanced on bad JSON: %d → %d", revBefore, revAfter)
	}
	if len(nodesAfter) != len(nodesBefore) || nodesAfter[0] != nodesBefore[0] {
		t.Errorf("graph mutated on bad JSON: %+v", nodesAfter)
	}
	if mock.Calls() != 1 {
		t.Errorf("mock calls = %d, want 1 (the round did run)", mock.Calls())
	}
	sess.Hooks().Flush()
	audit := auditText(t)
	if !strings.Contains(audit, `"event":"mindmap.distill"`) {
		t.Errorf("distill round not audited:\n%s", audit)
	}
	if !strings.Contains(audit, "bad JSON") {
		t.Errorf("bad-JSON reason missing from audit:\n%s", audit)
	}
}

func TestD1AskFailureSkipped(t *testing.T) {
	// Deadline below the mock's latency: Ask errors out and the round
	// skips — same D1 contract as bad JSON.
	old := distillTimeout
	distillTimeout = 5 * time.Millisecond
	t.Cleanup(func() { distillTimeout = old })

	mock := mockapi.New(mockapi.Config{
		Script:  []mockapi.Turn{{Text: []string{`{"ops":[]}`}}},
		Latency: 300 * time.Millisecond,
	})
	t.Cleanup(mock.Close)
	m, sess, sid := newDistillSession(t, mock)

	m.distill(sess, sid)

	if mock.Calls() != 1 {
		t.Errorf("mock calls = %d, want 1 (the attempt was made)", mock.Calls())
	}
	rev, nodes, _ := m.State(sid)
	if rev != 0 || len(nodes) != 0 {
		t.Errorf("state touched on ask failure: rev=%d nodes=%d", rev, len(nodes))
	}
	sess.Hooks().Flush()
	audit := auditText(t)
	if !strings.Contains(audit, `"event":"mindmap.distill"`) {
		t.Errorf("ask failure not audited:\n%s", audit)
	}
	if !strings.Contains(audit, "ask failed") {
		t.Errorf("ask-failed reason missing:\n%s", audit)
	}
}

// ── D2: LYON_MINDMAP=off → zero calls, zero events ────────────────

func TestD2OffZeroCalls(t *testing.T) {
	t.Setenv("LYON_MINDMAP", "off")
	m, sess, mock, sid := newDistillEnv(t, nil)
	var published bool
	// Hook reads the process-wide default manager — wire both managers so
	// a wrongly-spawned round would be visible either way.
	m.SetPublisher(func(string, agent.Event) { published = true })
	Open().SetPublisher(func(string, agent.Event) { published = true })
	t.Cleanup(func() { Open().SetPublisher(nil) })

	Hook(sess, agent.Event{Type: agent.EventPromptEnd})
	time.Sleep(100 * time.Millisecond) // a wrongly-spawned round would reach the mock by now

	if mock.Calls() != 0 {
		t.Errorf("mock received %d requests, want 0", mock.Calls())
	}
	if published {
		t.Error("an event was published while off")
	}
	rev, nodes, err := m.State(sid)
	if err != nil || rev != 0 || len(nodes) != 0 {
		t.Errorf("state touched while off: rev=%d nodes=%d err=%v", rev, len(nodes), err)
	}
	if audit := auditText(t); strings.Contains(audit, "mindmap.distill") {
		t.Errorf("audit written while off:\n%s", audit)
	}
}

// Even when on, non-prompt_end events are not a trigger.
func TestHookIgnoresOtherEvents(t *testing.T) {
	_, sess, mock, _ := newDistillEnv(t, nil)
	Open().SetPublisher(func(string, agent.Event) {})
	t.Cleanup(func() { Open().SetPublisher(nil) })

	Hook(sess, agent.Event{Type: agent.EventTurnEnd})
	time.Sleep(50 * time.Millisecond)
	if mock.Calls() != 0 {
		t.Errorf("non-prompt_end event triggered a call: %d", mock.Calls())
	}
}

// ── Image #71: only mindmap mode draws ─────────────────────────────

// A default-mode session must never grow a map: no ask, no publish,
// no state. (The screenshot case — a running default session whose
// map kept drawing itself.)
func TestHookDefaultModeNeverDraws(t *testing.T) {
	_, sess, mock, _ := newDistillEnv(t, nil)
	sid := "defaultmode000001"
	sess.SetSeat(sid, time.Now())
	published := false
	Open().SetPublisher(func(string, agent.Event) { published = true })
	t.Cleanup(func() { Open().SetPublisher(nil) })

	Hook(sess, agent.Event{Type: agent.EventPromptEnd}) // sess.Mode() == default
	time.Sleep(150 * time.Millisecond)                  // a wrongly-spawned round would reach the mock by now

	if mock.Calls() != 0 {
		t.Errorf("default mode distilled: %d mock calls, want 0", mock.Calls())
	}
	if published {
		t.Error("mindmap event published from default mode")
	}
	if rev, nodes, err := Open().State(sid); err != nil || rev != 0 || len(nodes) != 0 {
		t.Errorf("default mode touched state: rev=%d nodes=%d err=%v", rev, len(nodes), err)
	}
}

// Flip the session to mindmap mode and the same event distills — the
// gate is the mode, not a missing hook.
func TestHookMindmapModeDraws(t *testing.T) {
	reply := `{"title":"存储方案","ops":[
		{"op":"add","id":"root","parent_id":"","label":"存储方案","kind":"branch","source_turn":1}
	]}`
	_, sess, mock, _ := newDistillEnv(t, []mockapi.Turn{{Text: []string{reply}}})
	sid := "mindmapmode00001"
	sess.SetSeat(sid, time.Now())
	if _, err := sess.SetMode(agent.ModeMindmap); err != nil {
		t.Fatalf("SetMode: %v", err)
	}
	done := make(chan struct{}, 1)
	Open().SetPublisher(func(string, agent.Event) {
		select {
		case done <- struct{}{}:
		default:
		}
	})
	t.Cleanup(func() { Open().SetPublisher(nil) })

	Hook(sess, agent.Event{Type: agent.EventPromptEnd})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("mindmap mode did not distill within 5s (no publish)")
	}
	if mock.Calls() == 0 {
		t.Error("publish landed but the mock never saw the ask")
	}
}

// ── happy path: diff lands, event publishes, audit records ────────

func TestDistillHappyPathPublishes(t *testing.T) {
	reply := `{"title":"存储方案","ops":[
		{"op":"add","id":"root","parent_id":"","label":"存储方案","kind":"branch","source_turn":1},
		{"op":"add","id":"b-sqlite","parent_id":"root","label":"决定用 SQLite","kind":"decision","source_turn":1},
		{"op":"add","id":"b-wal","parent_id":"b-sqlite","label":"开启 WAL","kind":"fact","source_turn":1}
	]}`
	m, sess, mock, sid := newDistillEnv(t, []mockapi.Turn{{Text: []string{reply}}})

	got := make(chan agent.Event, 1)
	m.SetPublisher(func(psid string, ev agent.Event) {
		if psid != sid {
			t.Errorf("publish sid = %q, want %q", psid, sid)
		}
		got <- ev
	})

	m.distill(sess, sid)

	if mock.Calls() != 1 {
		t.Fatalf("mock calls = %d, want exactly 1 (C1: one call per round)", mock.Calls())
	}
	// The request carried the §2 input contract: base revision +
	// inventory + transcript tail.
	body := string(mock.LastBody())
	for _, want := range []string{"Map revision:", "Transcript (role-tagged tail)", "nodes (id | parent_id | label"} {
		if !strings.Contains(body, want) {
			t.Errorf("request body missing %q", want)
		}
	}

	// State: title + three nodes, revision ticked once.
	rev, title, _, nodesJSON, err := m.Snapshot(sid)
	if err != nil {
		t.Fatal(err)
	}
	if rev != 1 || title != "存储方案" {
		t.Errorf("rev=%d title=%q, want 1/存储方案", rev, title)
	}
	for _, id := range []string{"root", "b-sqlite", "b-wal"} {
		if !strings.Contains(nodesJSON, `"id":"`+id+`"`) {
			t.Errorf("node %q missing from %s", id, nodesJSON)
		}
	}
	if !strings.Contains(nodesJSON, `"state":"proposed"`) {
		t.Error("distill nodes must land proposed (§4.3)")
	}

	select {
	case ev := <-got:
		if ev.Type != TypeUpdate {
			t.Errorf("event type = %q, want %q", ev.Type, TypeUpdate)
		}
		if ev.Attrs["revision"] != "1" || ev.Attrs["title"] != "存储方案" {
			t.Errorf("attrs = %+v", ev.Attrs)
		}
		if !strings.Contains(ev.Attrs["nodes"], "b-sqlite") {
			t.Errorf("attrs.nodes missing content: %s", ev.Attrs["nodes"])
		}
	default:
		t.Fatal("no EVENT_MINDMAP_UPDATE published")
	}

	sess.Hooks().Flush()
	if audit := auditText(t); !strings.Contains(audit, `"event":"mindmap.distill"`) {
		t.Errorf("distill round not audited:\n%s", audit)
	}
	assertTree(t, m, sid)
}
