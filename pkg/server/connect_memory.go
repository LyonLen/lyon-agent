// MemoryService — the three memory layers (team / personal under
// $LYON_HOME, project under the workspace) for the web panel. Handlers go
// through pkg/memory's process-wide Store (memory.Open caches per
// workspace), the SAME instance the agent's save_memory tool mutates, so
// panel edits, model writes and the search index stay in one place; the
// $LYON_HOME resolution lives in homeDir() server-side, which is exactly
// what the browser could never do through FileService.

package server

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	lyonv1 "lyon-agent/gen/go/lyon/v1"
	"lyon-agent/gen/go/lyon/v1/lyonv1connect"
	"lyon-agent/pkg/memory"
)

type memoryService struct {
	s *Server
}

var _ lyonv1connect.MemoryServiceHandler = (*memoryService)(nil)

func (m *memoryService) store() *memory.Store {
	return memory.Open(m.s.eng.Config().Workspace)
}

// validMemScope mirrors pkg/memory's unexported validScope — reject bad
// scopes here instead of letting Store silently coerce them to project.
func validMemScope(scope string) bool {
	return scope == memory.ScopeTeam || scope == memory.ScopePersonal || scope == memory.ScopeProject
}

func factProto(f memory.Fact) *lyonv1.MemoryFact {
	return &lyonv1.MemoryFact{
		Name:        f.Name,
		Description: f.Description,
		Scope:       f.Scope,
		Tags:        f.Tags,
		Hot:         f.Hot,
		Hits:        int32(f.Hits),
		Body:        f.Body,
	}
}

func (m *memoryService) ListMemory(_ context.Context, _ *connect.Request[lyonv1.ListMemoryRequest]) (*connect.Response[lyonv1.ListMemoryResponse], error) {
	// "" = all scopes; the store already orders project → personal → team
	// (closest first), name-ascending within a scope.
	facts := m.store().List("")
	out := make([]*lyonv1.MemoryFact, 0, len(facts))
	for _, f := range facts {
		out = append(out, factProto(f))
	}
	return connect.NewResponse(&lyonv1.ListMemoryResponse{Facts: out}), nil
}

func (m *memoryService) UpdateMemory(_ context.Context, req *connect.Request[lyonv1.UpdateMemoryRequest]) (*connect.Response[lyonv1.UpdateMemoryResponse], error) {
	msg := req.Msg
	if !validMemScope(msg.Scope) {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("bad memory scope %q (want team|personal|project)", msg.Scope))
	}
	st := m.store()
	f, ok := st.Get(msg.Scope, msg.Name)
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("no memory %q in scope %q", msg.Name, msg.Scope))
	}
	// Text fields ride Save (which preserves Created/Hits and — by design —
	// the stored hot flag; hot only ever moves through ToggleHot).
	if msg.Description != nil || msg.Body != nil {
		if msg.Description != nil {
			f.Description = *msg.Description
		}
		if msg.Body != nil {
			f.Body = *msg.Body
		}
		saved, err := st.Save(f)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		f = saved
	}
	// hot is a SET: toggle only when it differs (ToggleHot flips).
	if msg.Hot != nil && *msg.Hot != f.Hot {
		if _, err := st.ToggleHot(f.Scope, f.Name); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		if cur, ok := st.Get(f.Scope, f.Name); ok {
			f = cur
		}
	}
	return connect.NewResponse(&lyonv1.UpdateMemoryResponse{Fact: factProto(f)}), nil
}

func (m *memoryService) DeleteMemory(_ context.Context, req *connect.Request[lyonv1.DeleteMemoryRequest]) (*connect.Response[lyonv1.DeleteMemoryResponse], error) {
	msg := req.Msg
	if !validMemScope(msg.Scope) {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("bad memory scope %q (want team|personal|project)", msg.Scope))
	}
	if err := m.store().Delete(msg.Scope, msg.Name); err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewResponse(&lyonv1.DeleteMemoryResponse{}), nil
}
