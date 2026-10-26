package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	lyonv1 "lyon-agent/gen/go/lyon/v1"
	"lyon-agent/gen/go/lyon/v1/lyonv1connect"
	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/eval"
	"lyon-agent/pkg/space"
	"lyon-agent/pkg/tools"
)

// newEvolveFixture serves a real server on a scratch LYON_HOME — same
// shape as the dashboard fixture (no prompts, disk-only reads).
func newEvolveFixture(t *testing.T) (lyonv1connect.EvolveServiceClient, *agent.Engine) {
	t.Helper()
	scratchHome(t)
	eng := agent.NewEngine(agent.DefaultConfig(), nil)
	s := New(eng, NewStore(0, 0))
	s.SetObserver(tools.NewMetrics())
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return lyonv1connect.NewEvolveServiceClient(http.DefaultClient, ts.URL), eng
}

// TestGetEvolveState: the panel's one round trip — incumbent first, a
// seeded child with its lineage, its space lesson, the commons tier,
// builtin flows and the ledger's exam banks.
func TestGetEvolveState(t *testing.T) {
	cli, eng := newEvolveFixture(t)

	// Seed what the panel must see: a child candidate and its space.
	if err := eval.SaveCandidate(eval.Candidate{
		ID: "alpha", Model: "m-alpha", Gen: 1, Parent: "self",
		Note: "evolved from self: missed the test question",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := space.SaveKnowledge("alpha",
		"lesson from self", "why this generation exists", "run go build first"); err != nil {
		t.Fatal(err)
	}

	res, err := cli.GetEvolveState(context.Background(),
		connect.NewRequest(&lyonv1.GetEvolveStateRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	st := res.Msg

	if st.SelfModel != eng.Config().Model {
		t.Errorf("self_model = %q, want %q", st.SelfModel, eng.Config().Model)
	}

	// Incumbent first, then the saved child.
	if len(st.Candidates) != 2 {
		t.Fatalf("candidates = %d (%+v), want 2", len(st.Candidates), st.Candidates)
	}
	if self := st.Candidates[0]; self.Id != "self" || !self.Self || self.Model != eng.Config().Model {
		t.Errorf("first candidate = %+v, want the live incumbent", self)
	}
	child := st.Candidates[1]
	if child.Id != "alpha" || child.Gen != 1 || child.Parent != "self" ||
		len(child.Seats) != 0 {
		t.Errorf("child = %+v, want alpha g1 of self with no seats", child)
	}

	// Commons rides first, alpha follows with its lesson.
	if len(st.Spaces) < 2 || st.Spaces[0].AgentId != "commons" {
		t.Fatalf("spaces head = %+v, want commons first", st.Spaces)
	}
	var alpha *lyonv1.AgentSpace
	for _, sp := range st.Spaces {
		if sp.AgentId == "alpha" {
			alpha = sp
		}
	}
	if alpha == nil || len(alpha.Knowledge) != 1 ||
		alpha.Knowledge[0].Name != "lesson from self" {
		t.Fatalf("alpha space = %+v, want the seeded lesson", alpha)
	}

	// Builtins always load.
	if len(st.Flows) == 0 {
		t.Fatal("flows empty — builtins must list")
	}
	for _, f := range st.Flows {
		if f.Source != "builtin" && f.Source != "user" {
			t.Errorf("flow %s source = %q", f.Slug, f.Source)
		}
		if f.Nodes == 0 {
			t.Errorf("flow %s has no nodes", f.Slug)
		}
	}

	// Banks mirror the ledger's class order; scratch home has no files
	// on disk yet, so nothing is seeded.
	if len(st.Banks) == 0 {
		t.Fatal("banks empty — the class order always describes itself")
	}
	for _, b := range st.Banks {
		if b.Seeded {
			t.Errorf("bank %s seeded on a scratch home", b.Class)
		}
	}
}
