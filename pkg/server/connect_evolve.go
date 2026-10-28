package server

import (
	"context"
	"os"
	"sort"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	lyonv1 "lyon-agent/gen/go/lyon/v1"
	"lyon-agent/gen/go/lyon/v1/lyonv1connect"
	"lyon-agent/pkg/eval"
	"lyon-agent/pkg/flow"
	"lyon-agent/pkg/market"
	"lyon-agent/pkg/space"
)

// evolveService is the Connect implementation of lyon.v1.EvolveService:
// one read-only state RPC over $LYON_HOME's evolution files — the
// candidate lineage, each agent's specialist space, the frozen procedure
// set and the exam-bank footprint. Same per-call disk-read shape as
// dashboardService (no cache: the panel must see what the TUI sees).
// Routing and leaderboard rows ride DashboardService.GetOverview — the
// panel composes the two, one source per dataset.
type evolveService struct {
	s *Server
}

var _ lyonv1connect.EvolveServiceHandler = (*evolveService)(nil)

// GetEvolveState assembles the whole panel in one round trip. Reads are
// independent disk loads; they run in parallel (Go fan-out — the panel
// fires both this and GetOverview on mount).
func (e *evolveService) GetEvolveState(
	ctx context.Context, _ *connect.Request[lyonv1.GetEvolveStateRequest],
) (*connect.Response[lyonv1.GetEvolveStateResponse], error) {
	type part struct {
		cands []*lyonv1.Candidate
		spcs  []*lyonv1.AgentSpace
		flws  []*lyonv1.FlowRef
		banks []*lyonv1.ExamBank
		mstat []*lyonv1.MarketStat
		mrec  []*lyonv1.MarketRecord
	}
	var p part
	done := make(chan struct{}, 6)
	load := func(f func()) {
		go func() { f(); done <- struct{}{} }()
	}
	load(func() { p.cands = e.candidates() })
	load(func() { p.spcs = e.spaces() })
	load(func() { p.flws = e.flows() })
	load(func() { p.banks = e.banks() })
	load(func() { p.mstat = e.marketStats() })
	load(func() { p.mrec = e.marketRecent() })
	for i := 0; i < 6; i++ {
		<-done
	}
	return connect.NewResponse(&lyonv1.GetEvolveStateResponse{
		SelfModel:    e.s.eng.Config().Model,
		Candidates:   p.cands,
		Spaces:       p.spcs,
		Flows:        p.flws,
		Banks:        p.banks,
		MarketStats:  p.mstat,
		MarketRecent: p.mrec,
	}), nil
}

// candidates is the lineage: every saved profile plus the live incumbent,
// each with the classes it currently holds on the leaderboard.
func (e *evolveService) candidates() []*lyonv1.Candidate {
	lb := eval.OpenLeaderboard()
	selfID := eval.SelfCandidate("").ID
	out := make([]*lyonv1.Candidate, 0, 8)

	// The incumbent first — the lineage root the panel anchors on.
	self := eval.SelfCandidate(e.s.eng.Config().Model)
	out = append(out, candidateMsg(&self, lb, true))

	for _, c := range eval.LoadCandidates() {
		c := c
		if c.ID == selfID {
			continue // a file claiming "self" loses to the live config
		}
		out = append(out, candidateMsg(&c, lb, false))
	}
	return out
}

func candidateMsg(c *eval.Candidate, lb *eval.Leaderboard, self bool) *lyonv1.Candidate {
	m := &lyonv1.Candidate{
		Id:      c.ID,
		Model:   c.Model,
		Effort:  c.Effort,
		Note:    c.Note,
		Gen:     int32(c.Gen),
		Parent:  c.Parent,
		Created: timestamppb.New(c.Created),
		Self:    self,
		Seats:   lb.Seats(c.ID),
	}
	return m
}

// spaces is the commons tier first, then every agent id owning a space
// directory — knowledge and skills side by side, endorsements counted.
func (e *evolveService) spaces() []*lyonv1.AgentSpace {
	out := []*lyonv1.AgentSpace{spaceMsg("commons",
		space.ListCommons(space.KindKnowledge), space.ListCommons(space.KindSkills))}

	entries, err := os.ReadDir(space.AgentsDir())
	if err != nil {
		return out // no spaces yet — commons alone is the honest answer
	}
	var ids []string
	for _, en := range entries {
		if en.IsDir() {
			ids = append(ids, en.Name())
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		out = append(out, spaceMsg(id,
			space.List(id, space.KindKnowledge), space.List(id, space.KindSkills)))
	}
	return out
}

func spaceMsg(id string, knowledge, skills []space.Item) *lyonv1.AgentSpace {
	m := &lyonv1.AgentSpace{AgentId: id}
	for _, it := range knowledge {
		m.Knowledge = append(m.Knowledge, itemMsg(it))
	}
	for _, it := range skills {
		m.Skills = append(m.Skills, itemMsg(it))
	}
	return m
}

func itemMsg(it space.Item) *lyonv1.SpaceItem {
	return &lyonv1.SpaceItem{
		Name:         it.Name,
		Description:  it.Description,
		Endorsements: int32(len(it.Endorsements)),
	}
}

// flows is /flow list — builtins and user overrides merged by slug, the
// source column marking which side won.
func (e *evolveService) flows() []*lyonv1.FlowRef {
	wfs, err := flow.Load()
	if err != nil {
		return nil
	}
	builtins, _ := flow.Builtins()
	bset := make(map[string]bool, len(builtins))
	for _, b := range builtins {
		bset[b.Slug] = true
	}
	out := make([]*lyonv1.FlowRef, 0, len(wfs))
	for _, w := range wfs {
		src := "user"
		if bset[w.Slug] {
			src = "builtin"
		}
		out = append(out, &lyonv1.FlowRef{
			Slug:    w.Slug,
			Title:   w.Title,
			Trigger: w.Trigger,
			Nodes:   int32(len(w.Nodes)),
			Source:  src,
		})
	}
	return out
}

// banks is the exam-set footprint: one LoadExam per ledger class (the
// same Views snapshot the TUI's /exam describes — an error means the
// bank has not landed on disk yet).
func (e *evolveService) banks() []*lyonv1.ExamBank {
	views := eval.OpenLeaderboard().Views(dashClassOrder)
	out := make([]*lyonv1.ExamBank, 0, len(views))
	for _, v := range views {
		b := &lyonv1.ExamBank{Class: v.Class}
		if ex, err := eval.LoadExam(v.Class); err == nil {
			b.Seeded = true
			b.Questions = int32(len(ex.Questions))
		}
		out = append(out, b)
	}
	return out
}

// marketStats is the task-hour ledger's per-class table (the TUI's
// /market header: class, samples, mean, latest).
func (e *evolveService) marketStats() []*lyonv1.MarketStat {
	stats := market.Open().Stats()
	out := make([]*lyonv1.MarketStat, 0, len(stats))
	for _, s := range stats {
		out = append(out, &lyonv1.MarketStat{
			Class: s.Class, Samples: int32(s.Samples),
			AvgMs: s.AvgMS, LastMs: s.LastMS,
		})
	}
	return out
}

// marketRecent is the ledger's history tail, newest first (the TUI's
// recent-tasks list; 20 rows is a web page, not the 500-row file).
func (e *evolveService) marketRecent() []*lyonv1.MarketRecord {
	recent := market.Open().Recent(20)
	out := make([]*lyonv1.MarketRecord, 0, len(recent))
	for i := len(recent) - 1; i >= 0; i-- { // newest first
		r := recent[i]
		out = append(out, &lyonv1.MarketRecord{
			Class: r.Class, Model: r.Model, Prompt: r.Prompt,
			DurMs: r.DurMS, Iterations: int32(r.Iters), Err: r.Err,
			Ts: timestamppb.New(r.TS),
		})
	}
	return out
}
