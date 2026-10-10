package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"lyon-agent/pkg/tools"
)

// Session snapshots: Export/Restore for in-process handoff, Save/Load for
// the on-disk store `lyon chat -c` resumes from. The SDK's message params
// all implement Marshal/UnmarshalJSON, so snapshots are plain JSON.

// snapshotVersion bumps when the Snapshot layout changes incompatibly.
const snapshotVersion = 1

// Snapshot is one conversation's persistable state.
type Snapshot struct {
	Version            int
	Workspace          string
	Model              string
	SavedAt            time.Time
	Messages           []anthropic.BetaMessageParam
	PendingToolResults []anthropic.BetaContentBlockParamUnion
	Turns              []TurnStat
}

// Export captures the conversation history. histMu makes it wait for an
// in-flight prompt to finish its bookkeeping, so at exit call Interrupt
// first and then Export — you get one stable, fully-repaired history.
func (s *Session) Export() Snapshot {
	s.histMu.Lock()
	defer s.histMu.Unlock()
	var msgs []anthropic.BetaMessageParam
	switch {
	case s.runner != nil:
		msgs = s.runner.Messages()
	case s.seed != nil:
		msgs = append([]anthropic.BetaMessageParam(nil), s.seed...)
	}
	return Snapshot{
		Version:            snapshotVersion,
		Workspace:          s.eng.cfg.Workspace,
		Model:              s.eng.cfg.Model,
		SavedAt:            time.Now(),
		Messages:           msgs,
		PendingToolResults: append([]anthropic.BetaContentBlockParamUnion(nil), s.pendingToolResults...),
		Turns:              s.Stats(),
	}
}

// Restore replaces the session's conversation with a saved snapshot. It is
// safe mid-session (`/resume`), not just at startup: a live runner is
// dropped so the next buildHistory reads the restored seed, compaction
// bookkeeping resets so the restored history re-evaluates the threshold,
// and the workspace memory snapshot refreshes. The caller must Interrupt an
// in-flight prompt and let prompt_end land first — Restore waits on histMu,
// which drive holds for a whole prompt. Lock order: histMu before mu.
func (s *Session) Restore(snap Snapshot) {
	s.histMu.Lock()
	defer s.histMu.Unlock()
	s.seed = append([]anthropic.BetaMessageParam(nil), snap.Messages...)
	s.pendingToolResults = append([]anthropic.BetaContentBlockParamUnion(nil), snap.PendingToolResults...)
	s.runner = nil // pre-restore history must not leak into the next build
	s.compactArmed = false
	s.lastCompactInput = 0 // progress guard tracks the restored history
	s.memoryText = tools.MemoryFacts(s.eng.cfg.Workspace)
	s.memoryInjected = false
	s.mu.Lock()
	s.turns = append([]TurnStat(nil), snap.Turns...)
	s.mu.Unlock()
}

// Reset clears the conversation back to fresh-session state — history,
// pending tool results, turn stats, compaction bookkeeping; the next
// prompt re-injects workspace memory. Snapshots on disk are untouched, and
// because SaveSnapshot skips empty histories, quitting right after Reset
// preserves the previously saved session. Safe mid-session like Restore
// (callers must settle an in-flight prompt first).
func (s *Session) Reset() {
	s.Restore(Snapshot{Workspace: s.eng.cfg.Workspace})
}

// lyonHome is the store root: $LYON_HOME when set (containers, CI, tests —
// anywhere the real home directory must not be touched), else ~/.lyon.
// Read on every call so environment changes take effect immediately.
func lyonHome() (string, error) {
	if h := os.Getenv("LYON_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".lyon"), nil
}

// SnapshotPath is where a workspace's chat snapshot lives:
// $LYON_HOME/sessions/<sha256(abs workspace)[:16]>.json, defaulting to
// ~/.lyon/sessions/.
func SnapshotPath(workspace string) (string, error) {
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(filepath.Clean(abs)))
	root, err := lyonHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "sessions", hex.EncodeToString(sum[:16])+".json"), nil
}

// archiveKeep bounds the per-workspace archive (the latest slot sits
// outside it): every save that changes the conversation rotates the old
// latest into the archive, so pruning here bounds total disk usage.
const archiveKeep = 20

// sessionDir is the per-workspace archive directory — SnapshotPath minus
// the .json suffix, i.e. $LYON_HOME/sessions/<hash16>/.
func sessionDir(workspace string) (string, error) {
	p, err := SnapshotPath(workspace)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(p, ".json"), nil
}

// sameHistory reports whether two message histories are byte-identical as
// JSON (BetaMessageParam marshals deterministically).
func sameHistory(a, b []anthropic.BetaMessageParam) bool {
	ja, err := json.Marshal(a)
	if err != nil {
		return false
	}
	jb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return string(ja) == string(jb)
}

// storedElsewhere reports whether snap's history already lives in the
// workspace store — the latest slot or any archive entry. Both SaveSnapshot
// and /new consult it: re-saving an unchanged conversation (resume + quit)
// or archiving one that was never edited must not duplicate entries.
func storedElsewhere(snap Snapshot) (bool, error) {
	refs, err := ListSnapshots(snap.Workspace)
	if err != nil {
		return false, err
	}
	for _, r := range refs {
		old, err := LoadSnapshotFrom(r.Path)
		if err != nil || old == nil {
			continue
		}
		if sameHistory(old.Messages, snap.Messages) {
			return true, nil
		}
	}
	return false, nil
}

// writeArchive files snap into the workspace's archive directory under its
// SavedAt timestamp (millisecond precision plus a collision suffix) and
// prunes past archiveKeep. It never touches the latest slot.
func writeArchive(workspace string, snap Snapshot) error {
	dir, err := sessionDir(workspace)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	base := snap.SavedAt.Format("20060102-150405.000")
	for i := 0; ; i++ {
		name := base + ".json"
		if i > 0 {
			name = fmt.Sprintf("%s-%d.json", base, i)
		}
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err == nil {
			continue // same-millisecond collision — try a suffix
		}
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, data, 0o600); err != nil {
			return err
		}
		if err := os.Rename(tmp, path); err != nil {
			return err
		}
		return pruneArchive(dir)
	}
}

// pruneArchive keeps the newest archiveKeep entries (timestamped names
// sort chronologically) and best-effort removes the rest.
func pruneArchive(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	if len(names) <= archiveKeep {
		return nil
	}
	sort.Strings(names)
	for _, n := range names[:len(names)-archiveKeep] {
		_ = os.Remove(filepath.Join(dir, n))
	}
	return nil
}

// ArchiveSnapshot files snap into the workspace archive without touching
// the latest slot — the store keeps one entry per distinct conversation.
func ArchiveSnapshot(snap Snapshot) error {
	if len(snap.Messages) == 0 {
		return nil
	}
	return writeArchive(snap.Workspace, snap)
}

// ArchiveIfNew exports the live conversation and archives it unless it is
// empty or already stored anywhere — what /new calls before wiping, so an
// abandoned conversation stays resumable instead of dying with the session.
func (s *Session) ArchiveIfNew() (bool, error) {
	snap := s.Export()
	if len(snap.Messages) == 0 {
		return false, nil
	}
	stored, err := storedElsewhere(snap)
	if err != nil || stored {
		return false, err
	}
	if err := writeArchive(s.eng.cfg.Workspace, snap); err != nil {
		return false, err
	}
	return true, nil
}

// SnapshotRef is one entry of a workspace's session store: where it lives
// plus enough metadata to list it without a full restore.
type SnapshotRef struct {
	Path     string
	SavedAt  time.Time
	Messages int
}

// ListSnapshots returns every saved session for a workspace — latest slot
// and archive together — newest first by SavedAt.
func ListSnapshots(workspace string) ([]SnapshotRef, error) {
	var refs []SnapshotRef
	latestPath, err := SnapshotPath(workspace)
	if err != nil {
		return nil, err
	}
	latest, err := LoadSnapshotFrom(latestPath)
	if err != nil {
		return nil, err
	}
	if latest != nil {
		refs = append(refs, SnapshotRef{Path: latestPath, SavedAt: latest.SavedAt, Messages: len(latest.Messages)})
	}
	dir, err := sessionDir(workspace)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		s, err := LoadSnapshotFrom(p)
		if err != nil || s == nil {
			continue // one corrupt archive entry must not hide the rest
		}
		refs = append(refs, SnapshotRef{Path: p, SavedAt: s.SavedAt, Messages: len(s.Messages)})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].SavedAt.After(refs[j].SavedAt) })
	return refs, nil
}

// SaveSnapshot persists snap as the workspace's latest slot, atomically
// (tmp + rename). An unchanged conversation is not rewritten; a changed one
// rotates the previous latest into the archive first, so every distinct
// conversation stays resumable — nothing is ever overwritten away.
func SaveSnapshot(snap Snapshot) error {
	if len(snap.Messages) == 0 {
		return nil // nothing to resume — never write an empty file
	}
	path, err := SnapshotPath(snap.Workspace)
	if err != nil {
		return err
	}
	stored, err := storedElsewhere(snap)
	if err != nil {
		return err
	}
	if stored {
		return nil // resume + quit (or any no-op save): no duplicate, no churn
	}
	if old, err := LoadSnapshotFrom(path); err != nil {
		return err
	} else if old != nil {
		if err := writeArchive(snap.Workspace, *old); err != nil {
			return err // preserve the old conversation before overwriting
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadSnapshot reads a workspace's latest snapshot. (nil, nil) when no
// snapshot exists yet; a corrupt or unreadable file is an error the caller
// should surface without crashing.
func LoadSnapshot(workspace string) (*Snapshot, error) {
	path, err := SnapshotPath(workspace)
	if err != nil {
		return nil, err
	}
	return LoadSnapshotFrom(path)
}

// LoadSnapshotFrom reads one snapshot file — the latest slot or any archive
// entry. (nil, nil) when missing or empty; corrupt content is an error.
func LoadSnapshotFrom(path string) (*Snapshot, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("snapshot %s: %w", path, err)
	}
	if len(snap.Messages) == 0 {
		return nil, nil
	}
	return &snap, nil
}
