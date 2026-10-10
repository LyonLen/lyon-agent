package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"lyon-agent/pkg/hooks"
	"lyon-agent/pkg/tools"
)

// Rollback: undo points over the conversation history.
//
// A checkpoint is a full JSON copy of the messages at a boundary the user
// can return to — before each prompt that had history to lose, and right
// before compaction replaces it (so a compact can always be undone).
// Restoring swaps the live history for the copy: the runner is dropped and
// the next prompt rebuilds from the seed, exactly like /resume.
//
// Locking: capture runs under histMu (drive / compactNow hold it for a
// whole prompt). Listing takes only cpMu so the UI never blocks on a live
// prompt; restore takes histMu first (lock order histMu → cpMu). The TUI
// must Interrupt and wait for prompt_end before restoring, same as /resume.

// Checkpoint kinds.
const (
	CheckpointPrompt  = "prompt"  // captured before a prompt entered history
	CheckpointCompact = "compact" // captured before compaction replaced history
	CheckpointManual  = "manual"  // user-marked undo point (Session.Checkpoint)
)

// maxCheckpoints bounds the undo stack. Compact checkpoints evict last:
// undoing a compaction is the one case where the oldest state matters most.
const maxCheckpoints = 20

// CheckpointInfo is the user-visible description of one undo point.
type CheckpointInfo struct {
	ID       int
	Kind     string // CheckpointPrompt | CheckpointCompact
	Label    string // prompt excerpt / "before compact (N msgs)"
	At       time.Time
	Messages int // history length at capture
}

// checkpoint is the stored undo point: message + pending-state payloads as
// raw JSON (BetaMessageParam round-trips losslessly — the snapshot store
// proves it) plus the label metadata.
type checkpoint struct {
	CheckpointInfo
	raw     []byte // marshaled []BetaMessageParam
	pending []byte // marshaled pendingToolResults (nil when empty)
	note    string // queued system note at capture
}

// currentHistoryLocked returns the live conversation history. histMu held.
func (s *Session) currentHistoryLocked() []anthropic.BetaMessageParam {
	switch {
	case s.runner != nil:
		return s.runner.Messages()
	case s.seed != nil:
		return s.seed
	}
	return nil
}

// captureCheckpoint stores msgs as an undo point. histMu held. Best-effort:
// a marshal failure skips the checkpoint rather than failing the prompt.
// Returns the stored info (zero value when skipped).
func (s *Session) captureCheckpoint(kind, label string, msgs []anthropic.BetaMessageParam) CheckpointInfo {
	if len(msgs) == 0 {
		return CheckpointInfo{} // nothing to roll back to (fresh session)
	}
	raw, err := json.Marshal(msgs)
	if err != nil {
		return CheckpointInfo{}
	}
	var pend []byte
	if len(s.pendingToolResults) > 0 {
		if p, err := json.Marshal(s.pendingToolResults); err == nil {
			pend = p
		}
	}
	s.cpMu.Lock()
	s.checkpointSeq++
	cp := checkpoint{
		CheckpointInfo: CheckpointInfo{
			ID:       s.checkpointSeq,
			Kind:     kind,
			Label:    label,
			At:       time.Now(),
			Messages: len(msgs),
		},
		raw:     raw,
		pending: pend,
		note:    s.pendingNote,
	}
	s.checkpoints = append(s.checkpoints, cp)
	s.evictCheckpointsLocked()
	s.cpMu.Unlock()
	return cp.CheckpointInfo
}

// Checkpoint captures the current history as a user-marked undo point —
// mark one before a risky stretch of work, then /rollback back to it.
// Blocks only briefly (it copies the history; no prompt wait beyond the
// usual histMu semantics). Empty histories have nothing to roll back to.
func (s *Session) Checkpoint(label string) (CheckpointInfo, error) {
	s.histMu.Lock()
	defer s.histMu.Unlock()
	msgs := s.currentHistoryLocked()
	if len(msgs) == 0 {
		return CheckpointInfo{}, fmt.Errorf("checkpoint: no history yet")
	}
	label = strings.TrimSpace(label)
	if label == "" {
		label = fmt.Sprintf("manual checkpoint (%d msgs)", len(msgs))
	}
	return s.captureCheckpoint(CheckpointManual, clipRunes(label, 64), msgs), nil
}

// evictCheckpointsLocked trims the oldest checkpoints past the cap,
// preferring prompt checkpoints so compact undo points survive longest.
// cpMu held.
func (s *Session) evictCheckpointsLocked() {
	for len(s.checkpoints) > maxCheckpoints {
		victim := 0 // default: oldest (only right when every entry is compact)
		for i, cp := range s.checkpoints {
			if cp.Kind != CheckpointCompact {
				victim = i // oldest prompt checkpoint
				break
			}
		}
		s.checkpoints = append(s.checkpoints[:victim], s.checkpoints[victim+1:]...)
	}
}

// Checkpoints lists undo points newest first. Takes only cpMu, so a list
// request never waits on an in-flight prompt.
func (s *Session) Checkpoints() []CheckpointInfo {
	s.cpMu.Lock()
	defer s.cpMu.Unlock()
	out := make([]CheckpointInfo, 0, len(s.checkpoints))
	for i := len(s.checkpoints) - 1; i >= 0; i-- {
		out = append(out, s.checkpoints[i].CheckpointInfo)
	}
	return out
}

// Rollback restores the history captured at checkpoint id and discards
// every checkpoint taken after it (those states no longer exist). Turn
// stats, usage and the title are kept — the spend happened; only the
// conversation rewinds. Blocks until any in-flight prompt settles (histMu).
func (s *Session) Rollback(id int) (CheckpointInfo, error) {
	s.histMu.Lock()
	defer s.histMu.Unlock()

	s.cpMu.Lock()
	idx := -1
	for i, cp := range s.checkpoints {
		if cp.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		s.cpMu.Unlock()
		return CheckpointInfo{}, fmt.Errorf("rollback: checkpoint #%d no longer exists", id)
	}
	cp := s.checkpoints[idx]
	s.cpMu.Unlock()

	var msgs []anthropic.BetaMessageParam
	if err := json.Unmarshal(cp.raw, &msgs); err != nil {
		return CheckpointInfo{}, fmt.Errorf("rollback: checkpoint #%d is unreadable: %w", id, err)
	}
	var pend []anthropic.BetaContentBlockParamUnion
	if len(cp.pending) > 0 {
		_ = json.Unmarshal(cp.pending, &pend) // best effort: empty is valid
	}

	s.seed = msgs
	s.pendingToolResults = pend
	s.pendingNote = cp.note
	s.runner = nil // pre-checkpoint runner must not leak into the next build
	s.compactArmed = false
	s.lastCompactInput = 0 // progress guard tracks the restored history
	s.memoryText = tools.MemoryFacts(s.eng.cfg.Workspace)
	s.memoryInjected = false // buildHistory's containment check avoids re-injecting

	s.cpMu.Lock()
	s.checkpoints = s.checkpoints[:idx+1] // drop states after the target
	s.cpMu.Unlock()

	s.eng.hooks.Fire(hooks.Record{
		Event:  hooks.EvRollback,
		Detail: fmt.Sprintf("restored checkpoint #%d (%s) — %d messages", cp.ID, cp.Label, cp.Messages),
	})
	return cp.CheckpointInfo, nil
}
