package server

// MindmapService — the live conversation mind-map RPCs
// (docs/design/mindmap.md §3/§7.3). State lives in pkg/mindmap; this
// layer is seat resolution, error mapping, and the two event seams: the
// distiller's structure diff and a human's edit action.

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"connectrpc.com/connect"

	lyonv1 "lyon-agent/gen/go/lyon/v1"
	"lyon-agent/gen/go/lyon/v1/lyonv1connect"
	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/hooks"
	"lyon-agent/pkg/mindmap"
)

type mindmapService struct {
	s *Server
}

var _ lyonv1connect.MindmapServiceHandler = (*mindmapService)(nil)

// GetMindmap is the /resume rebuild path (§2): the map is NOT in the
// session snapshot — opening a seat pulls it here. A seat with no map
// yet answers revision 0 + "[]", a valid state rather than an error.
func (m *mindmapService) GetMindmap(_ context.Context,
	req *connect.Request[lyonv1.GetMindmapRequest]) (*connect.Response[lyonv1.GetMindmapResponse], error) {
	sid := req.Msg.SessionId
	if _, err := m.s.seatRef(sid); err != nil {
		return nil, rpcErr(err)
	}
	rev, title, subtitle, nodes, err := mindmap.Open().Snapshot(sid)
	if err != nil {
		return nil, mindmapErr(err)
	}
	return connect.NewResponse(&lyonv1.GetMindmapResponse{
		Revision:  int32(rev),
		Title:     title,
		Subtitle:  subtitle,
		NodesJson: nodes,
	}), nil
}

// UpdateMindmap applies one map_ops batch (§7.3) under the §5 optimistic
// lock and §4.1 authority rules, all enforced inside pkg/mindmap:
//
//	origin == "user" → the human on the card: full authority, touched
//	nodes pin; any other origin → agent map_ops: never modifies a
//	pinned/confirmed node (FailedPrecondition), new nodes land proposed.
//
// A revision whose gap contains a human edit → FailedPrecondition (人工
// 优先). One bad op rejects the whole batch (InvalidArgument) — the map
// never half-applies a request.
func (m *mindmapService) UpdateMindmap(_ context.Context,
	req *connect.Request[lyonv1.UpdateMindmapRequest]) (*connect.Response[lyonv1.UpdateMindmapResponse], error) {
	msg := req.Msg
	sid := msg.SessionId
	if _, err := m.s.seatRef(sid); err != nil {
		return nil, rpcErr(err)
	}
	origin := msg.Origin // "" is NOT user — the human must say so
	rev, nodes, changed, err := mindmap.Open().ApplyUserOps(sid, int(msg.Revision), origin, msg.OpsJson)
	if err != nil {
		return nil, mindmapErr(err)
	}
	if changed {
		m.s.eng.Hooks().Fire(hooks.Record{
			Event:     mindmap.EventUpdate,
			SessionID: sid,
			Detail:    fmt.Sprintf("origin=%s rev=%d ops=%s", origin, rev, clipDetail(msg.OpsJson, 400)),
		})
		// §4.2 security boundary: this event carries ONLY the ops JSON and
		// the post-edit snapshot — no executable intent (tool_name / input)
		// ever rides the map → chat channel. At most the web turns it into
		// one chat message; tool execution stays behind the approval chain.
		if !mindmap.Off() {
			if h, herr := m.s.store.Hub(sid); herr == nil {
				h.inject(agent.Event{
					Type: mindmap.TypeEdit,
					Attrs: map[string]string{
						"origin":   origin,
						"revision": strconv.Itoa(rev),
						"nodes":    nodes,
						"ops":      msg.OpsJson,
					},
				})
			}
		}
	}
	return connect.NewResponse(&lyonv1.UpdateMindmapResponse{
		Revision:  int32(rev),
		NodesJson: nodes,
	}), nil
}

// PublishMindmap is the distiller's wire seam: cmd/lyond points
// mindmap.Open's publisher here so an applied diff becomes
// EVENT_MINDMAP_UPDATE on the seat's hub. Injects only when the seat is
// live — a parked seat has no ring, and adopting one just because the
// model finished a diff would drag an idle conversation into memory (the
// next reader pulls the map through GetMindmap anyway).
func (s *Server) PublishMindmap(sid string, ev agent.Event) {
	if h, err := s.store.Hub(sid); err == nil {
		h.inject(ev)
	}
}

// mindmapErr maps pkg/mindmap's sentinels onto connect codes (§5/§4.1).
func mindmapErr(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, mindmap.ErrConflict), errors.Is(err, mindmap.ErrFrozen):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, mindmap.ErrInvalid):
		return connect.NewError(connect.CodeInvalidArgument, err)
	default:
		return connect.NewError(connect.CodeInternal, err)
	}
}

// clipDetail bounds an audit Detail line (hooks truncates Input/Output/
// Prompt but not Detail; ops_json is client-supplied).
func clipDetail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
