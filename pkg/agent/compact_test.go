package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"lyon-agent/internal/mockapi"
	"lyon-agent/pkg/tools"
)

// newCompactMock points the process at a scriptable mock API. Threshold
// 0.001 × 1000-token window: every mock request (its usage is derived from
// body size, tools array alone ≈ kilobytes) crosses the trigger.
func newCompactMock(t *testing.T, script []mockapi.Turn) *mockapi.Server {
	t.Helper()
	mock := mockapi.New(mockapi.Config{Script: script, Latency: 2 * time.Millisecond})
	t.Cleanup(mock.Close)
	t.Setenv("ANTHROPIC_BASE_URL", mock.URL())
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "compaction-test-token")
	return mock
}

func compactTestConfig(t *testing.T) Config {
	return Config{
		CompactThreshold: 0.001,
		ContextWindow:    1000,
		Workspace:        tempDir(t),
	}
}

// waitPromptEndCollect drains until EventPromptEnd, also collecting every
// EventCompaction seen on the way (waitEvent drops intermediate events).
func waitPromptEndCollect(t *testing.T, sess *Session) (Event, []Event) {
	t.Helper()
	var comps []Event
	deadline := time.After(15 * time.Second)
	for {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				t.Fatal("event stream closed while waiting for prompt_end")
			}
			switch ev.Type {
			case EventCompaction:
				comps = append(comps, ev)
			case EventPromptEnd:
				return ev, comps
			}
		case <-deadline:
			t.Fatal("timeout waiting for prompt_end")
		}
	}
}

// TestClientCompactionFinalTurn: an end_turn response crossing the
// threshold is summarized client-side — the history becomes one summary
// user message, an EventCompaction reports the replacement, a compaction
// TurnStat is recorded, and the next prompt merges into the summary
// instead of replaying the original conversation.
func TestClientCompactionFinalTurn(t *testing.T) {
	mock := newCompactMock(t, []mockapi.Turn{
		{Text: []string{"original-task-marker: implement the widget"}},
		{Text: []string{"THE-SUMMARY"}},
	})
	eng := NewEngine(compactTestConfig(t), nil)
	sess := eng.NewSession(nil)
	defer sess.Close()

	if err := sess.Submit("original-task-marker: implement the widget"); err != nil {
		t.Fatal(err)
	}
	pe, comps := waitPromptEndCollect(t, sess)
	if pe.Err != "" {
		t.Fatalf("prompt failed: %s", pe.Err)
	}
	if len(comps) == 0 || !strings.Contains(comps[0].Detail, "summary replaced") {
		t.Fatalf("compaction events: %+v", comps)
	}

	// History replaced: exactly one summary user message.
	snap := sess.Export()
	if len(snap.Messages) != 1 || snap.Turns == nil {
		t.Fatalf("post-compaction history: %d messages, turns=%d", len(snap.Messages), len(snap.Turns))
	}
	var texts []string
	for _, m := range snap.Messages {
		for _, b := range m.Content {
			if b.OfText != nil {
				texts = append(texts, b.OfText.Text)
			}
		}
	}
	joined := strings.Join(texts, "\n")
	if !strings.Contains(joined, "conversation compacted") || !strings.Contains(joined, "THE-SUMMARY") {
		t.Fatalf("summary message missing wrapper or summary:\n%.500s", joined)
	}
	if strings.Contains(joined, "original-task-marker") {
		t.Fatalf("original conversation survived compaction:\n%.500s", joined)
	}
	// A compaction turn was accounted for.
	var sawCompactionStat bool
	for _, st := range sess.Stats() {
		if st.StopReason == "compaction" {
			sawCompactionStat = true
		}
	}
	if !sawCompactionStat {
		t.Fatalf("no compaction TurnStat: %+v", sess.Stats())
	}
	// The mock served exactly: turn + summarize.
	if n := mock.Calls(); n != 2 {
		t.Fatalf("calls = %d, want 2 (turn + summarize)", n)
	}

	// Next prompt merges into the trailing user summary — no second
	// consecutive user message, no original history.
	if err := sess.Submit("continue with the plan"); err != nil {
		t.Fatal(err)
	}
	if pe := waitEvent(t, sess, EventPromptEnd); pe.Err != "" {
		t.Fatalf("follow-up prompt: %s", pe.Err)
	}
	body := string(mock.LastBody())
	if !strings.Contains(body, "THE-SUMMARY") || !strings.Contains(body, "continue with the plan") {
		t.Fatalf("follow-up did not build on the summary:\n%.2000s", body)
	}
	if strings.Contains(body, "original-task-marker") {
		t.Fatalf("follow-up replayed pre-compaction history:\n%.2000s", body)
	}
}

// TestClientCompactionDeferredRecreate: a trigger on a tool_use turn waits
// for the tools to run (deferred via compactArmed); when the very next turn
// still crosses the threshold and ends on a dangling tool_use, compactNow
// recreates the runner seeded with the summary — the abandoned calls are
// never dispatched against it.
func TestClientCompactionDeferredRecreate(t *testing.T) {
	mock := newCompactMock(t, []mockapi.Turn{
		{Tool: &mockapi.ToolCall{Name: "noop", Input: "{}"}},
		{Tool: &mockapi.ToolCall{Name: "noop", Input: "{}"}},
		{Text: []string{"RECREATED-SUMMARY"}},
		{Text: []string{"final-answer"}},
	})
	reg := tools.NewRegistry()
	if err := tools.RegisterFunc(reg, "noop", "does nothing", false,
		func(ctx context.Context, _ struct{}) (string, error) { return "ok", nil }); err != nil {
		t.Fatal(err)
	}
	eng := NewEngine(compactTestConfig(t), reg)
	sess := eng.NewSession(nil)
	defer sess.Close()

	if err := sess.Submit("task-needs-tools"); err != nil {
		t.Fatal(err)
	}
	pe, comps := waitPromptEndCollect(t, sess)
	if pe.Err != "" {
		t.Fatalf("prompt failed: %s", pe.Err)
	}
	if len(comps) == 0 || !strings.Contains(comps[0].Detail, "summary replaced") {
		t.Fatalf("compaction events: %+v", comps)
	}

	snap := sess.Export()
	// The history restarts as the summary user message; turn3's real
	// answer appends after it. Nothing pre-compaction may survive.
	if len(snap.Messages) == 0 || snap.Messages[0].Role != anthropic.BetaMessageParamRoleUser {
		t.Fatalf("history does not restart with the summary: %d msgs", len(snap.Messages))
	}
	var got string
	for _, b := range snap.Messages[0].Content {
		if b.OfText != nil {
			got += b.OfText.Text
		}
	}
	for _, want := range []string{"conversation compacted", "RECREATED-SUMMARY", "Continue the original task"} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary message missing %q:\n%.500s", want, got)
		}
	}
	for i, m := range snap.Messages {
		if strings.Contains(plainText(m), "task-needs-tools") {
			t.Fatalf("original prompt survived compaction in msg[%d]", i)
		}
	}
	// turn1 + turn2 + summarize + turn3 — exactly once, no re-compaction
	// loop after the summary shrank the request.
	if n := mock.Calls(); n != 4 {
		t.Fatalf("calls = %d, want 4", n)
	}
}

// TestRenderHistory: role tags, per-block clipping, thinking dropped.
func TestRenderHistory(t *testing.T) {
	long := strings.Repeat("x", compactBlockCap+100)
	msgs := []anthropic.BetaMessageParam{
		anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("hello")),
		{
			Role: anthropic.BetaMessageParamRoleAssistant,
			Content: []anthropic.BetaContentBlockParamUnion{
				{OfThinking: &anthropic.BetaThinkingBlockParam{Thinking: "secret reasoning"}},
				{OfToolUse: &anthropic.BetaToolUseBlockParam{ID: "t1", Name: "run_shell", Input: json.RawMessage(`{"command":"ls"}`)}},
				{OfText: &anthropic.BetaTextBlockParam{Text: long}},
			},
		},
	}
	out := renderHistory(msgs)
	if !strings.Contains(out, "[user] hello") {
		t.Fatalf("user text missing:\n%s", out)
	}
	if !strings.Contains(out, "[assistant] → tool run_shell {\"command\":\"ls\"}") {
		t.Fatalf("tool_use missing:\n%s", out)
	}
	if strings.Contains(out, "secret reasoning") {
		t.Fatal("thinking must be dropped")
	}
	if strings.Contains(out, long) || !strings.Contains(out, "... [truncated]") {
		t.Fatal("oversized block not clipped")
	}
	// Clipped block is bounded.
	if len(out) > 8000 {
		t.Fatalf("rendered history suspiciously large: %d bytes", len(out))
	}
}

// TestBuildHistoryTrailingUserMerge: a history ending in a user message
// (restored snapshot / compaction summary) receives the new prompt as extra
// blocks — never a second consecutive user message.
func TestBuildHistoryTrailingUserMerge(t *testing.T) {
	sess := newTestSessionForRunner(t, Config{Workspace: tempDir(t)})
	// Simulate a restored/compacted history: the runner's Messages() is the
	// history source, so feed it via seed + nil runner.
	sess.runner = nil
	sess.seed = []anthropic.BetaMessageParam{
		anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("summary so far")),
	}
	sess.pendingToolResults = []anthropic.BetaContentBlockParamUnion{}
	// pendingToolResults with a trailing-user seed would violate the repair
	// invariant; keep empty and assert the merge shape instead.
	hist := sess.buildHistory("", "next question")
	if len(hist) != 1 {
		t.Fatalf("want merged single message, got %d", len(hist))
	}
	if hist[0].Role != anthropic.BetaMessageParamRoleUser {
		t.Fatalf("role = %s", hist[0].Role)
	}
	var texts []string
	for _, b := range hist[0].Content {
		if b.OfText != nil {
			texts = append(texts, b.OfText.Text)
		}
	}
	joined := strings.Join(texts, "|")
	if !strings.Contains(joined, "summary so far") || !strings.Contains(joined, "next question") {
		t.Fatalf("merge lost content: %q", joined)
	}
}

// TestBuildHistoryMemoryInjection: the workspace memory block enters the
// first prompt exactly once, and never when the restored history already
// carries it.
func TestBuildHistoryMemoryInjection(t *testing.T) {
	sess := newTestSessionForRunner(t, Config{Workspace: tempDir(t)})
	sess.memoryText = "[persistent-memory]\n## codename\nproject: lychee"

	hist := sess.buildHistory("", "hi")
	if len(hist) != 1 {
		t.Fatalf("hist: %d", len(hist))
	}
	if !strings.Contains(plainText(hist[0]), "project: lychee") {
		t.Fatalf("memory not injected: %q", plainText(hist[0]))
	}
	// Second prompt: injected once, not again.
	sess.runner = nil // force fresh assembly (no runner history growth)
	sess.seed = hist
	hist2 := sess.buildHistory("", "again")
	if n := strings.Count(plainText(hist2[0]), "project: lychee"); n != 1 {
		t.Fatalf("memory duplicated: %d copies", n)
	}

	// Fresh session whose restored history already carries the current
	// memory: skip injection entirely.
	sess2 := newTestSessionForRunner(t, Config{Workspace: tempDir(t)})
	sess2.memoryText = "[persistent-memory]\n## codename\nproject: lychee"
	sess2.runner = nil
	sess2.seed = hist
	hist3 := sess2.buildHistory("", "fresh")
	if n := strings.Count(plainText(hist3[0]), "project: lychee"); n != 1 {
		t.Fatalf("restored history re-injected memory: %d copies", n)
	}
}

// plainText concatenates a message's text blocks.
func plainText(m anthropic.BetaMessageParam) string {
	var b strings.Builder
	for _, blk := range m.Content {
		if blk.OfText != nil {
			b.WriteString(blk.OfText.Text)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// TestSnapshotRoundTrip: Export → Save → Load → Restore preserves the
// conversation, and the restored session replays it into the next request.
func TestSnapshotRoundTrip(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t)) // the store must not touch ~/.lyon
	mock := newCompactMock(t, []mockapi.Turn{
		{Text: []string{"first answer about the widget"}},
		{Text: []string{"second answer after restore"}},
	})
	reg := tools.NewRegistry()
	if err := tools.RegisterFunc(reg, "noop", "does nothing", false,
		func(ctx context.Context, _ struct{}) (string, error) { return "ok", nil }); err != nil {
		t.Fatal(err)
	}
	cfg := Config{CompactThreshold: -1, ContextWindow: 100000, Workspace: tempDir(t)}
	eng := NewEngine(cfg, reg)
	sess := eng.NewSession(nil)
	if err := sess.Submit("remember the widget plan"); err != nil {
		t.Fatal(err)
	}
	pe := waitEvent(t, sess, EventPromptEnd)
	if pe.Err != "" {
		t.Fatalf("prompt1: %s", pe.Err)
	}

	snap := sess.Export()
	if len(snap.Messages) == 0 || len(snap.Turns) == 0 {
		t.Fatalf("export empty: msgs=%d turns=%d", len(snap.Messages), len(snap.Turns))
	}
	sess.Close() // the caller's normal lifecycle

	// On-disk round trip.
	if err := SaveSnapshot(snap); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSnapshot(cfg.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil {
		t.Fatal("LoadSnapshot returned nil for an existing snapshot")
	}
	if len(loaded.Messages) != len(snap.Messages) || len(loaded.Turns) != len(snap.Turns) {
		t.Fatalf("round trip lost data: %d→%d msgs, %d→%d turns",
			len(snap.Messages), len(loaded.Messages), len(snap.Turns), len(loaded.Turns))
	}
	// Bytes-identical history content after the JSON round trip.
	rawA, _ := json.Marshal(snap.Messages)
	rawB, _ := json.Marshal(loaded.Messages)
	if string(rawA) != string(rawB) {
		t.Fatalf("messages changed through JSON:\nA: %.600s\nB: %.600s", rawA, rawB)
	}
	// Snapshot path is workspace-keyed and stable.
	p1, _ := SnapshotPath(cfg.Workspace)
	p2, _ := SnapshotPath(cfg.Workspace)
	if p1 != p2 || !strings.HasSuffix(p1, ".json") {
		t.Fatalf("path unstable: %q %q", p1, p2)
	}
	other, _ := SnapshotPath(filepath.Join(tempDir(t), "elsewhere"))
	if other == p1 {
		t.Fatal("different workspaces must map to different snapshot files")
	}

	// Restore into a fresh session: the follow-up request replays the
	// saved history (turn numbering continues past the restored stats).
	sess2 := eng.NewSession(nil)
	defer sess2.Close()
	sess2.Restore(*loaded)
	if err := sess2.Submit("and the next step?"); err != nil {
		t.Fatal(err)
	}
	pe = waitEvent(t, sess2, EventPromptEnd)
	if pe.Err != "" {
		t.Fatalf("prompt2: %s", pe.Err)
	}
	body := string(mock.LastBody())
	if !strings.Contains(body, "remember the widget plan") || !strings.Contains(body, "and the next step?") {
		t.Fatalf("restored session did not replay history:\n%.2000s", body)
	}
	// Turn numbering continues past the restored stats: 1 saved + 1 new.
	stats := sess2.Stats()
	if len(stats) != 2 || stats[1].Turn != 2 {
		var ns []int
		for _, st := range stats {
			ns = append(ns, st.Turn)
		}
		t.Fatalf("restored session turns = %v, want [1 2]", ns)
	}
}

// TestLoadSnapshotMissing: no file → (nil, nil), not an error.
func TestLoadSnapshotMissing(t *testing.T) {
	snap, err := LoadSnapshot(tempDir(t))
	if err != nil || snap != nil {
		t.Fatalf("got %v, %v; want nil, nil", snap, err)
	}
}
