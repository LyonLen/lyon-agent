package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Live drafts (#45): a live session mirrors its conversation to a
// per-session draft file at every prompt_end, so a hard kill — taskkill /F,
// OOM, power loss — costs at most the prompt that was running. Drafts live
// one level below the workspace archive (sessions/<hash16>/drafts/), where
// ListSnapshots' archive scan ignores them: a live session must not appear
// twice (live row + saved row). A draft only becomes a saved session when
// the next lyond startup sweeps it (SweepDrafts); a clean exit archives the
// final state itself and drops the draft.

// draftsDir is the live-draft directory for a workspace.
func draftsDir(workspace string) (string, error) {
	dir, err := sessionDir(workspace)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "drafts"), nil
}

// DraftPath is the draft slot for one live session id — stable for the
// session's lifetime, so every prompt_end overwrites the same file.
func DraftPath(workspace, id string) (string, error) {
	dir, err := draftsDir(workspace)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, id+".json"), nil
}

// WriteSnapshotFile atomically replaces path with snap. A draft is a fixed
// path rewritten in place — no dedupe or rotation, which is SaveSnapshot's
// job on the exit path.
func WriteSnapshotFile(path string, snap Snapshot) error {
	if len(snap.Messages) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data)
}

// Draft mirrors a live session's conversation to a fixed path. Touch
// schedules a write and returns immediately: the hub calls it from its
// consumer goroutine, and Export blocks on histMu until the prompt that
// just ended releases the history — so the write itself runs on its own
// goroutine. At most one write is in flight per session; Touches arriving
// meanwhile collapse into one follow-up write.
type Draft struct {
	sess *Session
	path string

	mu      sync.Mutex
	dirty   bool
	running bool
	closed  bool
	wg      sync.WaitGroup
}

// NewDraft starts mirroring the session to path (which need not exist yet).
// Writes happen only on Touch — the caller decides which history states are
// stable enough to persist.
func (s *Session) NewDraft(path string) *Draft { return &Draft{sess: s, path: path} }

// Workspace is the session's confinement root — the same workspace its
// snapshot store lives under.
func (s *Session) Workspace() string { return s.eng.cfg.Workspace }

// Path is the draft file's location; teardown removes it once the final
// archive has landed.
func (d *Draft) Path() string { return d.path }

// Touch schedules a mirror write. Never blocks; a Touch after Close is a
// no-op.
func (d *Draft) Touch() {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.dirty = true
	if d.running {
		d.mu.Unlock()
		return
	}
	d.running = true
	d.wg.Add(1)
	d.mu.Unlock()
	go d.run()
}

func (d *Draft) run() {
	defer d.wg.Done()
	for {
		d.mu.Lock()
		if !d.dirty {
			d.running = false
			d.mu.Unlock()
			return
		}
		// Close only stops *new* mirror requests: a write already scheduled
		// still lands, so the last Touch before teardown is never lost.
		d.dirty = false
		d.mu.Unlock()
		// Best effort, like the exit-time archive: a disk error must not
		// wedge the session, and the next prompt_end retries anyway.
		_ = WriteSnapshotFile(d.path, d.sess.Export())
	}
}

// Close stops mirroring and waits for the in-flight write, so the caller
// can archive the final state and remove Path without racing a straggler.
func (d *Draft) Close() {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	d.wg.Wait()
}

// SweepDrafts archives every draft left in workspace and removes it — lyond
// runs it once at startup, before it serves a list. A draft only survives a
// process that died hard, so each one is a conversation the previous run
// never saved. Content already in the store is deduped away (the archive may
// have landed just before the kill). A draft that fails to archive is kept
// for the next startup; N counts recovered conversations.
func SweepDrafts(workspace string) (int, error) {
	dir, err := draftsDir(workspace)
	if err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	recovered := 0
	var firstErr error
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, ent.Name())
		snap, err := LoadSnapshotFrom(path)
		if err != nil {
			if firstErr == nil {
				firstErr = err // keep the file: it may be the only copy
			}
			continue
		}
		if snap != nil {
			stored, err := storedElsewhere(*snap)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue // keep the draft; retry on the next startup
			}
			if !stored {
				if err := writeArchive(workspace, *snap); err != nil {
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
				recovered++
			}
		}
		// Archived now or already stored: nothing unique is left in it.
		_ = os.Remove(path)
	}
	_ = os.Remove(dir) // best effort — succeeds only once empty
	return recovered, firstErr
}
