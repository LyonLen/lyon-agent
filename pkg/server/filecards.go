package server

// File cards — the chat's "产出文件" surface (workbuddy / Claude Desktop
// parity). At submit the hub snapshots the workspace's git status as a
// baseline; at prompt_end it re-snapshots, diffs against that baseline and
// injects ONE files_changed event carrying the delta (Attrs["files"] = JSON
// rows). The web reduce() turns it into a card whose rows click straight
// into the preview pane.
//
// Server-side on purpose: deliverables built through run_shell — a pptx
// assembled by a python script, say — never touch a file tool, so any
// client-side tool-based detection misses exactly the screenshot's case;
// and an injected hub event rides the replay ring like every other
// transcript event, so a reconnect redraws the card (the RunEnds badge
// durability bar). EventType is a string: type_name carries it raw
// (protoEvent's contract — "a client can still render a future type"),
// no proto change. TUI ignores unknown types (只减不增).

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"lyon-agent/pkg/agent"
)

// evFilesChanged is server-synthesized (never emitted by the driver), like
// evUserMessage.
const evFilesChanged = agent.EventType("files_changed")

// fileCard is one delta row — GitChange's JSON shape, so the web reuses
// gitBadge({kind, xy}) straight off the files panel.
type fileCard struct {
	Path   string `json:"path"`
	XY     string `json:"xy"`
	Kind   string `json:"kind"`
	Staged bool   `json:"staged"`
}

// fileSnap is one porcelain row at snapshot time: the XY letter (did the
// path enter or leave the dirty set) plus the file's mtime (did a path that
// was ALREADY dirty get touched again — same letter, new content; an
// untracked deliverable edited by the next run is exactly this case, and a
// letter-only diff would miss it).
type fileSnap struct {
	xy    string
	mtime time.Time
}

// fileCardTimeout bounds each git status pass. The diff runs off the hub's
// consumer goroutine, but an index.lock wait must not keep a goroutine
// alive forever either.
const fileCardTimeout = 2 * time.Second

// captureFileBaseline snapshots ws's porcelain status. ok=false means the
// pass failed for a reason other than "not a repo" — the caller skips the
// card rather than report a delta it cannot trust. A non-repo is a
// successful EMPTY baseline: every later diff is empty, so no card fires.
func captureFileBaseline(ctx context.Context, ws string) (map[string]fileSnap, bool) {
	ctx, cancel := context.WithTimeout(ctx, fileCardTimeout)
	defer cancel()
	out, err := gitRun(ctx, ws, "status", "--porcelain=v1", "-uall")
	if err != nil {
		if isNotARepo(err) {
			return map[string]fileSnap{}, true
		}
		return nil, false
	}
	return snapshotRows(ws, out), true
}

// snapshotRows pairs each porcelain row's XY with the file's mtime. A path
// that cannot be stat'd (raced deletion) keeps the zero time: it still
// differs from any real baseline row, so it lands in the delta honestly.
func snapshotRows(ws, out string) map[string]fileSnap {
	rows := parsePorcelainRows(out)
	snaps := make(map[string]fileSnap, len(rows))
	for _, r := range rows {
		s := fileSnap{xy: r.Xy}
		if fi, err := os.Stat(filepath.Join(ws, filepath.FromSlash(r.Path))); err == nil {
			s.mtime = fi.ModTime()
		}
		snaps[r.Path] = s
	}
	return snaps
}

// diffFileCards is the delta: a path is card-worthy when it is new since
// the baseline, its XY letter changed (clean→dirty, add, delete), or its
// mtime moved under an unchanged letter (already-dirty file touched again).
// Paths only in the baseline are NOT swept in: porcelain lists deletions
// itself, and a path that left the dirty set was committed — a card of
// "everything the run cleaned up" would be noise. Output is path-sorted so
// the card renders deterministically.
func diffFileCards(base, cur map[string]fileSnap) []fileCard {
	var cards []fileCard
	for path, now := range cur {
		was, ok := base[path]
		if ok && was.xy == now.xy && was.mtime.Equal(now.mtime) {
			continue
		}
		kind, staged := classifyChange(now.xy)
		cards = append(cards, fileCard{Path: path, XY: now.xy, Kind: kind, Staged: staged})
	}
	sort.Slice(cards, func(i, j int) bool { return cards[i].Path < cards[j].Path })
	return cards
}

// writeLog is the cross-seat write ledger behind the file-card attribution
// filter (#41-2): the delta itself is workspace-wide, so a parallel
// session's edits would land in THIS run's 产出文件 card (Image #41). A
// path-taking tool (write_file / edit_file) carries its path in the input,
// and the hub is the single consumer that sees every such event — so each
// seat's hub records into this shared ledger, and the card filter drops
// rows another seat wrote during the run window.
//
// Shell writes leave no record: unattributable by construction, and they
// stay in the delta on purpose — the card exists chiefly for shell-built
// deliverables (the filecards.go header's whole reason for being).
//
// One entry per path (last writer wins), pruned past writeLogTTL so a
// long-lived server doesn't hold every path ever edited.
type writeLog struct {
	mu sync.Mutex
	m  map[string]writeRec
}

// writeRec is one seat's most recent path-taking write of one path.
type writeRec struct {
	sid string
	at  time.Time
}

// writeLogTTL bounds a record's usefulness: no run window approaches a
// day, so anything older can never change a filter decision again.
const writeLogTTL = 24 * time.Hour

// record notes that sid wrote path (already in the porcelain's shape —
// callers normalize through normCardPath) at now. Last writer wins.
func (w *writeLog) record(sid, path string, now time.Time) {
	if sid == "" || path == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.m == nil {
		w.m = make(map[string]writeRec, 64)
	}
	for k, r := range w.m {
		if now.Sub(r.at) > writeLogTTL {
			delete(w.m, k)
		}
	}
	w.m[path] = writeRec{sid: sid, at: now}
}

// foreignWrite reports: a DIFFERENT seat wrote path at or after since —
// the row belongs to that seat's run, not this one's. Own writes and
// pre-window writes return false (they stay in this run's card: a path
// another seat edited yesterday but THIS run's shell touched is this
// run's output).
func (w *writeLog) foreignWrite(sid, path string, since time.Time) bool {
	if w == nil || sid == "" {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	r, ok := w.m[path]
	return ok && r.sid != sid && !r.at.Before(since)
}

// recorded reports whether the ledger holds a write of path by sid — the
// wait seam for tests (foreignWrite is the filter's question and answers
// false for the writer's own sid by design).
func (w *writeLog) recorded(sid, path string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	r, ok := w.m[path]
	return ok && r.sid == sid
}

// filterCardsByWriter drops rows another seat wrote during the run window
// [since, …]. Everything else survives: this seat's own tool writes, paths
// no tool attributed (shell deliverables), and other-seat writes from
// before the window (the delta already proves this run touched them).
func filterCardsByWriter(cards []fileCard, sid string, w *writeLog, since time.Time) []fileCard {
	if sid == "" || w == nil || len(cards) == 0 {
		return cards
	}
	out := cards[:0] // filter in place — out index never passes the range index
	for _, c := range cards {
		if w.foreignWrite(sid, c.Path, since) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// normCardPath puts a tool input path into the porcelain's shape —
// workspace-relative, slash-separated — so a ledger record matches a card
// row exactly. A path outside the workspace returns "" (it can never be a
// card row; recording it could only cause false matches).
func normCardPath(ws, p string) string {
	if ws == "" || p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		rel, err := filepath.Rel(ws, p)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return ""
		}
		p = rel
	}
	return filepath.ToSlash(filepath.Clean(filepath.FromSlash(p)))
}

// beginFileCards snapshots the workspace for the run that is about to
// start — async (a cold index can take a beat; Submit must not wait on
// git). A baseline already in place is kept: the phantom double-submit
// that slipped past Busy() would otherwise overwrite the live run's
// snapshot mid-flight (same residual class as its echo).
func (h *hub) beginFileCards(ws string) {
	h.baselineMu.Lock()
	if h.baselineOK {
		h.baselineMu.Unlock()
		return
	}
	h.fileGen++
	// Attribution window opens HERE (submit), not when the async capture
	// lands: a foreign seat's write between submit and capture-completion
	// may or may not be inside the snapshot, but it is their run's write
	// either way — the filter must exclude it.
	h.baselineAt = time.Now()
	gen := h.fileGen
	h.baselineMu.Unlock()
	go func() {
		base, ok := captureFileBaseline(context.Background(), ws)
		h.baselineMu.Lock()
		// Store only if this is still the current attempt: a schedule in
		// the meantime invalidated it (the snapshot would describe a run
		// that already ended), and ok=false leaves baselineOK false so the
		// next submit retries the capture.
		if ok && h.fileGen == gen && !h.baselineOK {
			h.baseline, h.baselineOK = base, true
		}
		h.baselineMu.Unlock()
	}()
}

// scheduleFileCards consumes the baseline at prompt_end: the diff itself
// runs in its own goroutine (git status never delays the hub's consumer —
// same contract as retitle's tap) and injects files_changed once the delta
// is known non-empty. The same goroutine then records the delta on the
// session's side ref (autoCommitRun, autocommit.go) — one capture, one
// status pass, card first, commit second, no index.lock contention.
// Wired from Store's onPromptEnd, which run() calls AFTER pushing
// prompt_end — so the card's seq is always above the reply it belongs to.
// prompt (Session.LastPrompt) is the commit message's summary source.
func (h *hub) scheduleFileCards(ws, prompt string) {
	h.baselineMu.Lock()
	h.fileGen++ // invalidate any capture still in flight — it describes a run that already ended
	if !h.baselineOK {
		h.baselineMu.Unlock()
		return // run shorter than a git status (or a capture failure): no card, honest skip
	}
	base := h.baseline
	since := h.baselineAt // the run window's start — see beginFileCards
	h.baseline, h.baselineOK = nil, false
	h.baselineMu.Unlock()
	go func() {
		cur, ok := captureFileBaseline(context.Background(), ws)
		if !ok {
			return
		}
		// #41-2: drop rows a parallel seat wrote during this run (the
		// workspace-wide delta can't tell writers apart; the ledger can).
		cards := filterCardsByWriter(diffFileCards(base, cur), h.sid, h.writes, since)
		if len(cards) == 0 {
			return
		}
		data, err := json.Marshal(cards)
		if err != nil {
			return
		}
		h.inject(agent.Event{
			Type:  evFilesChanged,
			Attrs: map[string]string{"files": string(data)},
		})
		// The card lands first; the auto-commit of exactly this delta
		// follows in the same goroutine (fail-open, silent).
		h.autoCommitRun(ws, prompt, cards)
	}()
}
