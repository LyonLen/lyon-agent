package memory

import (
	"fmt"
	"hash/fnv"
	"strings"
)

// DiagCategory is the fact category failure post-mortems (错因归纳)
// land in: the name prefix, the tag and the dedup namespace all share
// it, so the lessons group under one handle for /memory and search.
const DiagCategory = "failed-conditions"

// diagBodyCap clips one post-mortem body — the diagnoser output is
// bounded, but the local-fallback path must not write an unbounded
// lesson either.
const diagBodyCap = 4096

// diagKey fingerprints conditions+patch with distill.go's distillKey
// fnv64a pattern (copied rather than reaching into pkg/agent internals):
// equal keys mean the same lesson. The key rides in the fact name, so
// an existing name IS the dedup check — a repeat never rewrites.
func diagKey(conds []string, patch string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(strings.Join(conds, "\n")))
	_, _ = h.Write([]byte{0}) // patch may hold newlines — keep fields apart
	_, _ = h.Write([]byte(patch))
	return h.Sum64()
}

// SaveDiagnosis persists a failure post-mortem as one project-scope
// fact in the COLD layer. It used to pin hot ("同类错误全场可见"), but
// an incident-bound lesson — one naming a single paste image — hijacked
// an unrelated session and dragged it into the wrong task (2026-11-11:
// budget chat + FQC skill). Standing preferences need the hot slots far
// more; post-mortems stay reachable through read_memory search instead.
// Deduped by content: a repeat diagnosis writes nothing (saved=false,
// no error), an empty post-mortem is a silent no-op — the same mistake
// never stacks up in the store.
func (s *Store) SaveDiagnosis(conds []string, patch string) (Fact, bool, error) {
	clean := make([]string, 0, len(conds))
	for _, c := range conds {
		if c = strings.TrimSpace(c); c != "" {
			clean = append(clean, c)
		}
	}
	patch = strings.TrimSpace(patch)
	if len(clean) == 0 && patch == "" {
		return Fact{}, false, nil
	}
	name := fmt.Sprintf("%s-%016x", DiagCategory, diagKey(clean, patch))
	if old, ok := s.Get(ScopeProject, name); ok {
		return old, false, nil // the lesson is already on file
	}
	f := Fact{
		Name:        name,
		Description: diagDescription(clean, patch),
		Scope:       ScopeProject,
		Tags:        []string{DiagCategory},
		Hot:         false, // cold by policy — see the function comment
		Body:        diagBody(clean, patch),
	}
	saved, err := s.Save(f)
	if err != nil {
		return Fact{}, false, err
	}
	return saved, true, nil
}

// diagDescription is the list-view line: the first checkable condition
// (or the patch alone when the diagnoser found conditions only for a
// retry). Frontmatter values are single-line, so flatten before clip.
func diagDescription(conds []string, patch string) string {
	desc := "patch: " + patch
	if len(conds) > 0 {
		desc = conds[0]
	}
	desc = oneLine(desc)
	if r := []rune(desc); len(r) > 200 {
		desc = string(r[:200])
	}
	return desc
}

// diagBody renders the lesson as the fact body — the exact text
// read_memory / search surfaces when the next session looks it up.
func diagBody(conds []string, patch string) string {
	var b strings.Builder
	if len(conds) > 0 {
		b.WriteString("Failed conditions (verify before the next attempt):\n")
		for _, c := range conds {
			b.WriteString("- " + c + "\n")
		}
	}
	if patch != "" {
		b.WriteString("Patch: " + patch)
	}
	body := strings.TrimSpace(b.String())
	if r := []rune(body); len(r) > diagBodyCap {
		body = string(r[:diagBodyCap]) + "\n... [truncated]"
	}
	return body
}

// demoteDiagHot turns the hot flag off on every failed-conditions fact
// in the project layer. Open calls it once per load: saves from before
// the 2026-11-11 cold-only policy landed hot, and those legacy lessons
// would otherwise keep steering sessions that have nothing to do with
// the original incident. Cold keeps them searchable without letting an
// old failure call the shots in a new conversation.
func (s *Store) demoteDiagHot() {
	for _, f := range s.List(ScopeProject) {
		if f.Hot && hasTag(f.Tags, DiagCategory) {
			_, _ = s.ToggleHot(ScopeProject, f.Name) // hot -> cold
		}
	}
}

// hasTag reports whether tags contains tag (exact match; Save
// lowercases stored tags, and DiagCategory is already lowercase).
func hasTag(tags []string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}
