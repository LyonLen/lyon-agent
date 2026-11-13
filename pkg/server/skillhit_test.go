package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"lyon-agent/pkg/agent"
)

// writeSkill drops one frontmattered SKILL.md under root/dir and
// returns its absolute path.
func writeSkill(t *testing.T, root, dir, name, desc string) string {
	t.Helper()
	d := filepath.Join(root, dir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(d, "SKILL.md")
	body := "---\nname: " + name + "\ndescription: " + desc + "\n---\n\n# " + name + "\nbody\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func toolInput(t *testing.T, m map[string]string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestSkillHitDetection pins both read paths the dump forensics
// surfaced: a workspace-relative .lyon/skills read (read_file works),
// an absolute personal read (read_file escapes-workspace → the model's
// run_shell fallback), and the misses — foreign SKILL.md files and a
// bare "SKILL.md" after a cd must never card.
func TestSkillHitDetection(t *testing.T) {
	personal := t.TempDir()
	t.Setenv("LYON_SKILLS_DIR", personal)
	t.Setenv("LYON_HOME", t.TempDir()) // commons isolated from the real ~/.lyon
	ws := t.TempDir()

	pPath := writeSkill(t, personal, "graphify", "graphify", "any input into a knowledge graph")
	wPath := writeSkill(t, filepath.Join(ws, ".lyon", "skills"), "dcs-fqc-slide", "dcs-fqc-slide", "slide builder")
	fPath := writeSkill(t, filepath.Join(ws, "vendor"), "otherskill", "otherskill", "foreign repo skill")

	cases := []struct {
		name     string
		tool     string
		input    map[string]string
		wantOK   bool
		wantName string
		wantPath string
		wantDesc string
	}{
		{
			name:     "read_file workspace-relative",
			tool:     "read_file",
			input:    map[string]string{"path": ".lyon/skills/dcs-fqc-slide/SKILL.md"},
			wantOK:   true,
			wantName: "dcs-fqc-slide",
			wantPath: wPath,
			wantDesc: "slide builder",
		},
		{
			// The personal absolute path: Confine rejects it at execute
			// time, but the START still names a skill — the fallback that
			// follows is what actually cards.
			name:     "read_file absolute personal",
			tool:     "read_file",
			input:    map[string]string{"path": pPath},
			wantOK:   true,
			wantName: "graphify",
			wantPath: pPath,
			wantDesc: "any input into a knowledge graph",
		},
		{
			name:   "read_file foreign SKILL.md",
			tool:   "read_file",
			input:  map[string]string{"path": "vendor/otherskill/SKILL.md"},
			wantOK: false,
		},
		{
			name:   "read_file foreign absolute",
			tool:   "read_file",
			input:  map[string]string{"path": fPath},
			wantOK: false,
		},
		{
			name:     "run_shell cat personal",
			tool:     "run_shell",
			input:    map[string]string{"command": "cat " + pPath},
			wantOK:   true,
			wantName: "graphify",
			wantPath: pPath,
		},
		{
			name:     "run_shell head tilde-style absolute with spaces around",
			tool:     "run_shell",
			input:    map[string]string{"command": "sed -n '40,200p' \"" + pPath + "\""},
			wantOK:   true,
			wantName: "graphify",
		},
		{
			name:     "run_shell workspace-relative",
			tool:     "run_shell",
			input:    map[string]string{"command": "head -40 .lyon/skills/dcs-fqc-slide/SKILL.md"},
			wantOK:   true,
			wantName: "dcs-fqc-slide",
		},
		{
			name:   "run_shell bare SKILL.md after cd",
			tool:   "run_shell",
			input:  map[string]string{"command": "cd vendor/otherskill && sed -n '1,40p' SKILL.md"},
			wantOK: false,
		},
		{
			name:   "run_shell grep mention only",
			tool:   "run_shell",
			input:  map[string]string{"command": "grep -rn SKILL.md ."},
			wantOK: false,
		},
		{
			name:   "write_file never hits",
			tool:   "write_file",
			input:  map[string]string{"path": pPath},
			wantOK: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hit, ok := skillHitFromInput(ws, c.tool, toolInput(t, c.input))
			if ok != c.wantOK {
				t.Fatalf("ok=%v want %v (hit=%+v)", ok, c.wantOK, hit)
			}
			if !c.wantOK {
				return
			}
			if hit.name != c.wantName {
				t.Errorf("name=%q want %q", hit.name, c.wantName)
			}
			if c.wantPath != "" && filepath.Clean(hit.path) != filepath.Clean(c.wantPath) {
				t.Errorf("path=%q want %q", hit.path, c.wantPath)
			}
			if c.wantDesc != "" && hit.desc != c.wantDesc {
				t.Errorf("desc=%q want %q", hit.desc, c.wantDesc)
			}
		})
	}
}

// TestUnderRootBoundary: a sibling directory sharing the root's prefix
// (skills2 vs skills) is NOT under the root, case drift on the path IS
// accepted (the model retypes paths from the system prompt).
func TestUnderRootBoundary(t *testing.T) {
	root := filepath.Join(`C:\ws`, "skills")
	if underRoot(root, filepath.Join(`C:\ws`, "skills2", "x", "SKILL.md")) {
		t.Error("sibling prefix dir counted as under root")
	}
	if !underRoot(root, filepath.Join(`C:\WS`, "SKILLS", "x", "SKILL.md")) {
		t.Error("case drift must still count as under root")
	}
	if underRoot(root, root) {
		t.Error("the root itself is not a skill path")
	}
}

// TestSkillHitEndFiresOnce: exec_end cards only successes, collapses
// chunked re-reads of one skill, and a failed execution cards nothing.
// Cards go through inject → the replay ring — the same seam reconnect
// replay reads.
func TestSkillHitEndFiresOnce(t *testing.T) {
	h := newHub(nil) // run() never starts; the handlers are driven directly
	h.ws = t.TempDir()

	// A non-skill read stashes nothing.
	h.noteSkill(agent.Event{
		Type: agent.EventToolExecStart, ToolName: "read_file", ExecID: 1,
		Input: toolInput(t, map[string]string{"path": t.TempDir()}),
	})
	if len(h.skillPending) != 0 {
		t.Fatalf("non-skill read stashed: %v", h.skillPending)
	}

	// Two chunk reads of one skill + one of another + one failure.
	h.skillPending = map[uint64]skillHit{
		10: {name: "graphify", path: `C:\s\SKILL.md`},
		11: {name: "graphify", path: `C:\s\SKILL.md`},
		12: {name: "dcs", path: `C:\w\SKILL.md`},
		13: {name: "failed", path: `C:\f\SKILL.md`},
	}
	carded := func() []string {
		var out []string
		for _, it := range h.dump() {
			if it.ev.Type == evSkillUsed {
				out = append(out, it.ev.Attrs["name"])
			}
		}
		return out
	}
	end := func(id uint64, isErr bool) {
		h.skillHitEnd(agent.Event{Type: agent.EventToolExecEnd, ExecID: id, IsError: isErr})
	}

	end(10, false)
	if got := carded(); len(got) != 1 || got[0] != "graphify" {
		t.Fatalf("first hit must card graphify once: %v", got)
	}
	end(11, false) // same skill again — collapsed
	if got := carded(); len(got) != 1 {
		t.Fatalf("chunked re-read must collapse: %v", got)
	}
	end(12, false)
	if got := carded(); len(got) != 2 || got[1] != "dcs" {
		t.Fatalf("second skill must card: %v", got)
	}
	end(13, true) // failed read: no card
	if got := carded(); len(got) != 2 {
		t.Fatalf("failed read must not card: %v", got)
	}
	// prompt_end resets lastSkill (run()'s wiring) — next run cards again.
	h.lastSkill = ""
	h.skillPending = map[uint64]skillHit{20: {name: "graphify", path: `C:\s\SKILL.md`}}
	end(20, false)
	if got := carded(); len(got) != 3 {
		t.Fatalf("next run must card the same skill again: %v", got)
	}
	// The card carries identity attrs the panel reads.
	for _, it := range h.dump() {
		if it.ev.Type == evSkillUsed && it.ev.Attrs["name"] == "graphify" {
			if it.ev.Attrs["path"] == "" {
				t.Error("card carries no path attr")
			}
			return
		}
	}
	t.Fatal("graphify card missing")
}
