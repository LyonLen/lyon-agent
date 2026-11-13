// SkillService — the skills panel's read side: the merged installed
// list (personal + commons + workspace .lyon/skills, same roots the
// skill-hit detector watches) and the SKILL.md viewer behind a
// click-to-expand row. GetSkill fails closed: a path outside the roots
// is InvalidArgument, never an arbitrary file read.

package server

import (
	"context"
	"fmt"
	"os"
	"strings"

	"connectrpc.com/connect"

	lyonv1 "lyon-agent/gen/go/lyon/v1"
	"lyon-agent/gen/go/lyon/v1/lyonv1connect"
	"lyon-agent/pkg/agent"
)

// skillContentCap clips one SKILL.md served to the panel — skills are
// prompt documents, not payloads; 256KB is far past any real one.
const skillContentCap = 256 << 10

type skillService struct {
	s *Server
}

var _ lyonv1connect.SkillServiceHandler = (*skillService)(nil)

func (s *skillService) ListSkills(_ context.Context, req *connect.Request[lyonv1.ListSkillsRequest]) (*connect.Response[lyonv1.ListSkillsResponse], error) {
	// The header follows the selected session's workspace, so the list
	// and the slash entries include the workspace's own .lyon/skills.
	skills := agent.ListSkills(s.s.wsFor(req.Header()))
	out := make([]*lyonv1.Skill, 0, len(skills))
	for _, sk := range skills {
		out = append(out, &lyonv1.Skill{
			Name:        sk.Name,
			Description: sk.Description,
			Path:        sk.Path,
			Source:      sk.Source,
		})
	}
	return connect.NewResponse(&lyonv1.ListSkillsResponse{Skills: out}), nil
}

func (s *skillService) GetSkill(_ context.Context, req *connect.Request[lyonv1.GetSkillRequest]) (*connect.Response[lyonv1.GetSkillResponse], error) {
	msg := req.Msg
	ws := s.s.wsFor(req.Header())
	name, desc, path, err := resolveSkill(ws, msg.Name, msg.Path)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("%s: %w", name, err))
	}
	truncated := false
	if len(raw) > skillContentCap {
		raw, truncated = raw[:skillContentCap], true
	}
	return connect.NewResponse(&lyonv1.GetSkillResponse{
		Name:        name,
		Description: desc,
		Path:        path,
		Content:     string(raw),
		Truncated:   truncated,
	}), nil
}

// resolveSkill picks the SKILL.md to serve: a non-empty path must sit
// under a skills root (the panel's hit rows carry one), otherwise the
// name is looked up in the merged list — first root wins, exactly like
// ListSkills' dedup.
func resolveSkill(ws, name, path string) (string, string, string, error) {
	if strings.TrimSpace(path) != "" {
		hit, ok := skillFromPath(ws, path)
		if !ok {
			return "", "", "", fmt.Errorf("path is not a SKILL.md under a skills root: %q", path)
		}
		return hit.name, hit.desc, hit.path, nil
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", "", "", fmt.Errorf("name or path required")
	}
	for _, sk := range agent.ListSkills(ws) {
		if strings.EqualFold(sk.Name, name) {
			return sk.Name, sk.Description, sk.Path, nil
		}
	}
	return "", "", "", fmt.Errorf("no installed skill %q", name)
}
