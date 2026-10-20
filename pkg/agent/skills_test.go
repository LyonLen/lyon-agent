package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// writeSkill lays down <root>/<name>/SKILL.md with the given content.
func writeSkill(t *testing.T, root, name, content string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestScanSkillsParsesAndSkips: normal frontmatter (quoted description,
// nested keys that must not shadow the top-level ones), no-frontmatter
// fallback, unterminated frontmatter skipped, dirs without SKILL.md and
// loose files ignored — sorted by name.
func TestScanSkillsParsesAndSkips(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "alpha",
		"---\nname: alpha\ndescription: \"Does alpha things.\"\nmetadata:\n  version: 9\ntags:\n  - name: nested-decoy\n---\n\n# Alpha\n\nAlpha body.\n")
	writeSkill(t, dir, "beta", "# Beta Tool\n\nBeta does beta work.\n")     // no frontmatter
	writeSkill(t, dir, "gamma", "---\nname: gamma\n")                       // opened, never closed — bad file
	if err := os.MkdirAll(filepath.Join(dir, "delta"), 0o755); err != nil { // dir without SKILL.md
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "loose.md"), []byte("x"), 0o644); err != nil { // not a skill dir
		t.Fatal(err)
	}

	got := ScanSkills(dir)
	if len(got) != 2 {
		t.Fatalf("ScanSkills = %d entries, want 2: %+v", len(got), got)
	}
	if got[0].Name != "alpha" || got[1].Name != "beta" {
		t.Fatalf("names = %q, %q; want alpha, beta (sorted)", got[0].Name, got[1].Name)
	}
	if got[0].Description != "Does alpha things." {
		t.Fatalf("alpha description = %q (quotes must be stripped, nested keys ignored)", got[0].Description)
	}
	if want := "Beta Tool — Beta does beta work."; got[1].Description != want {
		t.Fatalf("beta description = %q, want %q", got[1].Description, want)
	}
	if !strings.HasSuffix(got[0].Path, filepath.Join("alpha", "SKILL.md")) {
		t.Fatalf("alpha path = %q, want .../alpha/SKILL.md", got[0].Path)
	}
}

// TestScanSkillsNameFallback: frontmatter without a name falls back to
// the directory name.
func TestScanSkillsNameFallback(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "only-desc", "---\ndescription: Has a description but no name.\n---\n")
	got := ScanSkills(dir)
	if len(got) != 1 || got[0].Name != "only-desc" {
		t.Fatalf("fallback name = %+v, want dir name only-desc", got)
	}
}

// TestScanSkillsLongDescriptionClipped: one entry's description is cut to
// the per-entry budget (plus clipRunes' suffix).
func TestScanSkillsLongDescriptionClipped(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "long", "---\nname: long\ndescription: "+strings.Repeat("x", 500)+"\n---\n")
	got := ScanSkills(dir)
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	if r := utf8.RuneCountInString(got[0].Description); r > skillsDescBudget+16 {
		t.Fatalf("description not clipped: %d runes > %d", r, skillsDescBudget+16)
	}
}

// TestScanSkillsMissingDir: an unreadable/missing directory degrades to an
// empty list — the scan must never fail the caller.
func TestScanSkillsMissingDir(t *testing.T) {
	if got := ScanSkills(filepath.Join(t.TempDir(), "nope")); len(got) != 0 {
		t.Fatalf("missing dir must yield empty list, got %+v", got)
	}
}

// TestScanSkillsEnvRedirect: LYON_SKILLS_DIR overrides the default
// ~/.claude/skills for both the scan and the rendered section.
func TestScanSkillsEnvRedirect(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "from-env", "---\nname: from-env\ndescription: Found via LYON_SKILLS_DIR.\n---\n")
	t.Setenv("LYON_SKILLS_DIR", dir)
	got := ScanSkills("")
	if len(got) != 1 || got[0].Name != "from-env" {
		t.Fatalf("LYON_SKILLS_DIR redirect failed: %+v", got)
	}
	sec := skillsSection()
	if !strings.Contains(sec, "from-env") || !strings.Contains(sec, dir) {
		t.Fatalf("section misses env skill or its path: %q", sec)
	}
}

// TestSkillsSectionEmptyInjectsNothing: no skills → no section at all (no
// empty heading in the system prompt).
func TestSkillsSectionEmptyInjectsNothing(t *testing.T) {
	t.Setenv("LYON_SKILLS_DIR", t.TempDir()) // empty dir
	if s := skillsSection(); s != "" {
		t.Fatalf("empty skills must inject nothing, got %q", s)
	}
	if s := renderSkillsSection(nil); s != "" {
		t.Fatalf("renderSkillsSection(nil) = %q, want empty", s)
	}
}

// TestSkillsSectionBudget: a long list stays within the total rune budget
// and announces the truncation instead of silently hiding entries.
func TestSkillsSectionBudget(t *testing.T) {
	var skills []SkillBrief
	for i := range 20 {
		skills = append(skills, SkillBrief{
			Name:        "skill-" + string(rune('a'+i)),
			Description: strings.Repeat("d", skillsDescBudget),
			Path:        `C:\somewhere\.claude\skills\skill\SKILL.md`,
		})
	}
	sec := renderSkillsSection(skills)
	if r := utf8.RuneCountInString(sec); r > skillsSectionBudget {
		t.Fatalf("section %d runes > budget %d", r, skillsSectionBudget)
	}
	if !strings.Contains(sec, "more skill(s)") {
		t.Fatalf("overflowing list must say it is partial, got tail %q", sec[len(sec)-80:])
	}
	if !strings.Contains(sec, "## Skills available (use read_file for full text)") {
		t.Fatalf("missing header: %q", sec[:120])
	}
}

// TestSessionSystemPromptCarriesSkills: the session creation path scans
// once and baseParams appends the block after the frozen base — bytes
// stable for the whole session (prompt cache stays warm).
func TestSessionSystemPromptCarriesSkills(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "inj", "---\nname: inj\ndescription: Injected into the system prompt.\n---\n")
	t.Setenv("LYON_SKILLS_DIR", dir)
	t.Setenv("LYON_HOME", t.TempDir()) // keep hooks off the real home
	cfg := DefaultConfig()
	cfg.Workspace = t.TempDir()
	cfg.WebToolMode = "off"
	eng := NewEngine(cfg, nil)
	t.Cleanup(eng.Close)
	sess := eng.NewSession(nil)
	t.Cleanup(sess.Close)

	if !strings.Contains(sess.skillsText, "inj") {
		t.Fatalf("skillsText = %q, want the env skill listed", sess.skillsText)
	}
	got := sess.baseParams(nil).System[0].Text
	if got != cfg.SystemPrompt+sess.skillsText+sess.shellText {
		t.Fatalf("system prompt must be frozen base + skills block + shell marker (cache-stable suffix)")
	}
	if !strings.Contains(got, "## Skills available (use read_file for full text)") {
		t.Fatalf("skills header missing from system prompt")
	}
}

// TestScanSkillsCRLFAndBlockScalar: CRLF line endings and YAML block
// scalars (description: | / > with indented continuation) both appear in
// the wild — a real ~/.claude/skills scan found exactly these. The
// description must come out as the joined text, not the bare indicator.
func TestScanSkillsCRLFAndBlockScalar(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "crlf", "---\r\nname: crlf\r\ndescription: CRLF file with quotes \"like this\".\r\n---\r\n\r\n# CRLF\r\n\r\nBody.\r\n")
	writeSkill(t, dir, "folded",
		"---\nname: folded\ndescription: >\n  folded lines join\n  into one line\nmetadata:\n  version: v1\n---\n\n# Folded\n")
	writeSkill(t, dir, "literal",
		"---\nname: literal\ndescription: |\n  literal block keeps\n  every line\ntriggers:\n  - x\n---\n\n# Literal\n")

	got := ScanSkills(dir)
	if len(got) != 3 {
		t.Fatalf("ScanSkills = %d entries, want 3: %+v", len(got), got)
	}
	if want := `CRLF file with quotes "like this".`; got[0].Description != want {
		t.Fatalf("crlf description = %q, want %q", got[0].Description, want)
	}
	if want := "folded lines join into one line"; got[1].Description != want {
		t.Fatalf("folded description = %q, want %q", got[1].Description, want)
	}
	if want := "literal block keeps every line"; got[2].Description != want {
		t.Fatalf("literal description = %q, want %q", got[2].Description, want)
	}
}
