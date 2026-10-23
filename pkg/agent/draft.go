package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Live drafts: the dump IS the session. A live session mirrors its
// conversation into a per-session draft file on every state change, so a hard
// kill — taskkill /F, OOM, power loss — costs at most the turn that was
// running (the drive settles every completed turn), and a clean exit has
// nothing left to write. Drafts live one level below the workspace archive
// (sessions/<hash16>/drafts/), where ListSnapshots' archive scan ignores
// them: the seat list is the only place a conversation appears.
//
// They are not swept or archived. Each draft carries its seat id, so the next
// lyond startup indexes them (Store.LoadDrafts) and re-adopts one on first
// touch — same id, same conversation, one row. Archiving them at startup is
// what turned every restart into another SAVED row.

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
// schedules a write and returns immediately: the writer copies the last
// settled snapshot (Settled — mu-guarded, never histMu), so a write can
// land while a prompt is running. At most one write is in flight per
// session; Touches arriving meanwhile collapse into one follow-up write.
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
// Writes happen on Touch — either the caller's or the one every session
// state change now fires (notifyDirty): the dump on disk is the record, so
// the mirror follows the conversation as it moves, not just at prompt_end.
func (s *Session) NewDraft(path string) *Draft {
	d := &Draft{sess: s, path: path}
	s.mirror.Store(d)
	return d
}

// notifyDirty schedules a mirror write for every state change worth
// persisting. Non-blocking by construction: Touch only flips a flag and
// spawns the writer, so the drive loop, the title pass and the RPC path
// never wait on disk. A session with no draft (the TUI) is a no-op.
func (s *Session) notifyDirty() {
	if d := s.mirror.Load(); d != nil {
		d.Touch()
	}
}

// Workspace is the session's confinement root — the same workspace its
// snapshot store lives under.
func (s *Session) Workspace() string { return s.eng.cfg.Workspace }

// Path is the draft file's location — the seat's dump. Delete removes it
// with the seat; an unload writes the settled state to it once more.
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
		// wedge the session, and the next state change retries anyway.
		// Settled, not Export: the mirror must land even while histMu is
		// held by a running prompt (that wait is what used to erase a
		// first-prompt session whole on a hard kill).
		_ = WriteSnapshotFile(d.path, d.sess.Settled())
	}
}

// Close stops mirroring and waits for the in-flight write, so the caller can
// write the settled state (or remove Path) without racing a straggler.
func (d *Draft) Close() {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	d.wg.Wait()
}

// DraftRef is one dump found in the drafts directory: the seat id the
// file was written under, and the state it captured.
type DraftRef struct {
	ID   string
	Path string
	Snap Snapshot
}

// ListDrafts returns every draft file the workspace holds — the seats a
// previous lyond process left behind, live ones included. A corrupt or empty
// file is skipped: it is not a seat, and the next write to that path may
// still repair it. Callers re-adopt these as live seats, which is why the id
// comes from the snapshot, not the filename: a file renamed by hand still
// lists under the seat it carries.
func ListDrafts(workspace string) ([]DraftRef, error) {
	dir, err := draftsDir(workspace)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []DraftRef
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, ent.Name())
		snap, err := LoadSnapshotFrom(path)
		if err != nil || snap == nil {
			continue // unreadable draft: not a seat — the sweep reports it
		}
		id := snap.ID
		if id == "" {
			// Written by a build that predates seat ids: the filename is
			// the id Store.Create minted, so it still names the seat.
			id = strings.TrimSuffix(ent.Name(), ".json")
			snap.ID = id
		}
		out = append(out, DraftRef{ID: id, Path: path, Snap: *snap})
	}
	return out, nil
}
