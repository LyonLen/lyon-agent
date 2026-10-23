package space

// Agent spaces (2026-10-23 重定义): every evolved agent owns two
// specialist stores under $LYON_HOME/agents/<id>/ — knowledge/*.md
// (markdown documents) and skills/<name>/SKILL.md (the same format
// ~/.claude/skills uses, so the scanner reads them unchanged).
// $LYON_HOME/commons/{knowledge,skills}/ is the consensus tier: an item
// promoted there is shared by every agent (commons skills ride the
// system prompt's skills section).
//
// Consensus (共识共享) is endorsement counting. An item starts endorsed
// only by its owner. A second DISTINCT source — another agent saving an
// identical body (two agents converged on the same fact), or a contest
// win ("contest:<class>") — promotes the item to commons. Promotion
// copies, never moves: the specialist keeps its own space, commons
// gains the shared fact.
//
// No imports from pkg/agent or pkg/eval: this package is the disk
// layer; session identity and evolution policy live above it.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// mu serializes all space writes — endorsement updates rewrite whole
// files, so read-modify-write must not interleave.
var mu sync.Mutex

// homeDir resolves $LYON_HOME (or ~/.lyon), mirroring flow/eval.
func homeDir() string {
	if h := os.Getenv("LYON_HOME"); h != "" {
		return h
	}
	h, err := os.UserHomeDir()
	if err != nil {
		h = "."
	}
	return filepath.Join(h, ".lyon")
}

// AgentsDir is the candidate registry AND the space root: <id>.json
// profiles sit beside <id>/ space directories (LoadCandidates skips
// directories, so the two never collide).
func AgentsDir() string { return filepath.Join(homeDir(), "agents") }

// CommonsDir is the consensus tier every agent can see.
func CommonsDir() string { return filepath.Join(homeDir(), "commons") }

// CommonsSkillsDir is the commons skill directory in the exact layout
// agent.ScanSkills consumes (dir-per-skill with SKILL.md inside).
func CommonsSkillsDir() string { return filepath.Join(CommonsDir(), "skills") }

func agentKnowledgeDir(id string) string { return filepath.Join(AgentsDir(), id, "knowledge") }
func agentSkillsDir(id string) string    { return filepath.Join(AgentsDir(), id, "skills") }

func commonsKnowledgeDir() string { return filepath.Join(CommonsDir(), "knowledge") }

// agentRe matches a space directory name — same shape as the candidate
// id regex, so every legal candidate has exactly one legal space.
var agentRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// endorserRe matches a valid endorsement source: an agent id, or a
// contest election result ("contest:<class>"). Elections are the
// strongest endorsement — a winner's space earns commons on a win.
var endorserRe = regexp.MustCompile(`^(contest:[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}|[a-zA-Z0-9][a-zA-Z0-9_-]{0,63})$`)

// Kind selects which of the two specialist stores an operation applies to.
const (
	KindKnowledge = "knowledge"
	KindSkills    = "skills"
)

// Item is one knowledge document or skill as listed by List*: identity,
// one-line description, and the endorsement ledger (distinct sources,
// owner first). Endorsed() ≥ 2 means consensus was reached.
type Item struct {
	Name         string   `json:"name"`
	Description  string   `json:"description,omitempty"`
	Endorsements []string `json:"endorments,omitempty"`
	Path         string   `json:"path"`
}

// Endorsed reports distinct-endorser consensus: owner + at least one
// independent source.
func (it Item) Endorsed() int { return len(it.Endorsements) }

func slugify(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ' || r == '/' || r == '\\':
			b.WriteRune('-')
		}
	}
	s := strings.Trim(b.String(), "-_")
	if s == "" {
		s = "item"
	}
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

// nameAvailable reports whether path holds an item whose name differs
// from want — a slug collision that must get a numeric suffix instead
// of silently shadowing a different document.
func nameAvailable(path, want string) bool {
	cur, _, err := readItem(path)
	if err != nil {
		return true
	}
	return cur.Name == want
}

// uniquePath returns a non-colliding path under dir for a name: the
// plain slug when free, slug-2, slug-3… when taken by a different name.
func uniquePath(dir, name string) string {
	slug := slugify(name)
	path := filepath.Join(dir, slug+".md")
	for i := 2; !nameAvailable(path, name); i++ {
		path = filepath.Join(dir, fmt.Sprintf("%s-%d.md", slug, i))
	}
	return path
}

// renderItem wires the frontmatter around the body. The endorsement
// ledger rides in the frontmatter so a Share rewrite is a whole-file
// rewrite with no second sidecar file to fall out of sync.
func renderItem(it Item, body string) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "name: %s\n", it.Name)
	if it.Description != "" {
		fmt.Fprintf(&b, "description: %s\n", firstLine(it.Description))
	}
	fmt.Fprintf(&b, "owner: %s\n", it.Endorsements[0])
	if len(it.Endorsements) > 1 {
		fmt.Fprintf(&b, "endorsed-by: %s\n", strings.Join(it.Endorsements[1:], ", "))
	}
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimSpace(body) + "\n")
	return b.String()
}

// readItem parses one item file back into identity + body.
func readItem(path string) (Item, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Item{}, "", err
	}
	var it Item
	text := strings.TrimPrefix(string(raw), string(rune(0xFEFF)))
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return Item{}, "", fmt.Errorf("space: %s has no frontmatter", path)
	}
	bodyStart := -1
	for i := 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "---" {
			bodyStart = i + 1
			break
		}
		k, v, ok := strings.Cut(lines[i], ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "name":
			it.Name = v
		case "description":
			it.Description = v
		case "owner":
			it.Endorsements = append(it.Endorsements, v)
		case "endorsed-by":
			for _, e := range strings.Split(v, ",") {
				if e = strings.TrimSpace(e); e != "" {
					it.Endorsements = append(it.Endorsements, e)
				}
			}
		}
	}
	if bodyStart < 0 {
		return Item{}, "", fmt.Errorf("space: %s frontmatter never closed", path)
	}
	body := strings.TrimSpace(strings.Join(lines[bodyStart:], "\n"))
	it.Path = path
	return it, body, nil
}

// bodyHash fingerprints an item's body for the consensus check —
// whitespace-trimmed so re-saving the same fact is the same fact.
func bodyHash(body string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(body)))
	return hex.EncodeToString(sum[:8])
}

// SaveKnowledge writes one knowledge document into agent's space and
// runs the consensus check: another agent holding an identical body
// gains a mutual endorsement, and any item that reaches two distinct
// sources promotes to commons.
func SaveKnowledge(agent, name, desc, body string) (Item, error) {
	if !agentRe.MatchString(agent) {
		return Item{}, fmt.Errorf("space: bad agent id %q", agent)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return Item{}, errors.New("space: knowledge needs a name")
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return Item{}, errors.New("space: knowledge needs a body")
	}

	mu.Lock()
	defer mu.Unlock()

	dir := agentKnowledgeDir(agent)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Item{}, err
	}
	path := uniquePath(dir, name)
	it := Item{Name: name, Description: strings.TrimSpace(desc), Endorsements: []string{agent}}
	if err := writeItem(path, it, body); err != nil {
		return Item{}, err
	}

	// Consensus check: identical bodies elsewhere mean two agents
	// converged on the same fact — the strongest possible endorsement.
	h := bodyHash(body)
	for _, other := range listAgentsLocked() {
		if other == agent {
			continue
		}
		for _, oit := range listKindLocked(other, KindKnowledge) {
			_, obody, err := readItem(oit.Path)
			if err != nil || bodyHash(obody) != h {
				continue
			}
			// Mutual endorsement, then promote both if the bar is met.
			endorseLocked(oit.Path, agent)
			endorseLocked(path, other)
			promoteLocked(path, KindKnowledge)
			promoteLocked(oit.Path, KindKnowledge)
		}
	}
	return it, nil
}

// SaveSkill writes one skill into agent's space as
// skills/<slug>/SKILL.md — the layout ScanSkills consumes, so the
// agent's own skills render through the same scanner.
func SaveSkill(agent, name, desc, body string) (Item, error) {
	if !agentRe.MatchString(agent) {
		return Item{}, fmt.Errorf("space: bad agent id %q", agent)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return Item{}, errors.New("space: skill needs a name")
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return Item{}, errors.New("space: skill needs a body")
	}

	mu.Lock()
	defer mu.Unlock()

	dir := filepath.Join(agentSkillsDir(agent), slugify(name))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Item{}, err
	}
	path := filepath.Join(dir, "SKILL.md")
	it := Item{Name: name, Description: strings.TrimSpace(desc), Endorsements: []string{agent}}
	if err := writeItem(path, it, body); err != nil {
		return Item{}, err
	}
	return it, nil
}

// writeItem serializes an item + body to path.
func writeItem(path string, it Item, body string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(renderItem(it, body)), 0o644)
}

// Share records one endorsement from endorser on agent's item and
// promotes to commons when the ledger reaches two distinct sources.
// kind is KindKnowledge or KindSkills. Idempotent: re-sharing from the
// same source changes nothing.
func Share(agent, kind, name, endorser string) (promoted bool, err error) {
	if !agentRe.MatchString(agent) || !endorserRe.MatchString(endorser) {
		return false, fmt.Errorf("space: bad agent/endorser id %q/%q", agent, endorser)
	}
	if agent == endorser {
		return false, errors.New("space: an item is already endorsed by its owner")
	}
	mu.Lock()
	defer mu.Unlock()

	path := findLocked(agent, kind, name)
	if path == "" {
		return false, fmt.Errorf("space: %s %q not found in agent %q", kind, name, agent)
	}
	changed, err := endorseLocked(path, endorser)
	if err != nil {
		return false, err
	}
	if !changed {
		return false, nil
	}
	return promoteLocked(path, kind)
}

// ShareAll endorses every item of agent with one source — the contest
// winner's whole space becomes consensus material at once. Returns the
// names promoted to commons.
func ShareAll(agent, endorser string) []string {
	if !agentRe.MatchString(agent) || !endorserRe.MatchString(endorser) {
		return nil
	}
	mu.Lock()
	defer mu.Unlock()

	var promoted []string
	for _, kind := range []string{KindKnowledge, KindSkills} {
		for _, it := range listKindLocked(agent, kind) {
			changed, err := endorseLocked(it.Path, endorser)
			if err != nil || !changed {
				continue
			}
			if ok, _ := promoteLocked(it.Path, kind); ok {
				promoted = append(promoted, it.Name)
			}
		}
	}
	return promoted
}

// CopySpace clones one agent's knowledge and skills into another —
// a spawned child starts with its parent's specialist stores. An
// identical item already in the target is skipped (dedup by body).
func CopySpace(from, to string) error {
	if !agentRe.MatchString(from) || !agentRe.MatchString(to) {
		return fmt.Errorf("space: bad agent id %q/%q", from, to)
	}
	mu.Lock()
	defer mu.Unlock()

	for _, it := range listKindLocked(from, KindKnowledge) {
		_, body, err := readItem(it.Path)
		if err != nil {
			continue
		}
		dst := filepath.Join(agentKnowledgeDir(to), filepath.Base(it.Path))
		if _, _, err := readItem(dst); err == nil {
			continue // target already holds this file
		}
		cp := it
		cp.Endorsements = append([]string{to}, it.Endorsements...)
		if err := writeItem(dst, cp, body); err != nil {
			return err
		}
	}
	skillsFrom := agentSkillsDir(from)
	skillsTo := agentSkillsDir(to)
	entries, err := os.ReadDir(skillsFrom)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		src := filepath.Join(skillsFrom, e.Name(), "SKILL.md")
		_, body, err := readItem(src)
		if err != nil {
			continue
		}
		it, _, err := readItem(src)
		if err != nil {
			continue
		}
		dst := filepath.Join(skillsTo, e.Name(), "SKILL.md")
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		cp := it
		cp.Endorsements = append([]string{to}, it.Endorsements...)
		if err := writeItem(dst, cp, body); err != nil {
			return err
		}
	}
	return nil
}

// endorseLocked appends endorser to the item's ledger when absent.
// changed=false when the endorsement already existed (idempotent).
func endorseLocked(path, endorser string) (changed bool, err error) {
	it, body, err := readItem(path)
	if err != nil {
		return false, err
	}
	for _, e := range it.Endorsements {
		if e == endorser {
			return false, nil
		}
	}
	it.Endorsements = append(it.Endorsements, endorser)
	return true, writeItem(path, it, body)
}

// promoteLocked copies an item reaching the consensus bar into commons.
// Copies are canonical: endorsed-by keeps every source so a re-share
// from commons stays idempotent. Returns whether a copy was written.
func promoteLocked(path, kind string) (bool, error) {
	it, body, err := readItem(path)
	if err != nil {
		return false, err
	}
	if it.Endorsed() < 2 {
		return false, nil
	}
	var dir, dst string
	if kind == KindSkills {
		dir = filepath.Join(CommonsSkillsDir(), slugify(it.Name))
		dst = filepath.Join(dir, "SKILL.md")
	} else {
		dir = commonsKnowledgeDir()
		dst = filepath.Join(dir, filepath.Base(path))
	}
	if _, curBody, err := readItem(dst); err == nil && bodyHash(body) == bodyHash(curBody) {
		return false, nil // commons already carries this exact fact
	}
	return true, writeItem(dst, it, body)
}

// findLocked locates an item's file by exact name under agent.
func findLocked(agent, kind, name string) string {
	for _, it := range listKindLocked(agent, kind) {
		if it.Name == name {
			return it.Path
		}
	}
	return ""
}

// List returns every item of agent in one store (kind=knowledge or
// skills), name-sorted. Locking wrapper — safe for concurrent callers.
func List(agent, kind string) []Item {
	mu.Lock()
	defer mu.Unlock()
	return listKindLocked(agent, kind)
}

// ListCommons returns the consensus tier of one store.
func ListCommons(kind string) []Item {
	mu.Lock()
	defer mu.Unlock()
	return listDirLocked(dirFor("", kind), kind)
}

// listKindLocked is the unlocked per-agent listing.
func listKindLocked(agent, kind string) []Item {
	return listDirLocked(dirFor(agent, kind), kind)
}

func dirFor(agent, kind string) string {
	if agent == "" {
		if kind == KindSkills {
			return CommonsSkillsDir()
		}
		return commonsKnowledgeDir()
	}
	if kind == KindSkills {
		return agentSkillsDir(agent)
	}
	return agentKnowledgeDir(agent)
}

// listDirLocked reads a store directory into items, name-sorted.
// Skills dirs carry SKILL.md inside; knowledge carries flat .md files.
func listDirLocked(dir, kind string) []Item {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Item
	for _, e := range entries {
		var path string
		if kind == KindSkills {
			if !e.IsDir() {
				continue
			}
			path = filepath.Join(dir, e.Name(), "SKILL.md")
		} else {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			path = filepath.Join(dir, e.Name())
		}
		if it, _, err := readItem(path); err == nil {
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// listAgentsLocked lists agent ids that own a space directory.
func listAgentsLocked() []string {
	entries, err := os.ReadDir(AgentsDir())
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && agentRe.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// Brief renders one agent's space as a short system-note payload —
// what the session injects when routing hands control to a specialist:
// every item, one line each, capped so a large space cannot flood the
// prompt. Empty when the agent has no specialist material.
func Brief(agent string) string {
	mu.Lock()
	items := append(listKindLocked(agent, KindKnowledge), listKindLocked(agent, KindSkills)...)
	mu.Unlock()
	if len(items) == 0 {
		return ""
	}
	const cap = 1200
	var b strings.Builder
	fmt.Fprintf(&b, "You are running as agent %q, a specialist. Your space:\n", agent)
	for _, it := range items {
		line := fmt.Sprintf("- %s: %s\n", it.Name, firstLine(it.Description))
		if len([]rune(b.String()))+len([]rune(line)) > cap {
			b.WriteString("- … (more items; list with the space tool)\n")
			break
		}
		b.WriteString(line)
	}
	return b.String()
}

func firstLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
