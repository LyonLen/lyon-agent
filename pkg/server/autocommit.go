package server

// Auto-commit + /undo — the FILE dimension of undo, paired with
// RollbackSession's conversation dimension. Every run that touched files
// records itself on a per-session side ref (refs/lyon/auto/<sid>) using
// pure git plumbing: a temp index (GIT_INDEX_FILE) so the user's index is
// never touched, commit-tree so HEAD and the branch never move, update-ref
// so only this session's private ref advances. /undo then restores the
// worktree paths that commit changed and steps the ref down its chain.
//
// The commit rides scheduleFileCards' goroutine (filecards.go): one
// baseline capture, one status pass, the card injected first, then the
// commit — the tap stays non-blocking and there is no index.lock dance
// between the card and the commit. Non-repo, unborn HEAD, empty delta and
// every transient git failure fail open silently: the card already told
// the user what changed, and an auto-commit must never surface as a
// transcript event.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect"

	lyonv1 "lyon-agent/gen/go/lyon/v1"
)

const (
	// autoRefPrefix is the side-ref namespace — one chain per session,
	// invisible to git log / ScmView / push unless explicitly asked for.
	autoRefPrefix = "refs/lyon/auto/"

	// autoCommitTimeout bounds the whole plumbing sequence (read-tree →
	// add → write-tree → commit-tree → update-ref), fileCardTimeout 量级.
	autoCommitTimeout = 5 * time.Second
)

func autoRef(sid string) string { return autoRefPrefix + sid }

// shortSID is the sid prefix the commit message carries (and the chain
// marker /undo matches when deciding step-vs-delete).
func shortSID(sid string) string {
	if len(sid) > 8 {
		return sid[:8]
	}
	return sid
}

// autoCommitMessage is BACKLOG.md's format: lyon(<sid[:8]>): <prompt 摘要>.
// The prompt collapses to one line (commit subjects are single-line) and
// clips at 60 runes with the shared clipRunes ellipsis.
func autoCommitMessage(sid, prompt string) string {
	summary := strings.Join(strings.Fields(prompt), " ")
	if summary == "" {
		summary = "(no prompt)"
	}
	return fmt.Sprintf("lyon(%s): %s", shortSID(sid), clipRunes(summary, 60))
}

// autoCommitRun is scheduleFileCards' tail: commit exactly the
// foreign-filtered delta the card just announced. Silent by contract —
// a non-repo workspace, an unborn HEAD or a transient git error is a
// nothing-to-see condition, never an event.
func (h *hub) autoCommitRun(ws, prompt string, cards []fileCard) {
	if h.sid == "" || len(cards) == 0 {
		return
	}
	paths := make([]string, len(cards))
	for i, c := range cards {
		paths[i] = c.Path
	}
	ctx, cancel := context.WithTimeout(context.Background(), autoCommitTimeout)
	defer cancel()
	// Serialized against UndoLastRun: an /undo racing the commit that is
	// mid-flight must see a consistent tip (step a ref that has already
	// advanced, never stranding an orphaned chain link).
	h.autoMu.Lock()
	defer h.autoMu.Unlock()
	_ = writeAutoCommit(ctx, ws, h.sid, prompt, paths) // fail open, always
}

// writeAutoCommit records one run on the side ref. git plumbing only:
// temp index (GIT_INDEX_FILE) → read-tree PARENT (so the chain composes —
// each link's tree contains every earlier run's recorded state) → add the
// run's paths only (user's pre-existing WIP never enters) → write-tree →
// commit-tree -p parent → update-ref. HEAD, the branch and the user's
// index are untouched by construction.
func writeAutoCommit(ctx context.Context, ws, sid, prompt string, paths []string) error {
	ref := autoRef(sid)

	// Parent: this session's previous auto-commit, else the HEAD the run
	// started from. Unborn HEAD (fresh `git init`) → nothing to hang the
	// chain on → skip (the caller fails open anyway).
	parent := ""
	if out, err := gitRun(ctx, ws, "rev-parse", "--verify", "--quiet", ref); err == nil {
		if p := strings.TrimSpace(out); p != "" {
			parent = p
		}
	}
	if parent == "" {
		out, err := gitRun(ctx, ws, "rev-parse", "HEAD")
		if err != nil {
			return err // not a repo / unborn HEAD
		}
		parent = strings.TrimSpace(out)
	}

	// Temp index: a fresh path git creates itself (a 0-byte file can trip
	// older parsers), removed on the way out along with any lock a failed
	// git left behind. The user's index is never in play.
	tmp, err := os.CreateTemp("", "lyon-idx-*")
	if err != nil {
		return err
	}
	idx := tmp.Name()
	tmp.Close()
	os.Remove(idx)
	defer os.Remove(idx)
	defer os.Remove(idx + ".lock")
	env := []string{"GIT_INDEX_FILE=" + idx}

	if _, err := gitRunEnv(ctx, ws, env, "read-tree", parent); err != nil {
		return err
	}
	// --literal-pathspecs: delta paths are literal filenames straight
	// from porcelain, not pathspec patterns (a file named notes[1].md
	// must not glob-match something else).
	add := append([]string{"--literal-pathspecs", "add", "--"}, paths...)
	if _, err := gitRunEnv(ctx, ws, env, add...); err != nil {
		return err
	}
	treeOut, err := gitRunEnv(ctx, ws, env, "write-tree")
	if err != nil {
		return err
	}
	tree := strings.TrimSpace(treeOut)

	// Content-identical delta (an mtime-only touch): nothing to record —
	// an empty commit would pollute the chain with a no-op undo point.
	parentTree, err := gitRun(ctx, ws, "rev-parse", parent+"^{tree}")
	if err != nil {
		return err
	}
	if strings.TrimSpace(parentTree) == tree {
		return nil
	}

	shaOut, err := gitRun(ctx, ws, "commit-tree", tree,
		"-p", parent, "-m", autoCommitMessage(sid, prompt))
	if err != nil {
		return err
	}
	_, err = gitRun(ctx, ws, "update-ref", ref, strings.TrimSpace(shaOut))
	return err
}

// indexBlob returns the path's blob sha in the USER's index — "" when the
// path isn't tracked there (ls-files -s prints "100644 <sha> 0<path").
func indexBlob(ctx context.Context, ws, path string) string {
	out, err := gitRun(ctx, ws, "ls-files", "-s", "--", path)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(out)
	if tab := strings.IndexByte(line, '\t'); tab > 0 {
		line = line[:tab]
	}
	f := strings.Fields(line)
	if len(f) < 2 {
		return ""
	}
	return f[1]
}

// undoLastRun restores the worktree paths the latest auto-commit changed
// and steps the side ref down its chain. Worktree only — `git restore
// --worktree` never touches the user's index, and run-created files (the
// parent lacks them) are removed instead. When the parent is no longer an
// auto-commit of THIS session (first run's parent = the user's HEAD),
// the ref is deleted rather than stepped: repeated /undo walks this
// session's chain only and can never wander into user history. Returns
// codes.NotFound when there is nothing left to undo.
func undoLastRun(ctx context.Context, ws, sid string) ([]string, string, error) {
	ref := autoRef(sid)
	out, err := gitRun(ctx, ws, "rev-parse", "--verify", "--quiet", ref)
	if err != nil || strings.TrimSpace(out) == "" {
		return nil, "", connect.NewError(connect.CodeNotFound,
			errors.New("nothing to undo — this session has no recorded run changes"))
	}
	latest := strings.TrimSpace(out)
	parent := latest + "^"

	diff, err := gitRun(ctx, ws, "diff", "--name-status", "--no-renames", parent, latest)
	if err != nil {
		return nil, "", err
	}
	// Parent's subject decides step-vs-delete — read it BEFORE touching
	// the worktree so a failure leaves everything as it was.
	subj, err := gitRun(ctx, ws, "log", "-1", "--format=%s", parent)
	if err != nil {
		return nil, "", err
	}
	parentIsOurs := strings.HasPrefix(strings.TrimSpace(subj), "lyon("+shortSID(sid)+"):")

	var restored []string
	for _, line := range strings.Split(diff, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		tab := strings.IndexByte(line, '\t')
		if tab < 1 {
			continue
		}
		status, path := line[:tab], line[tab+1:]
		if status[0] == 'A' {
			// Created by the run (absent from the parent): remove it.
			if err := os.Remove(filepath.Join(ws, filepath.FromSlash(path))); err != nil && !os.IsNotExist(err) {
				return nil, "", connect.NewError(connect.CodeInternal,
					fmt.Errorf("undo %s: %w", path, err))
			}
		} else {
			// Modified/deleted/typechanged: parent has it → restore
			// parent's content into the worktree, index untouched.
			//
			// Under core.autocrlf=true a `restore --source` leaves the
			// index's stat entry stale, and git status then reports a
			// PHANTOM M for the path (content clean, git diff empty)
			// that never self-heals — update-index --refresh refuses it
			// too. So when the user's index already carries the parent's
			// blob, restore FROM THE INDEX instead: identical bytes,
			// and git refreshes the stat on the way out. An index that
			// differs from the parent (user staged something) keeps
			// --source — content correctness first; that M is real.
			idxSha := indexBlob(ctx, ws, path)
			parOut, perr := gitRun(ctx, ws, "rev-parse", parent+":"+path)
			if perr == nil && idxSha != "" && idxSha == strings.TrimSpace(parOut) {
				if _, err := gitRun(ctx, ws, "restore", "--worktree", "--", path); err != nil {
					return nil, "", err
				}
			} else if _, err := gitRun(ctx, ws, "restore", "--source", parent, "--worktree", "--", path); err != nil {
				return nil, "", err
			}
		}
		restored = append(restored, path)
	}

	// Chain bookkeeping: step onto the parent auto-commit, or delete the
	// ref when the chain is exhausted (the parent is a real commit).
	refAfter := ""
	if parentIsOurs {
		if _, err := gitRun(ctx, ws, "update-ref", ref, parent); err != nil {
			return nil, "", err
		}
		sha, err := gitRun(ctx, ws, "rev-parse", "--short", ref)
		if err != nil {
			return nil, "", err
		}
		refAfter = strings.TrimSpace(sha)
	} else if _, err := gitRun(ctx, ws, "update-ref", "-d", ref); err != nil {
		return nil, "", err
	}
	return restored, refAfter, nil
}

// UndoLastRun — the /undo RPC. Restored paths come back so the UI can
// flash them; ref="" means the chain is exhausted (the ref was removed,
// the next call finds nothing). A post-commit user edit on the same file
// is overwritten (no 3-way merge at this scale — the flash list is the
// disclosure).
func (c *sessionService) UndoLastRun(ctx context.Context, req *connect.Request[lyonv1.UndoLastRunRequest]) (*connect.Response[lyonv1.UndoLastRunResponse], error) {
	sid := req.Msg.SessionId
	sess, err := c.s.store.Get(sid)
	if err != nil {
		return nil, rpcErr(err)
	}
	h, err := c.s.store.Hub(sid)
	if err != nil {
		return nil, rpcErr(err)
	}
	// Same autoMu as autoCommitRun: an /undo landing mid-commit waits for
	// a consistent tip instead of stranding an orphaned chain link.
	h.autoMu.Lock()
	defer h.autoMu.Unlock()
	paths, ref, err := undoLastRun(ctx, sess.Workspace(), sid)
	if err != nil {
		return nil, err // connect errors pass through untouched
	}
	return connect.NewResponse(&lyonv1.UndoLastRunResponse{
		RestoredPaths: paths,
		Ref:           ref,
	}), nil
}
