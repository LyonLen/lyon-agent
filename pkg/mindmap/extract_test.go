package mindmap

// M1–M5 acceptance tests for §10 extraction (docs/design/mindmap.md §11.4),
// plus the §10.2 oversize-split and flat-fallback rules the table assumes.
// Fixtures are built as raw []Node — ExtractFacts is pure, so no manager,
// no disk, no server. M4/M5 are the two cases that must touch a real
// memory store; they run against t.TempDir() workspaces so the package
// cache (memory.Open keys by workspace) never bleeds into the real one.

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"lyon-agent/pkg/memory"
)

// xnode builds a wire-shaped node (status active, turn 1) — the shape the
// route unmarshals out of Snapshot's nodes_json.
func xnode(id, parent, label, kind, state string) Node {
	return Node{
		ID: id, ParentID: parent, Label: label,
		Kind: kind, State: state, Status: StatusActive, SourceTurn: 1,
	}
}

// saveSpecs writes specs exactly as the route does (Hot never set — M4).
func saveSpecs(t *testing.T, st *memory.Store, specs []FactSpec) {
	t.Helper()
	for _, sp := range specs {
		if _, err := st.Save(memory.Fact{
			Name: sp.Name, Description: sp.Description, Scope: sp.Scope,
			Tags: sp.Tags, Body: sp.Body,
		}); err != nil {
			t.Fatalf("Save(%q): %v", sp.Name, err)
		}
	}
}

// freshMem builds a memory store over isolated dirs: LYON_HOME (team and
// personal layers) via scratchHome, project layer via its own temp
// workspace. The unique workspace key makes memory.Open's process-wide
// cache hand back a store nothing else has touched.
func freshMem(t *testing.T) *memory.Store {
	t.Helper()
	scratchHome(t)
	return memory.Open(t.TempDir())
}

func allBodies(specs []FactSpec) string {
	var b strings.Builder
	for _, sp := range specs {
		b.WriteString(sp.Name)
		b.WriteByte('\n')
		b.WriteString(sp.Body)
		b.WriteByte('\n')
	}
	return b.String()
}

// ── M1: only confirmed decision|fact content is ever written ───────
// 6 confirmed + 4 proposed/open/parked/tombstoned (+ one confirmed fact
// under a parked branch, the §10.1 同分支不得复活 row). Only the six
// confirmed labels may appear anywhere in name or body.

func TestM1OnlyConfirmedContentExtracted(t *testing.T) {
	kept := []string{"确认事实一", "确认决策二", "确认事实三", "确认决策四", "确认事实五", "确认决策六"}
	banned := []string{"模型猜的决策", "模型猜的事实", "待解的悬念", "停用的事实", "墓碑事实", "推翻分支里的事实"}

	parkedFact := xnode("pk1", "ch", banned[3], KindFact, StateConfirmed)
	parkedFact.Status = StatusParked
	tombFact := xnode("ts1", "ch", banned[4], KindFact, StateConfirmed)
	tombFact.Tombstone = true

	nodes := []Node{
		xnode("root", "", "样例导图", KindBranch, StateConfirmed),
		xnode("ch", "root", "需求", KindBranch, StateConfirmed),
		// 6 confirmed content nodes (§11.4 M1 input).
		xnode("c1", "ch", kept[0], KindFact, StateConfirmed),
		xnode("c2", "ch", kept[1], KindDecision, StateConfirmed),
		xnode("c3", "ch", kept[2], KindFact, StateConfirmed),
		xnode("c4", "ch", kept[3], KindDecision, StateConfirmed),
		xnode("c5", "ch", kept[4], KindFact, StateConfirmed),
		xnode("c6", "ch", kept[5], KindDecision, StateConfirmed),
		// 4 non-qualifying nodes: proposed pair, open_question, parked.
		xnode("p1", "ch", banned[0], KindDecision, StateProposed),
		xnode("p2", "ch", banned[1], KindFact, StateProposed),
		xnode("q1", "ch", banned[2], KindOpenQuestion, StateConfirmed),
		parkedFact,
		tombFact,
		// A parked branch: even its CONFIRMED child must not be extracted.
		func() Node {
			br := xnode("deadbr", "root", "已推翻的分支", KindBranch, StateProposed)
			br.Status = StatusParked
			return br
		}(),
		xnode("deadkid", "deadbr", banned[5], KindFact, StateConfirmed),
	}

	specs := ExtractFacts("样例导图", nodes)
	if len(specs) != 1 {
		t.Fatalf("facts = %d, want 1 (one chapter with confirmed content)", len(specs))
	}
	text := allBodies(specs)
	for _, label := range kept {
		if !strings.Contains(text, label) {
			t.Errorf("confirmed label %q missing from extraction:\n%s", label, text)
		}
	}
	for _, label := range banned {
		if strings.Contains(text, label) {
			t.Errorf("banned label %q leaked into name/body:\n%s", label, text)
		}
	}
	// The store sees exactly the same story (M1's 判据 is about the store).
	st := freshMem(t)
	saveSpecs(t, st, specs)
	if got := len(st.List("")); got != 1 {
		t.Errorf("store facts = %d, want 1", got)
	}
	for _, f := range st.List("") {
		for _, label := range banned {
			if strings.Contains(f.Body, label) || strings.Contains(f.Name, label) {
				t.Errorf("banned label %q reached the store (%s)", label, f.Name)
			}
		}
	}
}

// ── M2: the 8-branch golden sample → ≤8 facts, 导图-<topic>-<chapter> ──
// Transcribed from docs/design/mindmap-sample.js (wire keys). Four
// chapters hold confirmed content (需求 4 / 机制选型 2 / 已拍板 1 /
// 下一步 1); 三条硬规则 / 风险 / 亮点补充 / 未决 are all proposed or
// open_question → no facts. Names must match 导图-…-….

func TestM2SampleEightBranches(t *testing.T) {
	title := "对话实时思维导图"
	nodes := []Node{
		xnode("root", "", "对话实时思维导图（mindmap）", KindBranch, StateConfirmed),
		xnode("req", "root", "需求", KindBranch, StateConfirmed),
		xnode("req-live", "req", "随对话实时长出来的导图卡片", KindFact, StateConfirmed),
		xnode("req-card", "req", "住在对话流里，形制比照 plan 卡", KindFact, StateConfirmed),
		xnode("req-edit", "req", "图上的调整回流 chat，并自动提问", KindFact, StateConfirmed),
		xnode("req-new", "req", "全新方向，不复用 BACKLOG 候选", KindFact, StateConfirmed),
		xnode("mech", "root", "机制选型", KindBranch, StateProposed),
		xnode("mech-plan", "mech", "plan 卡＝todo_write 全量重写＋无状态渲染", KindFact, StateConfirmed),
		xnode("mech-no", "mech", "不照抄：token 平方增长＋模型没动机重画", KindDecision, StateProposed),
		xnode("mech-side", "mech", "旁路蒸馏，挂 prompt_end tap", KindDecision, StateProposed),
		xnode("mech-proto", "mech", "新事件走 attrs 前进兼容位", KindFact, StateConfirmed),
		xnode("rule", "root", "三条硬规则", KindBranch, StateProposed),
		xnode("rule-pin", "rule", "人工改动是权威，模型不得覆盖", KindDecision, StateProposed),
		xnode("rule-auth", "rule", "图上操作 ≠ 执行授权，不绕审批", KindDecision, StateProposed),
		xnode("settled", "root", "已拍板", KindBranch, StateConfirmed),
		xnode("settled-prop", "settled", "图是「待你确认的理解」，不是结论", KindDecision, StateConfirmed),
		xnode("settled-vis", "settled", "提案态虚线 / 确认态实线", KindDecision, StateProposed),
		xnode("risk", "root", "风险", KindBranch, StateProposed),
		xnode("risk-second", "risk", "二手阅读长得像一手结论", KindFact, StateProposed),
		xnode("risk-never", "risk", "永远没有完成态 → 会没人看", KindOpenQuestion, StateProposed),
		xnode("plus", "root", "亮点补充", KindBranch, StateProposed),
		xnode("plus-handoff", "plus", "图当会话交接物：存 space，下段注入", KindDecision, StateProposed),
		xnode("open", "root", "未决", KindBranch, StateProposed),
		xnode("open-model", "open", "蒸馏用哪个模型", KindOpenQuestion, StateProposed),
		xnode("next", "root", "下一步", KindBranch, StateProposed),
		xnode("next-validate", "next", "先用真实会话验价值（本图）", KindDecision, StateConfirmed),
		xnode("next-m1", "next", "M1 见证版只读卡片", KindDecision, StateProposed),
		xnode("next-m2", "next", "M2 人工通道（核心）", KindDecision, StateProposed),
	}

	specs := ExtractFacts(title, nodes)
	if len(specs) == 0 {
		t.Fatal("no facts extracted from the golden sample")
	}
	if len(specs) > 8 {
		t.Fatalf("facts = %d, want ≤8 (8 branches → ≤8, §11.4 M2)", len(specs))
	}
	// Exactly the four chapters holding confirmed content, named 导图-…-….
	want := map[string]bool{
		FactNamePrefix(title) + "需求":   false,
		FactNamePrefix(title) + "机制选型": false,
		FactNamePrefix(title) + "已拍板":  false,
		FactNamePrefix(title) + "下一步":  false,
	}
	for _, sp := range specs {
		if _, ok := want[sp.Name]; !ok {
			t.Errorf("unexpected fact name %q", sp.Name)
			continue
		}
		want[sp.Name] = true
		if !strings.HasPrefix(sp.Name, "导图-") || !strings.HasPrefix(sp.Name, FactNamePrefix(title)) {
			t.Errorf("name %q does not match 导图-…-…", sp.Name)
		}
		if len(sp.Tags) != 2 || sp.Tags[0] != "mindmap" || sp.Tags[1] != TopicSlug(title) {
			t.Errorf("%q tags = %v, want [mindmap %s]", sp.Name, sp.Tags, TopicSlug(title))
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("chapter fact %q missing", name)
		}
	}
	// The all-proposed chapters (三条硬规则, 风险 …) produce nothing at all.
	text := allBodies(specs)
	for _, label := range []string{"人工改动是权威，模型不得覆盖", "二手阅读长得像一手结论", "蒸馏用哪个模型"} {
		if strings.Contains(text, label) {
			t.Errorf("proposed-only chapter content %q extracted", label)
		}
	}
}

// ── M3: three fixture groups → personal / project / team ───────────

func TestM3ScopeTable(t *testing.T) {
	title := "样例导图"
	nodes := []Node{
		xnode("root", "", title, KindBranch, StateConfirmed),
		// 口味偏好 → personal (我 signal).
		xnode("taste", "root", "口味偏好", KindBranch, StateConfirmed),
		xnode("t1", "taste", "我偏好手冲咖啡，不加糖", KindFact, StateConfirmed),
		xnode("t2", "taste", "我要 PPT 用深色底", KindFact, StateConfirmed),
		// 仓库/架构 → project (path / package signal).
		xnode("repo", "root", "仓库架构", KindBranch, StateConfirmed),
		xnode("r1", "repo", "pkg/mindmap 蒸馏器挂 prompt_end tap", KindFact, StateConfirmed),
		xnode("r2", "repo", "web/src 卡片渲染在 MindMapCard", KindFact, StateConfirmed),
		// 组织口径/会议 → team.
		xnode("org", "root", "组织口径", KindBranch, StateConfirmed),
		xnode("o1", "org", "会议转写的 speaker 标签不可信", KindFact, StateConfirmed),
		xnode("o2", "org", "组织口径以季度复盘会为准", KindFact, StateConfirmed),
	}

	specs := ExtractFacts(title, nodes)
	if len(specs) != 3 {
		t.Fatalf("facts = %d, want 3 (one per chapter, no split)", len(specs))
	}
	wantScope := map[string]string{
		FactNamePrefix(title) + "口味偏好": memory.ScopePersonal,
		FactNamePrefix(title) + "仓库架构": memory.ScopeProject,
		FactNamePrefix(title) + "组织口径": memory.ScopeTeam,
	}
	for _, sp := range specs {
		want, ok := wantScope[sp.Name]
		if !ok {
			t.Errorf("unexpected fact %q", sp.Name)
			continue
		}
		if sp.Scope != want {
			t.Errorf("%q scope = %q, want %q", sp.Name, sp.Scope, want)
		}
		if strings.HasPrefix(sp.Body, pendingScopeLine) {
			t.Errorf("%q has a real signal but body starts %q", sp.Name, pendingScopeLine)
		}
	}
}

// ── §10.3 判不出来: no signal anywhere → project + scope: 待定 first line

func TestExtractPendingScopeFirstLine(t *testing.T) {
	nodes := []Node{
		xnode("root", "", "无信号主题", KindBranch, StateConfirmed),
		xnode("ch", "root", "某章", KindBranch, StateConfirmed),
		xnode("c1", "ch", "中性的事实陈述", KindFact, StateConfirmed),
	}
	specs := ExtractFacts("无信号主题", nodes)
	if len(specs) != 1 {
		t.Fatalf("facts = %d, want 1", len(specs))
	}
	if specs[0].Scope != memory.ScopeProject {
		t.Errorf("scope = %q, want project (最保守)", specs[0].Scope)
	}
	first, _, _ := strings.Cut(specs[0].Body, "\n")
	if first != pendingScopeLine {
		t.Errorf("first body line = %q, want exactly %q", first, pendingScopeLine)
	}
}

// ── §10.2 章内二级切分: two scopes ≥2 in one chapter → +1 extra fact ──

func TestExtractChapterSplitsSecondScope(t *testing.T) {
	title := "需求讨论"
	nodes := []Node{
		xnode("root", "", title, KindBranch, StateConfirmed),
		xnode("ch", "root", "需求", KindBranch, StateConfirmed),
		// personal ≥2
		xnode("p1", "ch", "我偏好圆角卡片", KindFact, StateConfirmed),
		xnode("p2", "ch", "我要默认深色", KindFact, StateConfirmed),
		// project ≥2
		xnode("j1", "ch", "pkg/server 路由注册在 New()", KindFact, StateConfirmed),
		xnode("j2", "ch", "web/src 用 tiling 布局", KindFact, StateConfirmed),
	}
	specs := ExtractFacts(title, nodes)
	if len(specs) != 2 {
		t.Fatalf("facts = %d, want 2 (base + one §10.2 split)", len(specs))
	}
	prefix := FactNamePrefix(title)
	base, extra := specs[0], specs[1]
	if base.Name != prefix+"需求" {
		t.Errorf("base name = %q, want %q", base.Name, prefix+"需求")
	}
	// primary = personal (first-hit: personal beats project); the split
	// scope is project — the second by priority, ≤1 extra per chapter.
	if base.Scope != memory.ScopePersonal {
		t.Errorf("base scope = %q, want personal", base.Scope)
	}
	if extra.Name != prefix+"需求-project" || extra.Scope != memory.ScopeProject {
		t.Errorf("extra = %q/%q, want %q/project", extra.Name, extra.Scope, prefix+"需求-project")
	}
	// The members partition: each label appears in exactly one body.
	if strings.Contains(base.Body, "pkg/server") || !strings.Contains(extra.Body, "pkg/server") {
		t.Errorf("split content wrong:\nbase: %s\nextra: %s", base.Body, extra.Body)
	}
	if strings.Contains(extra.Body, "我偏好") || !strings.Contains(base.Body, "我偏好") {
		t.Errorf("split content wrong:\nbase: %s\nextra: %s", base.Body, extra.Body)
	}
}

// ── §10.2 oversize: body >2048 bytes → summary + docs/ archive ─────

func TestExtractSplitsOversizedChapter(t *testing.T) {
	title := "超长主题"
	nodes := []Node{xnode("root", "", title, KindBranch, StateConfirmed),
		xnode("ch", "root", "长章", KindBranch, StateConfirmed)}
	long := strings.Repeat("数", 60) // ~180 bytes per bullet with indent/turn
	for i := 0; i < 15; i++ {
		nodes = append(nodes, xnode(
			"c"+strconv.Itoa(i), "ch", long+strconv.Itoa(i),
			KindFact, StateConfirmed))
	}

	specs := ExtractFacts(title, nodes)
	if len(specs) != 1 {
		t.Fatalf("facts = %d, want 1 (no scope signals → no split)", len(specs))
	}
	sp := specs[0]
	if sp.Archive == nil {
		t.Fatal("oversized chapter must carry an Archive")
	}
	if len(sp.Archive.Body) <= factBodyMaxBytes {
		t.Fatalf("full body = %d bytes, want > %d — fixture too small", len(sp.Archive.Body), factBodyMaxBytes)
	}
	wantRel := "docs/" + TopicSlug(title) + "/长章.md"
	if sp.Archive.RelPath != wantRel {
		t.Errorf("archive rel = %q, want %q", sp.Archive.RelPath, wantRel)
	}
	if !strings.HasSuffix(sp.Archive.Body, "\n") {
		t.Error("archive body must end with a newline (file text)")
	}
	// Archive holds the FULL body; the memory body is a line-clipped
	// summary + the trailing 明细 link (§10.2).
	if n := strings.Count(sp.Archive.Body, "- "+long); n != 15 {
		t.Errorf("archive bullets = %d, want 15 (full body)", n)
	}
	lines := strings.Split(sp.Body, "\n")
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "明细: "+wantRel) {
		t.Errorf("last body line = %q, want 明细: %s", last, wantRel)
	}
	if len(sp.Body) > factSummaryBytes+len(last)+1 {
		t.Errorf("summary body = %d bytes, budget ~%d + link line", len(sp.Body), factSummaryBytes)
	}
	// Neutral labels → no signal → the §10.3 待定 marker survives the
	// split as the FIRST line.
	if lines[0] != pendingScopeLine {
		t.Errorf("first summary line = %q, want %q", lines[0], pendingScopeLine)
	}
	// Truncation at a line boundary: early bullets kept, the tail gone.
	if !strings.Contains(sp.Body, long+"0") {
		t.Error("summary must keep early bullets")
	}
	if strings.Contains(sp.Body, long+"14") {
		t.Error("summary must truncate before bullet 15")
	}
	for _, l := range lines[:len(lines)-1] {
		if strings.HasPrefix(strings.TrimLeft(l, " "), "- ") && !strings.Contains(l, " t") {
			t.Errorf("bullet line lost its turn ref (partial?): %q", l)
		}
	}
}

// ── §10.2 flat fallback: no branch chapters → one fact for everything ──

func TestExtractFlatFallbackSingleFact(t *testing.T) {
	title := "扁平图"
	nodes := []Node{
		xnode("root", "", title, KindBranch, StateConfirmed),
		// content directly under the root: no kind=branch chapter exists.
		xnode("d1", "root", "直接挂根的决策", KindDecision, StateConfirmed),
		xnode("d2", "root", "直接挂根的事实", KindFact, StateConfirmed),
	}
	specs := ExtractFacts(title, nodes)
	if len(specs) != 1 {
		t.Fatalf("facts = %d, want 1 (flat fallback)", len(specs))
	}
	if !strings.HasSuffix(specs[0].Name, "-"+flatChapter) {
		t.Errorf("name = %q, want suffix -%s", specs[0].Name, flatChapter)
	}
	if !strings.Contains(specs[0].Body, "直接挂根的决策") || !strings.Contains(specs[0].Body, "直接挂根的事实") {
		t.Errorf("fallback body lost content:\n%s", specs[0].Body)
	}
}

// ── M4: extraction never makes anything hot ────────────────────────
// HotText(4096) length must not grow: all new facts are cold, no §10.4
// index entry is written (default off).

func TestM4HotTierNeverGrows(t *testing.T) {
	nodes := []Node{
		xnode("root", "", "热层主题", KindBranch, StateConfirmed),
		xnode("ch", "root", "章", KindBranch, StateConfirmed),
		xnode("c1", "ch", "冷事实一", KindFact, StateConfirmed),
		xnode("c2", "ch", "冷事实二", KindDecision, StateConfirmed),
		xnode("c3", "ch", "冷事实三", KindFact, StateConfirmed),
	}
	specs := ExtractFacts("热层主题", nodes)
	if len(specs) == 0 {
		t.Fatal("fixture must extract")
	}

	st := freshMem(t)
	// One seed hot fact so HotText is non-empty and any leak would show.
	if _, err := st.Save(memory.Fact{
		Name: "hot-seed", Scope: memory.ScopeProject, Hot: true,
		Description: "种子", Body: "始终热的种子事实",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	before := st.HotText(4096)
	if before == "" {
		t.Fatal("seed hot fact must render")
	}

	saveSpecs(t, st, specs)

	after := st.HotText(4096)
	if len(after) != len(before) {
		t.Errorf("HotText(4096) length %d → %d, must not grow (§10.4 图永不 hot)",
			len(before), len(after))
	}
	if after != before {
		t.Errorf("hot tier changed:\nbefore: %s\nafter:  %s", before, after)
	}
	for _, sp := range specs {
		f, ok := st.Get(sp.Scope, sp.Name)
		if !ok {
			t.Fatalf("fact %q missing from store", sp.Name)
		}
		if f.Hot {
			t.Errorf("extracted fact %q landed hot", f.Name)
		}
		if len(f.Tags) != 2 || f.Tags[0] != "mindmap" {
			t.Errorf("tags of %q = %v", f.Name, f.Tags)
		}
	}
}

// ── M5: extract twice → upsert, count unchanged, Updated advances ──
// Fact names are stable (导图-<topic>-<chapter>), so the second run is an
// update, not a fork. The frontmatter stores RFC3339 seconds, hence the
// >1s pause between runs — without it Updated would compare equal by
// construction and the assertion would be a coin flip.

func TestM5ReextractUpserts(t *testing.T) {
	title := "幂等主题"
	nodes := []Node{
		xnode("root", "", title, KindBranch, StateConfirmed),
		xnode("ch", "root", "章甲", KindBranch, StateConfirmed),
		xnode("c1", "ch", "事实一", KindFact, StateConfirmed),
		xnode("c2", "ch", "事实二", KindDecision, StateConfirmed),
	}
	specs := ExtractFacts(title, nodes)
	if len(specs) != 1 {
		t.Fatalf("facts = %d, want 1", len(specs))
	}

	st := freshMem(t)
	saveSpecs(t, st, specs)
	first := st.List("")
	if len(first) != 1 {
		t.Fatalf("after run 1: facts = %d, want 1", len(first))
	}
	created, updated := first[0].Created, first[0].Updated

	time.Sleep(1100 * time.Millisecond) // RFC3339 frontmatter granularity

	saveSpecs(t, st, ExtractFacts(title, nodes)) // run 2: same input
	second := st.List("")
	if len(second) != len(first) {
		t.Fatalf("after run 2: facts = %d, want %d (upsert, not fork)", len(second), len(first))
	}
	f := second[0]
	if !f.Updated.After(updated) {
		t.Errorf("updated = %v, want after %v (must advance)", f.Updated, updated)
	}
	if !f.Created.Equal(created) {
		t.Errorf("created = %v, want preserved %v", f.Created, created)
	}
	// Exactly one file on disk per fact — the name is the upsert key.
	stats := st.StoreStats()
	if got := stats.Team + stats.Personal + stats.Project; got != 1 {
		t.Errorf("store facts total = %d, want 1", got)
	}
}
