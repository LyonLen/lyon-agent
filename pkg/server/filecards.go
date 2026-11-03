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
// is known non-empty. Wired from Store's onPromptEnd, which run() calls
// AFTER pushing prompt_end — so the card's seq is always above the reply
// it belongs to.
func (h *hub) scheduleFileCards(ws string) {
	h.baselineMu.Lock()
	h.fileGen++ // invalidate any capture still in flight — it describes a run that already ended
	if !h.baselineOK {
		h.baselineMu.Unlock()
		return // run shorter than a git status (or a capture failure): no card, honest skip
	}
	base := h.baseline
	h.baseline, h.baselineOK = nil, false
	h.baselineMu.Unlock()
	go func() {
		cur, ok := captureFileBaseline(context.Background(), ws)
		if !ok {
			return
		}
		cards := diffFileCards(base, cur)
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
	}()
}
