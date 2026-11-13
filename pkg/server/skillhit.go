// skill-hit detection: the hub watches tool_exec_start for reads of a
// SKILL.md under one of the skills roots and, when the read SUCCEEDS,
// injects one skill_used event (files_changed precedent — server-
// synthesized, raw type_name string, no proto enum, rides the replay
// ring so reconnect redraws the card).
//
// Both read paths are covered, because the two behave differently in
// the wild (dump forensics 2026-11-03/04/10):
//   - read_file of a workspace-relative .lyon/skills/.../SKILL.md works;
//   - read_file of an absolute personal path (~/.lyon/…) FAILS with
//     "escapes workspace" (tools.Confine) and the model immediately
//     falls back to run_shell (head/sed/cat on the same file).
// So the start handler stashes candidates for read_file AND for
// run_shell commands naming a skills-root SKILL.md; the end handler
// fires the card only for executions that did NOT error — a rejected
// absolute read never cards, the run_shell fallback does.

package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"lyon-agent/pkg/agent"
)

// evSkillUsed is the synthesized event type. Like evFilesChanged it is
// a raw string: type_name on the wire, unknown-but-renderable by older
// clients, ignored by the TUI's 只减不增 reducer.
const evSkillUsed = agent.EventType("skill_used")

// skillHit is one resolved skill invocation — the card's payload and
// the panel row's identity.
type skillHit struct {
	name string
	path string // absolute SKILL.md path (attrs.path)
	desc string // frontmatter description when the skill is installed (attrs.desc)
}

// shellSkillToken finds path-shaped tokens ending in SKILL.md inside a
// run_shell command. The class stops at whitespace and shell metachars;
// a bare "SKILL.md" (after a cd) still matches and then fails the roots
// check below — it names a foreign repo's file, not one of ours.
var shellSkillToken = regexp.MustCompile(`[^\s"'&;|<>]*SKILL\.md`)

// noteSkill stashes a candidate skill read on tool_exec_start — the
// only exec event that carries Input. Consumer-goroutine only (run's
// loop), no lock.
func (h *hub) noteSkill(ev agent.Event) {
	hit, ok := skillHitFromInput(h.ws, ev.ToolName, ev.Input)
	if !ok {
		return
	}
	if h.skillPending == nil {
		h.skillPending = map[uint64]skillHit{}
	}
	h.skillPending[ev.ExecID] = hit
}

// skillHitEnd fires the card for a skill read that succeeded, exactly
// once per skill per run: chunked re-reads (head + sed ranges, or a
// second read_file) collapse onto the first hit, and prompt_end resets
// lastSkill so the next run may card it again. ExecIDs are unique per
// session (main flow and subagents share Session.seq).
func (h *hub) skillHitEnd(ev agent.Event) {
	hit, ok := h.skillPending[ev.ExecID]
	delete(h.skillPending, ev.ExecID)
	if !ok || ev.IsError {
		return
	}
	if hit.name == h.lastSkill {
		return
	}
	h.lastSkill = hit.name
	h.inject(agent.Event{
		Type: evSkillUsed,
		Attrs: map[string]string{
			"name": hit.name,
			"path": hit.path,
			"desc": hit.desc,
		},
	})
}

// skillHitFromInput resolves one tool_exec_start into a skill read.
func skillHitFromInput(ws, toolName string, input json.RawMessage) (skillHit, bool) {
	switch toolName {
	case "read_file":
		var in struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return skillHit{}, false
		}
		return skillFromPath(ws, in.Path)
	case "run_shell":
		var in struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return skillHit{}, false
		}
		if !strings.Contains(in.Command, "SKILL.md") {
			return skillHit{}, false
		}
		for _, tok := range shellSkillToken.FindAllString(in.Command, -1) {
			tok = strings.TrimLeft(tok, `({[$`)
			if hit, ok := skillFromPath(ws, tok); ok {
				return hit, true
			}
		}
		return skillHit{}, false
	}
	return skillHit{}, false
}

// skillFromPath resolves one path to the skill it names: it must be a
// SKILL.md sitting under a skills root (personal / commons / workspace),
// fail-closed. The identity prefers the installed brief (frontmatter
// name + description); an unscanned-but-valid file falls back to its
// parent directory name so a hand-dropped SKILL.md still cards.
func skillFromPath(ws, p string) (skillHit, bool) {
	p = strings.TrimSpace(strings.Trim(p, `"'`))
	if p == "" {
		return skillHit{}, false
	}
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return skillHit{}, false
		}
		p = filepath.Join(home, p[1:])
	}
	if !filepath.IsAbs(p) {
		if ws == "" {
			return skillHit{}, false
		}
		p = filepath.Join(ws, p)
	}
	p = filepath.Clean(p)
	if !strings.EqualFold(filepath.Base(p), "SKILL.md") {
		return skillHit{}, false
	}
	for _, r := range agent.SkillRoots(ws) {
		if !underRoot(r.Dir, p) {
			continue
		}
		hit := skillHit{name: filepath.Base(filepath.Dir(p)), path: p}
		for _, s := range agent.ListSkills(ws) {
			if strings.EqualFold(filepath.Clean(s.Path), p) {
				hit.name, hit.desc = s.Name, s.Description
				break
			}
		}
		return hit, true
	}
	return skillHit{}, false
}

// underRoot reports whether p sits inside dir, boundary- and
// case-insensitively: the model retypes paths from the system prompt
// (case drifts) and a sibling directory sharing the prefix must not
// count (skills2 vs skills).
func underRoot(dir, p string) bool {
	dir = filepath.Clean(dir)
	p = filepath.Clean(p)
	if len(p) <= len(dir) || !strings.EqualFold(p[:len(dir)], dir) {
		return false
	}
	rest := p[len(dir):]
	return rest[0] == filepath.Separator || rest[0] == '/'
}
