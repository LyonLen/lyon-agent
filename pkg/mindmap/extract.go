package mindmap

// §10 图 → memory 沉淀 (M2.5): the deterministic extraction of durable
// facts from a finished map. No LLM, no I/O — ExtractFacts is a pure
// function from (title, nodes) to the specs a route would write, so the
// acceptance table §11.4 (M1–M5) can be pinned without a server.
//
// The rules, and why they are hard-coded rather than model-judged:
//
//   - §10.1 only confirmed decision|fact content is extracted. proposed
//     is the model guessing (入库＝把猜测沉淀成事实), open_question is not
//     a conclusion, parked is refuted understanding, tombstone is a human
//     delete — all four simply never reach a body. Tombstones are READ
//     (ancestor suppression) so nothing under a deleted/parked branch
//     revives into memory either.
//   - §10.2 one first-level branch = one chapter = one fact. 36 tiny
//     facts dilute BM25; one 60-node monster is equally unsearchable.
//     A chapter body over factBodyMaxBytes splits: summary in memory,
//     full text to docs/<topic>/<chapter>.md (Archive), last line links
//     it. Names are 导图-<topic>-<chapter>, stable per map title so a
//     re-extract upserts (M5) instead of forking.
//   - §10.3 scope comes from a keyword table, never free心证: see
//     scopeOf. 图永不 hot (M4): specs carry no Hot and the route never
//     writes the §10.4 index by default.
//
// The route (pkg/server/mindmap_extract.go) owns the only non-pure part:
// writing spec bodies/archives to the workspace.

import (
	"sort"
	"strconv"
	"strings"

	"lyon-agent/pkg/memory"
)

// Extraction constants — the §10 budgets and naming pieces.
const (
	factNamePrefix   = "导图"        // names: 导图-<topic>-<chapter>
	factBodyMaxBytes = 2048        // §10.2: chapter body over this splits to docs/
	factSummaryBytes = 1600        // truncated summary budget (bytes, line boundary)
	slugMaxRunes     = 40          // per slug segment cap (topic / chapter)
	defaultTopic     = "会话"        // map title missing/unslugable
	defaultChapter   = "章节"        // chapter label missing/unslugable
	flatChapter      = "全部"        // the rootless/flat fallback fact's chapter slug
	pendingScopeLine = "scope: 待定" // §10.3 判不出来: first body line, scope=project
)

// FactSpec is one chapter rendered into what would be written for this
// map — the route's exact Save() input minus Hot (always false, M4).
type FactSpec struct {
	Scope       string   // memory scope: team | personal | project
	Name        string   // 导图-<topic>-<chapter>, unique per scope (upsert key)
	Description string   // <chapter label>（<N>条）
	Body        string   // final memory body (summary when Archive is set)
	Tags        []string // always [mindmap, <topic slug>]
	// Archive is non-nil when the full body exceeded factBodyMaxBytes:
	// Body then holds the line-clipped summary + a 明细: <RelPath> line,
	// and Archive holds the full text for the route to write under the
	// workspace (real runs only — a dry run must not touch disk).
	Archive *ArchiveFile
}

// ArchiveFile is the docs/ overflow half of an oversized chapter (§10.2).
type ArchiveFile struct {
	RelPath string // workspace-relative, e.g. docs/<topic>/<chapter>.md
	Body    string // the FULL chapter body
}

// EligibleContent is §10.1's admission test for one node: alive,
// confirmed, kind ∈ {decision, fact}, not parked. Exported so the route's
// "≥4 confirmed content nodes" eligibility count cannot drift from what
// ExtractFacts actually writes. An empty status counts as active (the
// golden sample omits the field).
func EligibleContent(n Node) bool {
	if n.Tombstone || n.State != StateConfirmed || n.Status == StatusParked {
		return false
	}
	return n.Kind == KindDecision || n.Kind == KindFact
}

// TopicSlug is the map-title half of every fact name derived from this
// map — exported so the route's already-extracted prefix check (and the
// web, if it mirrors it) uses the exact same string.
func TopicSlug(title string) string {
	if s := slugify(title); s != "" {
		return s
	}
	return defaultTopic
}

// FactNamePrefix is the §10.5 "本主题未抽过" prefix: any fact in ANY
// scope starting with 导图-<topic>- means this map was extracted before.
func FactNamePrefix(title string) string { return factNamePrefix + "-" + TopicSlug(title) + "-" }

// ExtractFacts turns a map into the chapter facts §10 describes. Pure:
// same input → same output, no clock, no disk.
func ExtractFacts(title string, nodes []Node) []FactSpec {
	byID := make(map[string]*Node, len(nodes))
	for i := range nodes {
		byID[nodes[i].ID] = &nodes[i]
	}

	// Root: the parentless node (conventionally id "root", parent "").
	// A rootless flat map falls back to the seeded convention "root" so
	// orphaned children still chapter themselves; a total fallback to
	// flatChapter handles true forests below.
	rootID := ""
	for i := range nodes {
		if nodes[i].ParentID == "" {
			rootID = nodes[i].ID
			break
		}
	}
	if rootID == "" {
		rootID = "root"
	}

	// Pool: eligible content not suppressed by an ancestor. A parked or
	// tombstoned ancestor poisons its whole subtree (§10.1: 同分支不得复活
	// / 已被推翻的不沉淀) — reading tombstones here is what keeps deleted
	// branches out of memory.
	pool := make([]poolEntry, 0, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		if !EligibleContent(*n) || suppressed(byID, n, rootID) {
			continue
		}
		// level = edges up to (not including) the node's top-level
		// ancestor; chapter nodes are top children themselves (level 0).
		level, top := levelAndTop(byID, n, rootID)
		pool = append(pool, poolEntry{node: n, level: level, top: top})
	}
	if len(pool) == 0 {
		return nil
	}

	// Chapters = first-level branches (kind=branch, alive, not parked —
	// parked/tombstoned branches never reach here because their content
	// is suppressed). Non-branch top children and anything off the root
	// subtree land in leftovers → one flat fact, which is also exactly
	// the §10.2 "flat map" fallback when no chapter matched.
	chapters := map[string]*Node{}
	for i := range nodes {
		n := &nodes[i]
		if n.ParentID == rootID && n.ID != rootID && n.Kind == KindBranch && !n.Tombstone && n.Status != StatusParked {
			chapters[n.ID] = n
		}
	}

	memberOf := map[string][]poolEntry{} // chapter id → members, depth order
	var leftovers []poolEntry
	for _, e := range pool {
		if ch, ok := chapters[e.top.ID]; ok {
			memberOf[ch.ID] = append(memberOf[ch.ID], e)
		} else {
			leftovers = append(leftovers, e)
		}
	}

	topic := TopicSlug(title)
	var out []FactSpec

	// Deterministic chapter order: document order of the top branches.
	var chIDs []string
	for id := range chapters {
		chIDs = append(chIDs, id)
	}
	sort.Slice(chIDs, func(a, b int) bool {
		return indexOfNode(nodes, chIDs[a]) < indexOfNode(nodes, chIDs[b])
	})
	for _, id := range chIDs {
		members := memberOf[id]
		if len(members) == 0 {
			continue // §10.2: a branch with no confirmed content is not a chapter
		}
		out = append(out, buildChapterSpecs(title, chapters[id].Label, topic, members)...)
	}
	if len(leftovers) > 0 {
		// Rootless/flat map (or content outside branch chapters): one
		// fact for all of it — the §10.2 fallback, uniformly applied.
		out = append(out, buildChapterSpecs(title, flatChapter, topic, leftovers)...)
	}
	return out
}

// poolEntry is one eligible node plus its position in the tree. pool is
// built in document order, so a stable level sort keeps it as the
// within-level order.
type poolEntry struct {
	node  *Node
	level int // edges to its top-level ancestor (top child = 0)
	top   *Node
}

// suppressed reports whether any ANCESTOR of n is parked or tombstoned.
// n's own parked/tombstone state is EligibleContent's job; this is the
// branch-level veto (§10.1 rows 3–4).
func suppressed(byID map[string]*Node, n *Node, rootID string) bool {
	seen := map[string]bool{}
	cur := byID[n.ParentID]
	steps := 0
	for cur != nil && !seen[cur.ID] && steps <= maxDepth+maxNodes {
		seen[cur.ID] = true
		if cur.Tombstone || cur.Status == StatusParked {
			return true
		}
		if cur.ID == rootID || cur.ParentID == "" {
			break
		}
		cur = byID[cur.ParentID]
		steps++
	}
	return false
}

// levelAndTop walks n up to the child-of-root it hangs under: level =
// edge count (top child itself → level 0), top = that ancestor (n itself
// when n is top-level; n when orphaned). Cycle-guarded like depthOf.
func levelAndTop(byID map[string]*Node, n *Node, rootID string) (int, *Node) {
	level := 0
	seen := map[string]bool{}
	cur := n
	for cur.ParentID != "" && cur.ParentID != rootID {
		if seen[cur.ID] {
			break
		}
		seen[cur.ID] = true
		p, ok := byID[cur.ParentID]
		if !ok {
			break
		}
		cur = p
		level++
		if level > maxDepth+maxNodes {
			break
		}
	}
	return level, cur
}

func indexOfNode(nodes []Node, id string) int {
	for i := range nodes {
		if nodes[i].ID == id {
			return i
		}
	}
	return 1 << 30
}

// buildChapterSpecs renders one chapter: the primary fact plus at most
// one §10.2 章内二级切分 extra fact for a second scope.
func buildChapterSpecs(title, chapterLabel, topic string, members []poolEntry) []FactSpec {
	// Depth order, stable within a level (document order).
	sort.SliceStable(members, func(i, j int) bool { return members[i].level < members[j].level })

	// §10.3 primary scope: scan title + member labels as one text, team →
	// personal → project first hit wins. team leads because a 社交/组织
	// signal (公司|团队|流程|会议…) is the most specific claim in the
	// table; personal beats project because a stated human preference is
	// what most must not be flattened into an anonymous project fact.
	scan := strings.Builder{}
	scan.WriteString(title)
	scan.WriteByte('\n')
	for _, m := range members {
		scan.WriteString(m.node.Label)
		scan.WriteByte('\n')
	}
	primary := scopeSignal(scan.String())

	// 章内二级切分: a second scope with ≥2 signal-matched nodes — plus a
	// third only if two already qualify, per "两个不同 scope 各 ≥2" — the
	// first by fixed priority that is not the primary scope splits off
	// ONE extra fact (≤1 per chapter). Members partition: the extra
	// scope's nodes leave the primary body, nothing is duplicated.
	counts := map[string]int{}
	for _, m := range members {
		if s := scopeSignal(m.node.Label); s != "" {
			counts[s]++
		}
	}
	var candidates []string // priority order: team > personal > project
	for _, s := range []string{memory.ScopeTeam, memory.ScopePersonal, memory.ScopeProject} {
		if counts[s] >= 2 {
			candidates = append(candidates, s)
		}
	}
	extra := ""
	if len(candidates) >= 2 {
		for _, s := range candidates {
			if s != primary {
				extra = s
				break
			}
		}
	}

	var kept, moved []poolEntry
	for _, m := range members {
		if extra != "" && scopeSignal(m.node.Label) == extra {
			moved = append(moved, m)
		} else {
			kept = append(kept, m)
		}
	}

	chapterSlug := slugify(chapterLabel)
	if chapterSlug == "" {
		chapterSlug = defaultChapter
	}
	desc := chapterLabel + "（" + strconv.Itoa(len(kept)) + "条）"
	if chapterLabel == "" {
		desc = strconv.Itoa(len(kept)) + "条"
	}

	out := []FactSpec{makeSpec(topic, chapterSlug, "", primary, desc, kept)}
	if extra != "" && len(moved) > 0 {
		edesc := chapterLabel + "-" + extra + "（" + strconv.Itoa(len(moved)) + "条）"
		out = append(out, makeSpec(topic, chapterSlug, "-"+extra, extra, edesc, moved))
	}
	return out
}

// makeSpec renders one fact: name, body bullets, scope 待定 marker, and
// the §10.2 >2KB split into archive + summary.
func makeSpec(topic, chapterSlug, nameSuffix, scope, desc string, members []poolEntry) FactSpec {
	spec := FactSpec{
		Scope:       scope,
		Name:        factNamePrefix + "-" + topic + "-" + chapterSlug + nameSuffix,
		Description: desc,
		Tags:        []string{"mindmap", topic},
	}
	// §10.3 判不出来 → most conservative scope (project, 不污染他人) with
	// the body's FIRST line flagging the doubt verbatim.
	var b strings.Builder
	if scope == "" {
		scope = memory.ScopeProject
		spec.Scope = scope
		b.WriteString(pendingScopeLine)
		b.WriteByte('\n')
	}
	for _, m := range members {
		if m.level > 0 {
			b.WriteString(strings.Repeat("  ", m.level-1))
		}
		b.WriteString("- ")
		b.WriteString(m.node.Label)
		if m.node.SourceTurn > 0 {
			b.WriteString(" t")
			b.WriteString(strconv.Itoa(m.node.SourceTurn))
		}
		b.WriteByte('\n')
	}
	body := strings.TrimRight(b.String(), "\n")
	if len(body) > factBodyMaxBytes {
		rel := "docs/" + topic + "/" + chapterSlug + nameSuffix + ".md"
		summary := clipLines(body, factSummaryBytes)
		spec.Body = summary + "\n明细: " + rel
		spec.Archive = &ArchiveFile{RelPath: rel, Body: body + "\n"}
	} else {
		spec.Body = body
	}
	return spec
}

// scopeSignal applies the §10.3 判定表 to one text: team → personal →
// project, first hit wins; "" = no signal. Patterns are literal keyword
// sets — deterministic by construction, no model in the loop.
func scopeSignal(text string) string {
	// team: 跨项目的组织口径 — the strongest (most specific) signal.
	teamWords := []string{"公司", "团队", "组织", "部门", "跨项目", "会议", "流程"}
	// personal: 用户本人的口味/要求 — second priority (see ExtractFacts).
	personalWords := []string{"我的", "我要", "我觉得", "我偏好", "我"}
	// project: 本仓库事实 — file paths, package-ish segments, code files,
	// or function-name shape (camelCase / snake_with_verb).
	for _, w := range teamWords {
		if strings.Contains(text, w) {
			return memory.ScopeTeam
		}
	}
	for _, w := range personalWords {
		if strings.Contains(text, w) {
			return memory.ScopePersonal
		}
	}
	if projectSignal(text) {
		return memory.ScopeProject
	}
	return ""
}

func projectSignal(text string) bool {
	for _, w := range []string{"pkg/", "web/", "cmd/", "internal/", ".go", ".ts", ".js", ".proto"} {
		if strings.Contains(text, w) {
			return true
		}
	}
	if strings.Contains(text, "/") {
		return true
	}
	// function-name signals: snake_case with a verb-ish tail, or a
	// camelCase run — cheap and good enough for label phrases (no
	// content clip anymore, so slugs only need a stable prefix).
	if hasToken(text, '_') || hasCamel(text) {
		return true
	}
	return false
}

func hasToken(s string, sep byte) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == sep && i > 0 && i+1 < len(s) {
			return true
		}
	}
	return false
}

// hasCamel reports an ascii lower→upper transition (runHandler, newSession).
func hasCamel(s string) bool {
	for i := 1; i < len(s); i++ {
		a, b := s[i-1], s[i]
		if a >= 'a' && a <= 'z' && b >= 'A' && b <= 'Z' {
			return true
		}
	}
	return false
}

// slugify lowercases and maps every non-letter/digit to a dash (CJK kept —
// memory names/filenames accept it), collapsing runs and trimming, capped
// at slugMaxRunes per the §10.2 naming rule. "" = nothing usable.
func slugify(s string) string {
	s = memory.Slug(s)
	if r := []rune(s); len(r) > slugMaxRunes {
		s = strings.TrimRight(string(r[:slugMaxRunes]), "-")
	}
	return s
}

// clipLines keeps whole lines up to max bytes (truncation at a bullet
// boundary — every bullet is its own line). The first line alone always
// fits any sane budget; a pathological single line is hard-cut rather
// than dropped.
func clipLines(s string, max int) string {
	if len(s) <= max {
		return s
	}
	var out strings.Builder
	used := 0
	for _, line := range strings.Split(s, "\n") {
		if used+len(line)+1 > max {
			break
		}
		if out.Len() > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(line)
		used += len(line) + 1
	}
	if out.Len() == 0 {
		if r := []rune(s); len(r) > max {
			return string(r[:max])
		}
		return s
	}
	return out.String()
}
