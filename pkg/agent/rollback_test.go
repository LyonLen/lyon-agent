package agent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"lyon-agent/internal/mockapi"
)

// TestPromptRollback: each prompt with history to lose captures a
// checkpoint; rolling back discards later prompts from the conversation
// while turn stats survive, and the next request replays only the
// restored history.
func TestPromptRollback(t *testing.T) {
	mock := mockapi.New(mockapi.Config{Script: []mockapi.Turn{
		{Text: []string{"answer-one"}},
		{Text: []string{"answer-two"}},
		{Text: []string{"answer-three"}},
		{Text: []string{"answer-four"}},
	}})
	t.Cleanup(mock.Close)
	t.Setenv("ANTHROPIC_BASE_URL", mock.URL())
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "rollback-test-token")

	eng := NewEngine(Config{Workspace: tempDir(t)}, nil)
	sess := eng.NewSession(nil)
	defer sess.Close()

	// First prompt: nothing to lose yet — no checkpoint for it.
	if err := sess.Submit("first question"); err != nil {
		t.Fatal(err)
	}
	if pe := waitEvent(t, sess, EventPromptEnd); pe.Err != "" {
		t.Fatalf("prompt 1: %s", pe.Err)
	}
	if cps := sess.Checkpoints(); len(cps) != 0 {
		t.Fatalf("checkpoints after empty-history prompt: %+v", cps)
	}

	for _, q := range []string{"second question", "third question"} {
		if err := sess.Submit(q); err != nil {
			t.Fatal(err)
		}
		if pe := waitEvent(t, sess, EventPromptEnd); pe.Err != "" {
			t.Fatalf("prompt %q: %s", q, pe.Err)
		}
	}
	cps := sess.Checkpoints()
	if len(cps) != 2 {
		t.Fatalf("checkpoints = %d, want 2 (before 2nd, before 3rd): %+v", len(cps), cps)
	}
	if !strings.Contains(cps[0].Label, "third question") || !strings.Contains(cps[1].Label, "second question") {
		t.Fatalf("checkpoint labels (newest first): %q / %q", cps[0].Label, cps[1].Label)
	}
	turnsBefore := len(sess.Stats())

	// Discard the third prompt: restore the state before it.
	info, err := sess.Rollback(cps[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != cps[0].ID {
		t.Fatalf("rollback returned #%d, want #%d", info.ID, cps[0].ID)
	}
	if len(sess.Stats()) != turnsBefore {
		t.Fatalf("turn stats changed by rollback: %d → %d", turnsBefore, len(sess.Stats()))
	}
	// Nothing sits after the target (it was newest); target + earlier
	// states remain, so the same point can be rolled back to again.
	if after := sess.Checkpoints(); len(after) != 2 || after[0].ID != cps[0].ID || after[1].ID != cps[1].ID {
		t.Fatalf("post-rollback checkpoints: %+v", after)
	}
	// Unknown id is a clean error, not a panic.
	if _, err := sess.Rollback(9999); err == nil {
		t.Fatal("rollback to a nonexistent checkpoint must fail")
	}

	// The next prompt must replay the restored history only.
	if err := sess.Submit("fourth question"); err != nil {
		t.Fatal(err)
	}
	if pe := waitEvent(t, sess, EventPromptEnd); pe.Err != "" {
		t.Fatalf("prompt 4: %s", pe.Err)
	}
	body := string(mock.LastBody())
	for _, want := range []string{"first question", "second question", "fourth question"} {
		if !strings.Contains(body, want) {
			t.Fatalf("post-rollback request missing %q:\n%.2000s", want, body)
		}
	}
	if strings.Contains(body, "third question") {
		t.Fatalf("rolled-back prompt leaked into history:\n%.2000s", body)
	}
}

// TestCompactRollback: compaction captures the full pre-compact history,
// and rolling back to it undoes the summary — the original conversation
// drives the next request again. This is the "rollback across a compact"
// requirement.
func TestCompactRollback(t *testing.T) {
	mock := newCompactMock(t, []mockapi.Turn{
		{Text: []string{"original-task-marker: implement the widget"}},
		{Text: []string{"THE-SUMMARY"}},
		{Text: []string{"ANSWER-AFTER-ROLLBACK"}},
	})
	eng := NewEngine(compactTestConfig(t), nil)
	sess := eng.NewSession(nil)
	defer sess.Close()

	if err := sess.Submit("original-task-marker: implement the widget"); err != nil {
		t.Fatal(err)
	}
	pe, comps := waitPromptEndCollect(t, sess)
	if pe.Err != "" {
		t.Fatalf("prompt: %s", pe.Err)
	}
	if len(comps) == 0 {
		t.Fatal("compaction did not run")
	}

	// The undo stack holds exactly the pre-compact history.
	cps := sess.Checkpoints()
	if len(cps) != 1 || cps[0].Kind != CheckpointCompact {
		t.Fatalf("checkpoints after compaction: %+v", cps)
	}
	if !strings.Contains(cps[0].Label, "before compact") {
		t.Fatalf("compact checkpoint label: %q", cps[0].Label)
	}

	// Roll back: summary gone, original conversation restored in full.
	if _, err := sess.Rollback(cps[0].ID); err != nil {
		t.Fatal(err)
	}
	snap := sess.Export()
	if len(snap.Messages) < 2 {
		t.Fatalf("restored history too short: %d messages", len(snap.Messages))
	}
	var joined strings.Builder
	for _, m := range snap.Messages {
		for _, b := range m.Content {
			if b.OfText != nil {
				joined.WriteString(b.OfText.Text)
				joined.WriteByte('\n')
			}
		}
	}
	got := joined.String()
	if !strings.Contains(got, "original-task-marker") {
		t.Fatalf("pre-compact history not restored:\n%.500s", got)
	}
	if strings.Contains(got, "THE-SUMMARY") || strings.Contains(got, "conversation compacted") {
		t.Fatalf("summary survived the rollback:\n%.500s", got)
	}

	// The follow-up prompt drives on the restored history (its request —
	// or the summarizer request that follows, threshold is tiny here —
	// renders the original conversation, never the discarded summary).
	if err := sess.Submit("continue after rollback"); err != nil {
		t.Fatal(err)
	}
	if pe := waitEvent(t, sess, EventPromptEnd); pe.Err != "" {
		t.Fatalf("post-rollback prompt: %s", pe.Err)
	}
	body := string(mock.LastBody())
	if !strings.Contains(body, "original-task-marker") {
		t.Fatalf("post-rollback request lost the original history:\n%.2000s", body)
	}
	if strings.Contains(body, "THE-SUMMARY") {
		t.Fatalf("discarded summary leaked into the request:\n%.2000s", body)
	}
}

// TestCheckpointEviction: the stack is capped, prompt checkpoints are
// evicted first, and compact undo points outlive them (undoing a compact
// is the case where the oldest state matters most).
func TestCheckpointEviction(t *testing.T) {
	eng := NewEngine(Config{Workspace: tempDir(t)}, nil)
	sess := eng.NewSession(nil)
	defer sess.Close()

	msg := func(i int) []anthropic.BetaMessageParam {
		return []anthropic.BetaMessageParam{
			anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(fmt.Sprintf("m%d", i))),
		}
	}
	sess.histMu.Lock()
	sess.captureCheckpoint(CheckpointCompact, "before compact", msg(0)) // oldest, sticky
	for i := 1; i <= maxCheckpoints+5; i++ {
		sess.captureCheckpoint(CheckpointPrompt, fmt.Sprintf("p%d", i), msg(i))
	}
	sess.histMu.Unlock()

	cps := sess.Checkpoints() // newest first
	if len(cps) != maxCheckpoints {
		t.Fatalf("checkpoints = %d, want cap %d", len(cps), maxCheckpoints)
	}
	if last := cps[len(cps)-1]; last.Kind != CheckpointCompact {
		t.Fatalf("oldest surviving checkpoint = %s, want the compact one: %+v", last.Kind, last)
	}
	// Prompt checkpoints evicted from the front: p1..p5 are gone.
	for _, cp := range cps {
		if cp.Kind == CheckpointPrompt && strings.HasPrefix(cp.Label, "p") {
			n := cp.Label[1:]
			if n == "1" || n == "2" || n == "3" || n == "4" || n == "5" {
				t.Fatalf("stale prompt checkpoint survived eviction: %q", cp.Label)
			}
		}
	}
}
