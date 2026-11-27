package server

// ProcessService — the background-shell process panel (proto
// process.proto): live tasks started by sessions through run_shell
// background=true. List is a snapshot of pkg/tools' registry with the
// owner key resolved to the live seat's id/title (best-effort: a parked
// or exited seat still shows its task, just without the title). Kill is
// the same tree-kill the owner sweep uses — the waiter unregisters on
// exit, so the next List no longer carries the row.

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	lyonv1 "lyon-agent/gen/go/lyon/v1"
	"lyon-agent/gen/go/lyon/v1/lyonv1connect"
	"lyon-agent/pkg/tools"
)

type processService struct {
	s *Server
}

var _ lyonv1connect.ProcessServiceHandler = (*processService)(nil)

func (p *processService) ListProcesses(_ context.Context, _ *connect.Request[lyonv1.ListProcessesRequest]) (*connect.Response[lyonv1.ListProcessesResponse], error) {
	// owner bgKey → the live seat behind it. bgKey is process-local
	// ("session-N"), so this only matches while the session lives —
	// tasks outliving their seat fall back to showing the raw owner.
	type seatMeta struct{ id, title string }
	byOwner := make(map[string]seatMeta)
	for _, sess := range p.s.store.Sessions() {
		byOwner[sess.BgKey()] = seatMeta{id: p.s.store.IDOf(sess), title: sess.Title()}
	}
	live := tools.ListBackground()
	out := make([]*lyonv1.ProcessInfo, 0, len(live))
	for _, b := range live {
		meta := byOwner[b.Owner]
		out = append(out, &lyonv1.ProcessInfo{
			Id:           int32(b.ID),
			Command:      b.Command,
			Pid:          int32(b.PID),
			Owner:        b.Owner,
			SessionId:    meta.id,
			SessionTitle: meta.title,
			LogPath:      b.Log,
			StartedUnix:  b.Started.Unix(),
			Tail:         b.Tail,
		})
	}
	return connect.NewResponse(&lyonv1.ListProcessesResponse{Processes: out}), nil
}

func (p *processService) KillProcess(_ context.Context, req *connect.Request[lyonv1.KillProcessRequest]) (*connect.Response[lyonv1.KillProcessResponse], error) {
	if !tools.KillBackground(int(req.Msg.Id)) {
		// Already exited between the panel's list and this click — the
		// row is stale, not broken; NotFound lets the client just reload.
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("process #%d not found", req.Msg.Id))
	}
	return connect.NewResponse(&lyonv1.KillProcessResponse{}), nil
}
