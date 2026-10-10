package agent

import (
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"lyon-agent/internal/mockapi"
)

// TestRestoreMidSession pins `/resume` semantics: Restore swaps a live
// session's history for the snapshot — the next prompt continues from the
// restored messages (the pre-restore runner history must not leak), and
// turn numbering continues at the snapshot's turn count instead of
// restarting. Compaction is disabled here so the history under test stays
// literal.
func TestRestoreMidSession(t *testing.T) {
	newCompactMock(t, []mockapi.Turn{
		{Text: []string{"LIVE-MARKER: before restore"}},
		{Text: []string{"after restore"}},
	})
	cfg := DefaultConfig()
	cfg.CompactThreshold = -1
	cfg.Workspace = tempDir(t)
	eng := NewEngine(cfg, nil)
	sess := eng.NewSession(nil)
	defer sess.Close()

	// Build live history so a completed runner exists when Restore runs.
	if err := sess.Submit("LIVE-MARKER: before restore"); err != nil {
		t.Fatal(err)
	}
	if pe, _ := waitPromptEndCollect(t, sess); pe.Err != "" {
		t.Fatalf("first prompt failed: %s", pe.Err)
	}

	// A snapshot from an earlier conversation: one message, seven turns.
	snap := Snapshot{
		Version:   snapshotVersion,
		Workspace: cfg.Workspace,
		Model:     cfg.Model,
		Messages: []anthropic.BetaMessageParam{{
			Role: anthropic.BetaMessageParamRoleUser,
			Content: []anthropic.BetaContentBlockParamUnion{{OfText: &anthropic.BetaTextBlockParam{
				Text: "RESTORED-MARKER: earlier session",
			}}},
		}},
	}
	for i := 1; i <= 7; i++ {
		snap.Turns = append(snap.Turns, TurnStat{Turn: i, StopReason: "end_turn"})
	}
	sess.Restore(snap)

	if err := sess.Submit("continue after restore"); err != nil {
		t.Fatal(err)
	}
	if pe, _ := waitPromptEndCollect(t, sess); pe.Err != "" {
		t.Fatalf("post-restore prompt failed: %s", pe.Err)
	}

	// Turn accounting continues from the snapshot (7 turns → the new one
	// is 8), not from what the live session burned before the restore.
	stats := sess.Stats()
	if len(stats) != 8 || stats[7].Turn != 8 {
		turns := make([]int, len(stats))
		for i, st := range stats {
			turns[i] = st.Turn
		}
		t.Fatalf("turns after restore = %v, want snapshot's 7 plus one turn numbered 8", turns)
	}

	out := sess.Export()
	var texts []string
	for _, m := range out.Messages {
		for _, b := range m.Content {
			if b.OfText != nil {
				texts = append(texts, b.OfText.Text)
			}
		}
	}
	joined := strings.Join(texts, "\n")
	if !strings.Contains(joined, "RESTORED-MARKER") {
		t.Fatalf("restored message missing from the next build:\n%.500s", joined)
	}
	if !strings.Contains(joined, "continue after restore") {
		t.Fatalf("new prompt missing from the next build:\n%.500s", joined)
	}
	if strings.Contains(joined, "LIVE-MARKER") {
		t.Fatalf("pre-restore live history leaked into the next build:\n%.500s", joined)
	}
}

// TestResetStartsFresh: `/new` semantics — Reset wipes history, turn stats
// and pending state; the next prompt runs on an empty conversation with
// numbering restarted at 1.
func TestResetStartsFresh(t *testing.T) {
	newCompactMock(t, []mockapi.Turn{
		{Text: []string{"FIRST-MARKER: before reset"}},
		{Text: []string{"second answer"}},
	})
	cfg := DefaultConfig()
	cfg.CompactThreshold = -1
	cfg.Workspace = tempDir(t)
	eng := NewEngine(cfg, nil)
	sess := eng.NewSession(nil)
	defer sess.Close()

	if err := sess.Submit("FIRST-MARKER: before reset"); err != nil {
		t.Fatal(err)
	}
	if pe, _ := waitPromptEndCollect(t, sess); pe.Err != "" {
		t.Fatalf("first prompt failed: %s", pe.Err)
	}
	if len(sess.Stats()) == 0 || len(sess.Export().Messages) == 0 {
		t.Fatal("precondition: expected turns and history before Reset")
	}

	sess.Reset()
	if n := len(sess.Stats()); n != 0 {
		t.Fatalf("Reset left %d turns", n)
	}
	if n := len(sess.Export().Messages); n != 0 {
		t.Fatalf("Reset left %d messages", n)
	}

	if err := sess.Submit("a clean start"); err != nil {
		t.Fatal(err)
	}
	if pe, _ := waitPromptEndCollect(t, sess); pe.Err != "" {
		t.Fatalf("post-reset prompt failed: %s", pe.Err)
	}
	stats := sess.Stats()
	if len(stats) != 1 || stats[0].Turn != 1 {
		t.Fatalf("turn numbering after Reset = %+v, want a single turn 1", stats)
	}
	out := sess.Export()
	var texts []string
	for _, m := range out.Messages {
		for _, b := range m.Content {
			if b.OfText != nil {
				texts = append(texts, b.OfText.Text)
			}
		}
	}
	joined := strings.Join(texts, "\n")
	if strings.Contains(joined, "FIRST-MARKER") {
		t.Fatalf("pre-reset history survived Reset:\n%.500s", joined)
	}
	if !strings.Contains(joined, "a clean start") {
		t.Fatalf("post-reset prompt missing:\n%.500s", joined)
	}
}
