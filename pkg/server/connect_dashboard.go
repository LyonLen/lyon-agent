package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	lyonv1 "lyon-agent/gen/go/lyon/v1"
	"lyon-agent/gen/go/lyon/v1/lyonv1connect"
	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/eval"
	"lyon-agent/pkg/routing"
)

// dashboardService is the Connect implementation of lyon.v1.DashboardService:
// read-only panels over $LYON_HOME on-disk data (run dumps, audit JSONL,
// saved sessions, routing table, leaderboard) plus the process aggregates
// /metrics already computes. Mounted in New() like its siblings, so the
// bearer wrapper covers every RPC here too.
type dashboardService struct {
	s *Server
}

var _ lyonv1connect.DashboardServiceHandler = (*dashboardService)(nil)

// dashClassOrder mirrors cmd/lyon's evoClassOrder — the evolution panel's
// row order (routing's classes, most common first). package main cannot be
// imported from here, so the list is copied; Views appends any extra class
// the ledger grows, sorted, so a rename lands at the bottom instead of
// vanishing.
var dashClassOrder = []string{
	string(routing.ClassCoding),
	string(routing.ClassFix),
	string(routing.ClassReview),
	string(routing.ClassResearch),
	string(routing.ClassWriting),
	string(routing.ClassDesign),
	string(routing.ClassData),
	string(routing.ClassGeneral),
}

// dashHome resolves $LYON_HOME for the on-disk panels with the same rule
// as pkg/agent's lyonHome: env override, else ~/.lyon.
func dashHome() (string, error) {
	if h := os.Getenv("LYON_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".lyon"), nil
}

// dashDay resolves a request's day selector: "" = today (local time —
// writers Format local time into their directory/file names), anything
// else must be a calendar date. Day strings become path segments, so the
// shape is validated here at the RPC boundary before anything reaches
// the filesystem (InvalidArgument, never a traversal attempt).
func dashDay(day string) (string, error) {
	if day == "" {
		return time.Now().Format("2006-01-02"), nil
	}
	bad := connect.NewError(connect.CodeInvalidArgument,
		fmt.Errorf("day must be YYYY-MM-DD, got %q", day))
	if len(day) != 10 || day[4] != '-' || day[7] != '-' {
		return "", bad
	}
	for i := 0; i < len(day); i++ {
		if i == 4 || i == 7 {
			continue
		}
		if day[i] < '0' || day[i] > '9' {
			return "", bad
		}
	}
	if _, err := time.Parse("2006-01-02", day); err != nil {
		return "", bad
	}
	return day, nil
}

// dashErr maps panel errors onto Connect codes: malformed selectors are
// InvalidArgument, everything else rides the shared file mapper (missing
// → NotFound, denied → PermissionDenied, else Internal with a path hint).
func dashErr(err error, what string) error {
	if errors.Is(err, agent.ErrBadRunID) || errors.Is(err, agent.ErrBadDay) {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	return fileOSErr(err, what)
}

// GetOverview answers "how is this process doing" in one shot: live
// session rollup (shared with /metrics — one aggregation, two
// transports), the routing network, and the leaderboard. Disk reads are
// per-call on purpose: routing/leaderboard are small files and re-reading
// means the panel never serves a stale cache.
func (s *dashboardService) GetOverview(_ context.Context, _ *connect.Request[lyonv1.GetOverviewRequest]) (*connect.Response[lyonv1.GetOverviewResponse], error) {
	agg := s.s.usageRollup()
	out := &lyonv1.GetOverviewResponse{
		Sessions:      int32(agg.Sessions),
		UptimeSeconds: int64(time.Since(s.s.start).Seconds()),
		Model:         s.s.eng.Config().Model,
		CacheShare:    agg.CacheShare,
	}
	for _, e := range routing.Open().All() {
		out.Routing = append(out.Routing, &lyonv1.RoutingEntry{
			Class:      e.Class,
			Agent:      e.Agent,
			Model:      e.Model,
			Confidence: e.Confidence,
			Samples:    int32(e.Samples),
			MeanScore:  e.MeanScore,
			Updated:    timestamppb.New(e.Updated),
		})
	}
	for _, v := range eval.OpenLeaderboard().Views(dashClassOrder) {
		out.Leaderboard = append(out.Leaderboard, &lyonv1.LeaderboardView{
			Class:      v.Class,
			OnDuty:     v.OnDuty,
			Rounds:     int32(v.Rounds),
			ExamFails:  int32(v.ExamFails),
			Cards:      int32(v.Cards),
			Mean:       v.Mean,
			LastAgent:  v.LastAgent,
			LastTotal:  v.LastTotal,
			LastPassed: v.LastPassed,
			LastSource: v.LastSource,
		})
	}
	return connect.NewResponse(out), nil
}

// ListRuns lists one day's run dumps, newest first. "" = today; a day
// nobody ran in is an empty list (missing dumps dir is normal), not an
// error.
func (s *dashboardService) ListRuns(_ context.Context, req *connect.Request[lyonv1.ListRunsRequest]) (*connect.Response[lyonv1.ListRunsResponse], error) {
	day, err := dashDay(req.Msg.Day)
	if err != nil {
		return nil, err
	}
	limit := int(req.Msg.Limit)
	if limit < 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("limit must be >= 0"))
	}
	if limit == 0 {
		limit = 50
	}
	refs, err := agent.ListRunDumps(day, limit)
	if err != nil {
		return nil, dashErr(err, "dumps/"+day)
	}
	out := &lyonv1.ListRunsResponse{Runs: make([]*lyonv1.RunRef, 0, len(refs))}
	for _, r := range refs {
		out.Runs = append(out.Runs, &lyonv1.RunRef{
			RunId:      r.RunID,
			Workspace:  r.Workspace,
			Model:      r.Model,
			Prompt:     r.Prompt,
			StartedAt:  timestamppb.New(r.StartedAt),
			DurationMs: r.DurationMS,
			Failed:     r.Failed,
		})
	}
	return connect.NewResponse(out), nil
}

// GetRun loads one full dump document for the replay timeline. The run id
// is the whole locator — agent.LoadRunDump validates its shape and only
// then builds the path, so a crafted id can never leave dumps/.
func (s *dashboardService) GetRun(_ context.Context, req *connect.Request[lyonv1.GetRunRequest]) (*connect.Response[lyonv1.GetRunResponse], error) {
	doc, err := agent.LoadRunDump(req.Msg.RunId)
	if err != nil {
		return nil, dashErr(err, "dumps")
	}
	out := &lyonv1.GetRunResponse{
		RunId:       doc.RunID,
		Workspace:   doc.Workspace,
		Model:       doc.Model,
		Prompt:      doc.Prompt,
		FirstPrompt: doc.FirstPrompt,
		StartedAt:   timestamppb.New(doc.StartedAt),
		EndedAt:     timestamppb.New(doc.EndedAt),
		DurationMs:  doc.DurationMS,
		Iterations:  int32(doc.Iterations),
		StopReason:  doc.StopReason,
		PromptErr:   doc.PromptErr,
		Usage:       protoUsage(doc.Usage),
		Events:      make([]*lyonv1.DumpEvent, 0, len(doc.Events)),
		History:     doc.History,
	}
	if len(doc.SubUsage) > 0 {
		out.SubUsage = make(map[string]*lyonv1.Usage, len(doc.SubUsage))
		for k, u := range doc.SubUsage {
			out.SubUsage[k] = protoUsage(u)
		}
	}
	for _, e := range doc.Events {
		out.Events = append(out.Events, dumpEventProto(e))
	}
	return connect.NewResponse(out), nil
}

// dumpEventProto maps one dump timeline entry onto the wire. This is the
// flat replay shape of dashboard.proto (seq included, type carried raw,
// input as a JSON string) — deliberately separate from the live
// protoEvent, whose shape is a stamped oneof.
func dumpEventProto(e agent.RunDumpEvent) *lyonv1.DumpEvent {
	ev := e.Event
	out := &lyonv1.DumpEvent{
		Seq:        int32(e.Seq),
		Type:       string(ev.Type),
		Text:       ev.Text,
		Thinking:   ev.Thinking,
		ToolId:     ev.ToolID,
		ExecId:     int64(ev.ExecID),
		ToolName:   ev.ToolName,
		InputJson:  string(ev.Input),
		Server:     ev.Server,
		Sub:        ev.Sub,
		SubId:      ev.SubID,
		DurationMs: ev.DurationMS,
		IsError:    ev.IsError,
		Error:      ev.Err,
		Turn:       int32(ev.Turn),
		StopReason: ev.StopReason,
		Iterations: int32(ev.Iterations),
		Detail:     ev.Detail,
	}
	if ev.Usage != nil {
		out.Usage = protoUsage(*ev.Usage)
	}
	if !ev.TS.IsZero() {
		out.Ts = timestamppb.New(ev.TS)
	}
	return out
}

// auditLine is the hooks.Record JSONL subset the proto exposes. Time is
// decoded as a string: the writer marshals time.Time to RFC3339Nano and
// the panel shows it verbatim, no parse round-trip needed.
type auditLine struct {
	Time       string `json:"time"`
	Event      string `json:"event"`
	Session    string `json:"session"`
	Tool       string `json:"tool"`
	Prompt     string `json:"prompt"`
	Detail     string `json:"detail"`
	IsError    bool   `json:"is_error"`
	Err        string `json:"error"`
	DurationMS int64  `json:"duration_ms"`
}

// ListAudit returns the day's audit tail, newest last (file order is
// append order = chronological). A day with no audit file yet is an empty
// list — hooks only creates the file on first write. Unparseable lines
// are skipped: one torn write must not hide the rest of the day.
func (s *dashboardService) ListAudit(_ context.Context, req *connect.Request[lyonv1.ListAuditRequest]) (*connect.Response[lyonv1.ListAuditResponse], error) {
	day, err := dashDay(req.Msg.Day)
	if err != nil {
		return nil, err
	}
	tail := int(req.Msg.Tail)
	if tail < 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("tail must be >= 0"))
	}
	if tail == 0 {
		tail = 200
	}
	home, err := dashHome()
	if err != nil {
		return nil, fileOSErr(err, "$LYON_HOME")
	}
	path := filepath.Join(home, "audit", day+".jsonl")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return connect.NewResponse(&lyonv1.ListAuditResponse{}), nil
		}
		return nil, fileOSErr(err, path)
	}
	defer f.Close()

	// Ring of the last `tail` records — a heavy day's file streams past
	// without ever being held whole in memory.
	ring := make([]*lyonv1.AuditRecord, 0, min(tail, 4096))
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec auditLine
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		ring = append(ring, &lyonv1.AuditRecord{
			Time:       rec.Time,
			Event:      rec.Event,
			Session:    rec.Session,
			Tool:       rec.Tool,
			Prompt:     rec.Prompt,
			Detail:     rec.Detail,
			IsError:    rec.IsError,
			Err:        rec.Err,
			DurationMs: rec.DurationMS,
		})
		if len(ring) > tail {
			ring = ring[len(ring)-tail:]
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fileOSErr(err, path)
	}
	return connect.NewResponse(&lyonv1.ListAuditResponse{Records: ring}), nil
}

// ListSnapshots lists the workspace's saved sessions (the /resume
// store) — same call the TUI's history panel makes.
func (s *dashboardService) ListSnapshots(_ context.Context, _ *connect.Request[lyonv1.ListSnapshotsRequest]) (*connect.Response[lyonv1.ListSnapshotsResponse], error) {
	refs, err := agent.ListSnapshots(s.s.eng.Config().Workspace)
	if err != nil {
		return nil, fileOSErr(err, "sessions")
	}
	out := &lyonv1.ListSnapshotsResponse{Snapshots: make([]*lyonv1.SnapshotRef, 0, len(refs))}
	for _, r := range refs {
		out.Snapshots = append(out.Snapshots, &lyonv1.SnapshotRef{
			Title:    r.Title,
			Messages: int32(r.Messages),
			SavedAt:  timestamppb.New(r.SavedAt),
			Path:     r.Path,
		})
	}
	return connect.NewResponse(out), nil
}
