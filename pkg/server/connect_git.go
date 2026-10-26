package server

// GitService — the files panel's source-control view. Every call runs
// `git -C <workspace>` with a fixed argv: the RPCs take no path, remote,
// or flag arguments (Commit's message rides a single argv slot, never a
// shell), so a hostile payload cannot aim git anywhere but the workspace
// repository. Reads fan out where two round trips are needed (Status
// parses the branch header separately from the change rows only when a
// rename forces it — otherwise one `status --porcelain -b` covers both).

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"connectrpc.com/connect"

	lyonv1 "lyon-agent/gen/go/lyon/v1"
	"lyon-agent/gen/go/lyon/v1/lyonv1connect"
)

type gitService struct {
	s *Server
}

var _ lyonv1connect.GitServiceHandler = (*gitService)(nil)

// gitRun executes one git command in the workspace root. Argv only —
// no shell — so user text (commit messages) can never become syntax.
// On failure the stderr tail becomes a FailedPrecondition message; the
// caller pattern-matches "not a git repository" to degrade to is_repo=false.
func gitRun(ctx context.Context, ws string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", ws}, args...)...)
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errBuf.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.String(), connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("git %s: %s", args[0], msg))
	}
	return out.String(), nil
}

// isNotARepo reports whether the failure is the benign "workspace is
// not a git repository" case the panel renders as an empty view.
func isNotARepo(err error) bool {
	return err != nil &&
		strings.Contains(err.Error(), "not a git repository")
}

// Status: one `git status --porcelain=v1 -b -uall` pass → branch header
// + change rows. Not-a-repo is a 200 with is_repo=false, not an error —
// the panel just hides itself.
func (g *gitService) Status(ctx context.Context,
	_ *connect.Request[lyonv1.StatusRequest]) (*connect.Response[lyonv1.StatusResponse], error) {
	out, err := gitRun(ctx, g.s.eng.Config().Workspace,
		"status", "--porcelain=v1", "-b", "-uall")
	if isNotARepo(err) {
		return connect.NewResponse(&lyonv1.StatusResponse{}), nil
	}
	if err != nil {
		return nil, err
	}
	resp := &lyonv1.StatusResponse{IsRepo: true}
	for i, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if i == 0 && strings.HasPrefix(line, "## ") {
			parseBranchHeader(line[3:], resp)
			continue
		}
		if len(line) < 3 {
			continue
		}
		xy := line[:2]
		path := line[3:]
		// Renames print "old -> new"; the panel opens the new name.
		if idx := strings.Index(path, " -> "); idx >= 0 && xy[0] == 'R' {
			path = path[idx+4:]
		}
		kind, staged := classifyChange(xy)
		resp.Changes = append(resp.Changes, &lyonv1.GitChange{
			Path: path, Xy: xy, Staged: staged, Kind: kind,
		})
	}
	return connect.NewResponse(resp), nil
}

// parseBranchHeader fills branch/upstream/ahead/behind from the
// `## main...origin/main [ahead 3, behind 1]` first line.
var aheadBehindRe = regexp.MustCompile(`\[(ahead (\d+))(?:, behind (\d+))?\]`)

func parseBranchHeader(h string, resp *lyonv1.StatusResponse) {
	branch, upstreamPart := h, ""
	if idx := strings.Index(h, "..."); idx >= 0 {
		branch = h[:idx]
		upstreamPart = h[idx+3:]
	}
	// Trailing " [ahead N, behind M]" belongs to the upstream, not its name.
	if idx := strings.Index(upstreamPart, " ["); idx >= 0 {
		if m := aheadBehindRe.FindStringSubmatch(upstreamPart); m != nil {
			resp.Ahead = int32(atoiOrZero(m[2]))
			if m[3] != "" {
				resp.Behind = int32(atoiOrZero(m[3]))
			}
		}
		upstreamPart = upstreamPart[:idx]
	}
	resp.Branch = branch
	resp.Upstream = upstreamPart
}

// classifyChange maps porcelain XY → the panel's kind word + staged flag.
func classifyChange(xy string) (kind string, staged bool) {
	x, y := byte(' '), byte(' ')
	if len(xy) >= 2 {
		x, y = xy[0], xy[1]
	}
	staged = x != ' ' && x != '?'
	switch {
	case xy == "??":
		return "untracked", false
	case x == 'U' || y == 'U' || (x == 'A' && y == 'A') || (x == 'D' && y == 'D'):
		return "conflicted", true
	case x == 'R' || y == 'R':
		return "renamed", staged
	case x == 'D' || y == 'D':
		return "deleted", staged
	case x == 'A' || x == 'C':
		return "added", staged
	default:
		return "modified", staged
	}
}

// Log: recent commits with decorations, plus the outgoing set (HEAD but
// not upstream) when an upstream exists. Two git round trips — log and
// rev-list — run as siblings, not in sequence.
func atoiOrZero(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func (g *gitService) Log(ctx context.Context,
	req *connect.Request[lyonv1.LogRequest]) (*connect.Response[lyonv1.LogResponse], error) {
	ws := g.s.eng.Config().Workspace
	n := int(req.Msg.Limit)
	if n <= 0 {
		n = 30
	}
	if n > 100 {
		n = 100
	}

	type logOut struct {
		lines []string
		err   error
	}
	type revOut struct {
		shas map[string]bool
		err  error
	}
	logC := make(chan logOut, 1)
	revC := make(chan revOut, 1)
	go func() {
		out, err := gitRun(ctx, ws, "log",
			fmt.Sprintf("--max-count=%d", n),
			"--pretty=format:%H%x1f%s%x1f%an%x1f%at%x1f%D")
		var lines []string
		if err == nil && out != "" {
			lines = strings.Split(out, "\n")
		}
		logC <- logOut{lines, err}
	}()
	go func() {
		// @{u} fails with no upstream — that's an empty outgoing set,
		// not an error (the badge simply never lights).
		out, err := gitRun(ctx, ws, "rev-list", "@{u}..HEAD")
		shas := map[string]bool{}
		if err == nil {
			for _, s := range strings.Fields(out) {
				shas[s] = true
			}
		}
		revC <- revOut{shas, nil}
	}()

	l := <-logC
	if isNotARepo(l.err) {
		return connect.NewResponse(&lyonv1.LogResponse{}), nil
	}
	if l.err != nil {
		return nil, l.err
	}
	r := <-revC

	resp := &lyonv1.LogResponse{}
	for _, line := range l.lines {
		parts := strings.Split(line, "\x1f")
		if len(parts) < 5 {
			continue
		}
		c := &lyonv1.GitCommit{
			Sha:      parts[0],
			Subject:  parts[1],
			Author:   parts[2],
			Outgoing: r.shas[parts[0]],
		}
		if at, err := strconv.ParseInt(parts[3], 10, 64); err == nil {
			c.AtUnix = at
		}
		if d := strings.TrimSpace(parts[4]); d != "" {
			for _, ref := range strings.Split(d, ", ") {
				if ref != "" {
					c.Refs = append(c.Refs, ref)
				}
			}
		}
		resp.Commits = append(resp.Commits, c)
	}
	return connect.NewResponse(resp), nil
}

// Commit stages the whole working tree (`git add -A` — gitignore still
// applies) and commits it. Empty message → InvalidArgument; nothing to
// commit → FailedPrecondition from git itself, surfaced verbatim.
func (g *gitService) Commit(ctx context.Context,
	req *connect.Request[lyonv1.CommitRequest]) (*connect.Response[lyonv1.CommitResponse], error) {
	msg := strings.TrimSpace(req.Msg.Message)
	if msg == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("commit message is empty"))
	}
	ws := g.s.eng.Config().Workspace
	if _, err := gitRun(ctx, ws, "add", "-A"); err != nil {
		return nil, err
	}
	summary, err := gitRun(ctx, ws, "commit", "-m", msg)
	if err != nil {
		return nil, err
	}
	sha, err := gitRun(ctx, ws, "rev-parse", "--short", "HEAD")
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&lyonv1.CommitResponse{
		Sha:     strings.TrimSpace(sha),
		Summary: strings.TrimSpace(summary),
	}), nil
}

// Sync: pull --rebase --autostash, then push. The panel only offers the
// button when Status reported an upstream; without one git's own
// "no upstream" stderr comes back as FailedPrecondition.
func (g *gitService) Sync(ctx context.Context,
	_ *connect.Request[lyonv1.SyncRequest]) (*connect.Response[lyonv1.SyncResponse], error) {
	ws := g.s.eng.Config().Workspace
	pull, err := gitRun(ctx, ws, "pull", "--rebase", "--autostash")
	if err != nil {
		return nil, err
	}
	push, err := gitRun(ctx, ws, "push")
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&lyonv1.SyncResponse{Output: pull + push}), nil
}
