package agent

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// settleTestMsgs is a two-turn conversation: a user prompt and its answer.
func settleTestMsgs() []anthropic.BetaMessageParam {
	return []anthropic.BetaMessageParam{
		anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("hello")),
		{
			Role: anthropic.BetaMessageParamRoleAssistant,
			Content: []anthropic.BetaContentBlockParamUnion{
				{OfText: &anthropic.BetaTextBlockParam{Text: "hi"}},
			},
		},
	}
}

// TestDraftMirrorDoesNotWaitOnHistMu pins the durability contract: a
// mirror write must land while histMu is held by a running prompt. Before
// settleLocked existed, the writer called Export, which blocked on histMu
// until prompt_end — a hard kill during a session's first prompt then left
// nothing on disk at all (no draft, no dump, no archive).
func TestDraftMirrorDoesNotWaitOnHistMu(t *testing.T) {
	eng := NewEngine(Config{Workspace: t.TempDir()}, nil)
	sess := eng.NewSession(nil)
	defer sess.Close()
	sess.Restore(Snapshot{Messages: settleTestMsgs()})

	path := t.TempDir() + "/draft.json"
	d := sess.NewDraft(path)
	defer d.Close()

	// Simulate a prompt in flight: the drive goroutine holds histMu for a
	// whole prompt. The write must still land.
	sess.histMu.Lock()
	d.Touch()
	deadline := time.Now().Add(3 * time.Second)
	for {
		snap, err := LoadSnapshotFrom(path)
		if err != nil {
			sess.histMu.Unlock()
			t.Fatalf("draft unreadable: %v", err)
		}
		if snap != nil && len(snap.Messages) == 2 {
			sess.histMu.Unlock()
			break
		}
		if time.Now().After(deadline) {
			sess.histMu.Unlock()
			t.Fatal("draft did not land while histMu was held — the mirror waits on the prompt again")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestSettlePairsDanglingToolUse: a settled snapshot whose history ends in
// an assistant tool_use carries synthesized tool_results as
// PendingToolResults — the same resume-safe pair repairInterruptedHistory
// builds in-process, so a mid-prompt kill never persists a history the API
// would reject on resume.
func TestSettlePairsDanglingToolUse(t *testing.T) {
	eng := NewEngine(Config{Workspace: t.TempDir()}, nil)
	sess := eng.NewSession(nil)
	defer sess.Close()

	msgs := []anthropic.BetaMessageParam{
		anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("run it")),
		{
			Role: anthropic.BetaMessageParamRoleAssistant,
			Content: []anthropic.BetaContentBlockParamUnion{
				{OfToolUse: &anthropic.BetaToolUseBlockParam{ID: "toolu_1", Name: "run_shell", Input: json.RawMessage(`{"command":"ls"}`)}},
			},
		},
	}
	// No runner yet: settleLocked reads the seed, exactly like a prompt
	// that was killed between its first tool_use turn and its results.
	sess.Restore(Snapshot{Messages: msgs})
	sess.histMu.Lock()
	sess.settleLocked()
	snap := sess.Settled()
	sess.histMu.Unlock()

	if len(snap.Messages) != 2 {
		t.Fatalf("settled messages = %d, want 2", len(snap.Messages))
	}
	if len(snap.PendingToolResults) != 1 {
		t.Fatalf("settled pending = %d, want 1 synthesized tool_result", len(snap.PendingToolResults))
	}
	res := snap.PendingToolResults[0].OfToolResult
	if res == nil || res.ToolUseID != "toolu_1" {
		t.Fatalf("synthesized result does not pair with toolu_1: %+v", snap.PendingToolResults[0])
	}

	// The pair round-trips: Restore carries both halves, so the next
	// buildHistory can merge them like the in-process repair would.
	sess2 := eng.NewSession(nil)
	defer sess2.Close()
	sess2.Restore(snap)
	if len(sess2.pendingToolResults) != 1 {
		t.Fatalf("restore lost the synthesized results: %d", len(sess2.pendingToolResults))
	}
}

// TestRunEndsRoundTrip: prompt_end records a run anchor (duration + the
// settled message count it ended at), and Settled/Export/Restore all carry
// it — the exact data an adopted seat re-emits the reply's duration badge
// from after a restart. The count must match the settled messages beside
// it: replay anchors prompt_end to that index.
func TestRunEndsRoundTrip(t *testing.T) {
	eng := NewEngine(Config{Workspace: t.TempDir()}, nil)
	sess := eng.NewSession(nil)
	defer sess.Close()
	sess.Restore(Snapshot{Messages: settleTestMsgs()})

	// Same call order as drive's prompt_end: settle first, then record —
	// the anchor's count is len(settledMsgs), what the mirror persists.
	sess.histMu.Lock()
	sess.settleLocked()
	sess.recordRunEnd(Event{Type: EventPromptEnd, DurationMS: 1234, StopReason: "end_turn"})
	sess.histMu.Unlock()

	snap := sess.Settled()
	if len(snap.RunEnds) != 1 {
		t.Fatalf("settled RunEnds = %d, want 1", len(snap.RunEnds))
	}
	re := snap.RunEnds[0]
	if re.DurationMS != 1234 || re.Messages != 2 || re.StopReason != "end_turn" || re.Err != "" {
		t.Fatalf("RunEnd = %+v, want {1234 2 end_turn \"\"}", re)
	}
	if re.Messages != len(snap.Messages) {
		t.Fatalf("anchor Messages=%d does not match settled len=%d — replay would land on the wrong reply",
			re.Messages, len(snap.Messages))
	}

	// Export (exit path) and Restore (adoption) carry the same anchors.
	if got := sess.Export().RunEnds; len(got) != 1 || got[0].DurationMS != 1234 {
		t.Fatalf("export RunEnds = %+v, want the recorded run", got)
	}
	sess2 := eng.NewSession(nil)
	defer sess2.Close()
	sess2.Restore(snap)
	if got := sess2.Export().RunEnds; len(got) != 1 || got[0].Messages != 2 {
		t.Fatalf("restored RunEnds = %+v, want the round-tripped run", got)
	}

	// Reset clears the anchors with the history: a fresh conversation must
	// not replay badges from the old one.
	sess2.Reset()
	if got := sess2.Settled().RunEnds; len(got) != 0 {
		t.Fatalf("after Reset RunEnds = %+v, want none", got)
	}
}
