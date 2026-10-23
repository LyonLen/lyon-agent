package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"lyon-agent/pkg/flow"
	"lyon-agent/pkg/space"
)

// SkillBrief is one on-disk skill as advertised in the system prompt:
// just enough identity (name + one-line description + path) for the model
// to decide whether to read the full SKILL.md with read_file. No mounting,
// no executor — visibility only (docs/planning/archive/EVOLUTION-GAPS.md task E2).
type SkillBrief struct {
	Name        string
	Description string
	Path        string // absolute path of the SKILL.md file
}

const (
	// skillsSectionBudget caps the whole injected section in runes, so a
	// large skills directory cannot bloat every request of the session.
	skillsSectionBudget = 2000
	// skillsDescBudget caps one entry's description in runes (long
	// frontmatter descriptions get cut; the full file stays readable).
	skillsDescBudget = 160
	// skillsNameBudget caps one entry's name in runes.
	skillsNameBudget = 64
	// skillsTailMax reserves room for the truncation tail line
	// ("- ... and N more skill(s)") so it can never push the section past
	// skillsSectionBudget.
	skillsTailMax = 32
)

// skillsDir resolves the scan root: LYON_SKILLS_DIR overrides; the default
// is ~/.claude/skills (the Claude Code skill location — same files, reused
// read-only). Empty when the home directory cannot be determined; callers
// treat that as "no skills" so skill visibility never breaks startup.
func skillsDir() string {
	if d := strings.TrimSpace(os.Getenv("LYON_SKILLS_DIR")); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "skills")
}

// ScanSkills lists */SKILL.md under dir ("" = skillsDir()). It never
// fails: an unreadable directory yields an empty list and a bad file is
// skipped — skills are a bonus, not a startup dependency.
func ScanSkills(dir string) []SkillBrief {
	if dir == "" {
		dir = skillsDir()
	}
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []SkillBrief
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if !e.IsDir() {
			// DirEntry reports the raw type: Windows junctions and
			// symlinks to directories come back non-dir. Stat follows
			// the link, so a skills dir reached through one still
			// counts (the real ~/.claude/skills uses junctions).
			if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
				continue
			}
		}
		brief, ok := parseSkillFile(filepath.Join(p, "SKILL.md"), e.Name())
		if !ok {
			continue
		}
		out = append(out, brief)
	}
	// ReadDir order is platform-defined; sort by name so the injected
	// section is byte-stable across scans (prompt-cache friendly).
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// parseSkillFile reads one SKILL.md and extracts name + description.
// ok=false marks a bad file (missing, unreadable, or frontmatter that
// opened but never closed) — skipped without failing the scan. When the
// frontmatter lacks a field, the directory name and the first
// heading/paragraph stand in.
func parseSkillFile(path, dirName string) (SkillBrief, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return SkillBrief{}, false
	}
	fm, body, ok := splitFrontmatter(string(raw))
	if !ok {
		return SkillBrief{}, false
	}
	name := fmScalar(fm, "name")
	desc := fmScalar(fm, "description")
	if name == "" {
		name = dirName
	}
	if desc == "" {
		desc = firstSummary(body)
	}
	if name == "" && desc == "" {
		return SkillBrief{}, false
	}
	// Whitespace-fold the description into one line — it rides as a list
	// item in the system prompt.
	desc = strings.Join(strings.Fields(desc), " ")
	return SkillBrief{
		Name:        clipRunes(name, skillsNameBudget),
		Description: clipRunes(desc, skillsDescBudget),
		Path:        path,
	}, true
}

// splitFrontmatter separates a leading "---" fenced YAML block from the
// body. ok=false means the fence opened but never closed (a bad file).
// No leading fence is fine: the whole text is the body.
func splitFrontmatter(text string) (fm, body string, ok bool) {
	text = strings.TrimPrefix(text, "\uFEFF")
	lines := strings.Split(text, "\n")
	if strings.TrimRight(lines[0], "\r") != "---" {
		return "", text, true
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], "\r") == "---" {
			return strings.Join(lines[1:i], "\n"), strings.Join(lines[i+1:], "\n"), true
		}
	}
	return "", "", false
}

// fmScalar scans YAML-ish frontmatter for one top-level scalar key. Only
// column-0 lines count, so nested keys (metadata.*, tags[]) can never
// shadow name/description. One layer of surrounding quotes is stripped.
// Block scalars (|, >, with chomping indicators) are joined from their
// indented continuation lines — common enough in real skill files that
// showing the bare "|" instead would be a lie about the description.
func fmScalar(fm, key string) string {
	prefix := key + ":"
	lines := strings.Split(fm, "\n")
	for i, line := range lines {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		v := strings.TrimSpace(strings.TrimPrefix(line, prefix))
		if isBlockScalar(v) {
			var parts []string
			for j := i + 1; j < len(lines); j++ {
				l := strings.TrimRight(lines[j], "\r")
				if strings.TrimSpace(l) == "" {
					parts = append(parts, "")
					continue
				}
				if l[0] != ' ' && l[0] != '\t' {
					break // next top-level key ends the block
				}
				parts = append(parts, strings.TrimSpace(l))
			}
			return strings.TrimSpace(strings.Join(parts, " "))
		}
		if n := len(v); n >= 2 && (v[0] == '"' || v[0] == '\'') && v[n-1] == v[0] {
			v = v[1 : n-1]
		}
		return strings.TrimSpace(v)
	}
	return ""
}

// isBlockScalar reports whether a YAML scalar value opens an indented
// block: | or > with optional chomping/indent indicators.
func isBlockScalar(v string) bool {
	switch v {
	case "|", "|-", "|+", ">", ">-", ">+":
		return true
	}
	return false
}

// firstSummary is the no-frontmatter (or field-less) fallback: the first
// markdown heading plus the first paragraph, joined on one line.
func firstSummary(body string) string {
	var heading string
	var para []string
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if len(para) == 0 {
			if t == "" || t == "---" {
				continue
			}
			if strings.HasPrefix(t, "#") {
				if heading == "" {
					heading = strings.TrimSpace(strings.TrimLeft(t, "#"))
				}
				continue
			}
		} else if t == "" || strings.HasPrefix(t, "#") {
			break // paragraph ended
		}
		para = append(para, t)
	}
	text := strings.Join(para, " ")
	switch {
	case heading != "" && text != "":
		return heading + " — " + text
	case heading != "":
		return heading
	default:
		return text
	}
}

// skillsSection renders the system-prompt block advertising the scanned
// skills, or "" when there are none — an empty block would waste bytes and
// say nothing, so it simply is not injected. The whole section is capped at
// skillsSectionBudget runes; an overflowing list stops with a "+N more"
// line so the model knows the list is partial. Consensus-shared skills
// (pkg/space commons) are scanned alongside the personal directory: a
// fact two agents endorsed is knowledge every session should carry.
func skillsSection() string {
	skills := ScanSkills("")
	if commons := ScanSkills(space.CommonsSkillsDir()); len(commons) > 0 {
		seen := make(map[string]bool, len(skills))
		for _, s := range skills {
			seen[s.Name] = true
		}
		for _, c := range commons {
			if !seen[c.Name] {
				skills = append(skills, c)
			}
		}
		sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	}
	return renderSkillsSection(skills) + renderFlowsSection(flow.ListScripts())
}

func renderSkillsSection(skills []SkillBrief) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n## Skills available (use read_file for full text)\n")
	b.WriteString("Optional playbooks on disk, one per line (name — description, path). To follow one, read its file with read_file first.\n")
	used := len([]rune(b.String()))
	for i, sk := range skills {
		line := fmt.Sprintf("- %s: %s (%s)\n", sk.Name, sk.Description, sk.Path)
		n := len([]rune(line))
		// While more entries could follow, keep enough room for the
		// truncation tail so the "+N more" line itself cannot push the
		// section past the budget (skillsTailMax covers a 7-digit count).
		room := skillsSectionBudget
		if i+1 < len(skills) {
			room -= skillsTailMax
		}
		if used+n > room {
			if i == 0 {
				// One oversized entry still gets in, clipped — the
				// section must not come out empty after a scan hit.
				line = fmt.Sprintf("- %s: %s (%s)\n", sk.Name, clipRunes(sk.Description, 64), sk.Path)
				b.WriteString(line)
				used += len([]rune(line))
			}
			if i+1 < len(skills) {
				fmt.Fprintf(&b, "- ... and %d more skill(s)\n", len(skills)-i)
			}
			return b.String()
		}
		b.WriteString(line)
		used += n
	}
	return b.String()
}

// renderFlowsSection advertises frozen workflow scripts (pkg/flow). These
// differ from skills: a skill is a document the model reads and interprets
// step by step, while a flow is an already-fixed sequence of shell steps the
// model invokes with ONE flow call and never re-derives. Empty when no flows
// are frozen — the section then is not injected at all.
func renderFlowsSection(scripts []flow.Script) string {
	if len(scripts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n## Frozen workflows (run with the flow tool, no need to re-derive steps)\n")
	b.WriteString("Reusable command sequences fixed after repeated success. Call `flow` with the slug and any args — it runs every step for you. One line: slug: description (args).\n")
	used := len([]rune(b.String()))
	for i, s := range scripts {
		argList := strings.Join(s.Args, ", ")
		desc := s.Description
		if desc == "" {
			desc = fmt.Sprintf("%d step(s)", len(s.Steps))
		}
		line := fmt.Sprintf("- %s: %s (%s)\n", s.Slug, desc, argList)
		n := len([]rune(line))
		room := skillsSectionBudget
		if i+1 < len(scripts) {
			room -= skillsTailMax
		}
		if used+n > room {
			if i == 0 {
				line = fmt.Sprintf("- %s: %s (%s)\n", s.Slug, clipRunes(desc, 64), argList)
				b.WriteString(line)
				used += len([]rune(line))
			}
			if i+1 < len(scripts) {
				fmt.Fprintf(&b, "- ... and %d more workflow(s)\n", len(scripts)-i)
			}
			return b.String()
		}
		b.WriteString(line)
		used += n
	}
	return b.String()
}
