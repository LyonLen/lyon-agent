package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	lyonv1 "lyon-agent/gen/go/lyon/v1"
	"lyon-agent/gen/go/lyon/v1/lyonv1connect"
	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/tools"
)

// newGitFixture serves a real server whose workspace is a fresh scratch
// git repo — NEVER the checkout under test (Commit stages with add -A;
// pointing it at the real tree would commit the developer's work).
// Returns the client, the repo path, and a setup helper that runs git
// in the repo.
func newGitFixture(t *testing.T) (lyonv1connect.GitServiceClient, string, func(args ...string)) {
	t.Helper()
	scratchHome(t)
	repo := t.TempDir()
	t.Cleanup(func() { settleRemoveAll(repo) })

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", args[0], err, out)
		}
	}
	run("init")
	run("config", "user.email", "test@lyon.local")
	run("config", "user.name", "lyon-test")

	cfg := agent.DefaultConfig()
	cfg.Workspace = repo
	eng := agent.NewEngine(cfg, nil)
	s := New(eng, NewStore(0, 0))
	s.SetObserver(tools.NewMetrics())
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return lyonv1connect.NewGitServiceClient(http.DefaultClient, ts.URL), repo, run
}

// TestGitStatusCommitLog: the panel's full round trip — a dirty tree
// classifies correctly, Commit stages and lands, the graph shows it.
func TestGitStatusCommitLog(t *testing.T) {
	cli, repo, run := newGitFixture(t)
	ctx := context.Background()

	// Seed one commit so the branch exists.
	if err := os.WriteFile(filepath.Join(repo, "README.md"),
		[]byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-m", "seed")

	// Dirty the tree: one tracked modification, one untracked file.
	if err := os.WriteFile(filepath.Join(repo, "README.md"),
		[]byte("seed\nedited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "new.txt"),
		[]byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := cli.Status(ctx, connect.NewRequest(&lyonv1.StatusRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if !st.Msg.IsRepo || st.Msg.Branch == "" {
		t.Fatalf("status = %+v, want a repo with a branch", st.Msg)
	}
	if len(st.Msg.Changes) != 2 {
		t.Fatalf("changes = %+v, want 2 (modified + untracked)", st.Msg.Changes)
	}
	byKind := map[string]*lyonv1.GitChange{}
	for _, c := range st.Msg.Changes {
		byKind[c.Kind] = c
	}
	mod := byKind["modified"]
	if mod == nil || mod.Staged || mod.Path != "README.md" {
		t.Errorf("modified change = %+v, want README.md unstaged", mod)
	}
	un := byKind["untracked"]
	if un == nil || un.Staged || un.Path != "new.txt" {
		t.Errorf("untracked change = %+v, want new.txt", un)
	}

	// Commit through the RPC: stages everything, returns the new sha.
	cm, err := cli.Commit(ctx, connect.NewRequest(&lyonv1.CommitRequest{
		Message: "web: commit from the source-control panel",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cm.Msg.Sha == "" || !strings.Contains(cm.Msg.Summary, "source-control panel") {
		t.Errorf("commit = %+v, want a sha and the subject echoed", cm.Msg)
	}

	// Tree clean now; the graph carries the new commit.
	st2, err := cli.Status(ctx, connect.NewRequest(&lyonv1.StatusRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(st2.Msg.Changes) != 0 {
		t.Errorf("changes after commit = %+v, want empty", st2.Msg.Changes)
	}
	lg, err := cli.Log(ctx, connect.NewRequest(&lyonv1.LogRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(lg.Msg.Commits) != 2 {
		t.Fatalf("commits = %d, want 2 (seed + panel)", len(lg.Msg.Commits))
	}
	if lg.Msg.Commits[0].Subject != "web: commit from the source-control panel" {
		t.Errorf("newest commit = %+v", lg.Msg.Commits[0])
	}
	if len(lg.Msg.Commits[0].Refs) == 0 {
		t.Errorf("newest commit has no ref decoration: %+v", lg.Msg.Commits[0])
	}
}

// TestGitNotARepo: a workspace without .git degrades to is_repo=false /
// empty log — the panel hides itself, the RPC does not error.
func TestGitNotARepo(t *testing.T) {
	scratchHome(t)
	plain := t.TempDir()
	t.Cleanup(func() { settleRemoveAll(plain) })

	cfg := agent.DefaultConfig()
	cfg.Workspace = plain
	eng := agent.NewEngine(cfg, nil)
	s := New(eng, NewStore(0, 0))
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	cli := lyonv1connect.NewGitServiceClient(http.DefaultClient, ts.URL)
	ctx := context.Background()

	st, err := cli.Status(ctx, connect.NewRequest(&lyonv1.StatusRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if st.Msg.IsRepo || len(st.Msg.Changes) != 0 {
		t.Errorf("status = %+v, want is_repo=false", st.Msg)
	}
	lg, err := cli.Log(ctx, connect.NewRequest(&lyonv1.LogRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(lg.Msg.Commits) != 0 {
		t.Errorf("commits = %+v, want empty", lg.Msg.Commits)
	}
}

// TestGitCommitEmptyMessage: blank message is InvalidArgument before
// git ever runs (no empty commit slipping through).
func TestGitCommitEmptyMessage(t *testing.T) {
	cli, _, _ := newGitFixture(t)
	_, err := cli.Commit(context.Background(),
		connect.NewRequest(&lyonv1.CommitRequest{Message: "   "}))
	if err == nil {
		t.Fatal("want error for empty message, got nil")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}

// TestParseBranchHeader: the `## main...origin/main [ahead N, behind M]`
// header grammar, including the no-upstream and no-counts shapes.
func TestParseBranchHeader(t *testing.T) {
	cases := []struct {
		in               string
		branch, upstream string
		ahead, behind    int32
	}{
		{"main", "main", "", 0, 0},
		{"main...origin/main [ahead 3, behind 1]", "main", "origin/main", 3, 1},
		{"main...origin/main [ahead 2]", "main", "origin/main", 2, 0},
		{"main...origin/main", "main", "origin/main", 0, 0},
		{"HEAD (no branch)", "HEAD (no branch)", "", 0, 0},
	}
	for _, c := range cases {
		got := &lyonv1.StatusResponse{}
		parseBranchHeader(c.in, got)
		if got.Branch != c.branch || got.Upstream != c.upstream ||
			got.Ahead != c.ahead || got.Behind != c.behind {
			t.Errorf("parseBranchHeader(%q) = %+v, want branch=%q upstream=%q ahead=%d behind=%d",
				c.in, got, c.branch, c.upstream, c.ahead, c.behind)
		}
	}
}
