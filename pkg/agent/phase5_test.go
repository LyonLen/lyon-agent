package agent

import (
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// newTestSessionForRunner builds a session plus a runner so drive-internal
// helpers (maybeCompact, buildHistory) are testable without an API call.
func newTestSessionForRunner(t *testing.T, cfg Config) *Session {
	t.Helper()
	eng := NewEngine(cfg, nil)
	sess := eng.NewSession(nil)
	t.Cleanup(sess.Close)
	sess.runner = eng.client.Beta.Messages.NewToolRunnerStreaming(sess.betaTools, anthropic.BetaToolRunnerParams{
		BetaMessageNewParams: sess.baseParams(nil),
	})
	return sess
}

func drainEvents(sess *Session) []Event {
	var out []Event
	for {
		select {
		case ev, ok := <-sess.events:
			if !ok {
				return out
			}
			out = append(out, ev)
		default:
			return out
		}
	}
}

func TestMaybeCompactThresholdPredicate(t *testing.T) {
	sess := newTestSessionForRunner(t, Config{CompactThreshold: 0.5, ContextWindow: 1000})

	// 40% of window: below threshold.
	if sess.maybeCompact(Usage{Input: 400}) {
		t.Fatal("triggered below threshold")
	}
	// 60%: crosses — the caller decides to defer or compact immediately.
	if !sess.maybeCompact(Usage{Input: 600}) {
		t.Fatal("did not trigger at threshold")
	}
	// Progress guard: compactNow records the triggering total; an equal
	// total must not re-trigger (a failed compaction cannot loop).
	sess.lastCompactInput = 600
	if sess.maybeCompact(Usage{Input: 600}) {
		t.Fatal("re-triggered at same total")
	}
	if !sess.maybeCompact(Usage{Input: 900}) {
		t.Fatal("did not trigger on growth past the guard")
	}
	// The predicate is silent — compaction announces when it RUNS.
	if evs := drainEvents(sess); len(evs) != 0 {
		t.Fatalf("unexpected events: %+v", evs)
	}
}

func TestMaybeCompactDisabled(t *testing.T) {
	sess := newTestSessionForRunner(t, Config{CompactThreshold: -1, ContextWindow: 1000})
	if sess.maybeCompact(Usage{Input: 999999}) {
		t.Fatal("negative threshold must disable compaction")
	}
	if evs := drainEvents(sess); len(evs) != 0 {
		t.Fatalf("unexpected events: %+v", evs)
	}
}

func TestIsCompactionMessage(t *testing.T) {
	if isCompactionMessage(nil) {
		t.Error("nil message")
	}
	plain := &anthropic.BetaMessage{Content: []anthropic.BetaContentBlockUnion{
		{Type: "text", Text: "hi"},
	}}
	if isCompactionMessage(plain) {
		t.Error("text message misdetected")
	}
	compact := &anthropic.BetaMessage{Content: []anthropic.BetaContentBlockUnion{
		{Type: "compaction"},
	}}
	if !isCompactionMessage(compact) {
		t.Error("compaction message not detected")
	}
}

func TestBuildHistoryInsertsSystemNote(t *testing.T) {
	sess := newTestSessionForRunner(t, Config{})

	// First prompt, no note: just the user message.
	hist := sess.buildHistory("", "hello")
	if len(hist) != 1 || hist[0].Role != anthropic.BetaMessageParamRoleUser {
		t.Fatalf("plain history: %+v", hist)
	}

	// Note at the turn boundary: role:"system" between history and prompt.
	hist = sess.buildHistory("workspace=/tmp; time=noon", "do the thing")
	if len(hist) != 2 {
		t.Fatalf("want 2 messages, got %d: %+v", len(hist), hist)
	}
	if hist[0].Role != anthropic.BetaMessageParamRoleSystem {
		t.Fatalf("first message role = %q, want system", hist[0].Role)
	}
	if hist[1].Role != anthropic.BetaMessageParamRoleUser {
		t.Fatalf("second message role = %q, want user", hist[1].Role)
	}

	// Pending-note queue: consumed exactly once.
	sess.PushSystemNote("dynamic context")
	if got := sess.takePendingNote(); got != "dynamic context" {
		t.Fatalf("take = %q", got)
	}
	if got := sess.takePendingNote(); got != "" {
		t.Fatalf("second take = %q, want empty", got)
	}
}

func TestSubagentToolRegisteredAndGuarded(t *testing.T) {
	eng := NewEngine(Config{}, nil)
	tool, ok := eng.Registry().Get("run_subagent")
	if !ok {
		t.Fatal("run_subagent not registered")
	}
	if tool.ParallelSafe() {
		t.Error("run_subagent must be serial (it is a whole API loop)")
	}
	props, ok := tool.InputSchema().Properties.(map[string]any)
	if !ok || len(props) == 0 {
		t.Fatalf("schema properties: %#v", tool.InputSchema().Properties)
	}
	if _, hasTask := props["task"]; !hasTask {
		t.Errorf("schema missing task property: %#v", props)
	}

	// Outside a driving session there is no engine context to run against.
	if _, err := tool.Execute(t.Context(), []byte(`{"task":"x"}`)); err == nil {
		t.Fatal("Execute without a session must fail")
	}
}
