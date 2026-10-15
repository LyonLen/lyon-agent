package memory

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
)

// DiagCategory is the fact category failure post-mortems (错因归纳)
// land in: the name prefix, the tag and the dedup namespace all share
// it, so the lessons group under one handle for /memory and search.
const DiagCategory = "failed-conditions"

// diagHotKeep bounds how many post-mortems live in the resident hot
// tier at once: MemoryFacts injects only that tier (4KB across all
// scopes), so unbounded pinning would crowd out standing preferences.
// The newest lessons stay injected; older ones fall back to search.
const diagHotKeep = 3

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
// fact pinned into the hot tier, so the next session's MemoryFacts
// injection carries the lesson (design §1.3: 同类错误全场可见).
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
		Hot:         true, // Save keeps the flag on new facts; MemoryFacts reads hot only
		Body:        diagBody(clean, patch),
	}
	saved, err := s.Save(f)
	if err != nil {
		return Fact{}, false, err
	}
	s.trimDiagHot(name)
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

// diagBody renders the lesson as the fact body — the exact text the
// hot-tier injection shows the next session.
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

// trimDiagHot keeps at most diagHotKeep post-mortems in the hot tier:
// the one just saved (keep) plus the most recently updated of the rest.
// Older lessons leave the resident tier but stay in the cold project
// layer — reachable through search, just not injected every session.
func (s *Store) trimDiagHot(keep string) {
	var hot []Fact
	for _, f := range s.List(ScopeProject) {
		if f.Hot && f.Name != keep && hasTag(f.Tags, DiagCategory) {
			hot = append(hot, f)
		}
	}
	sort.Slice(hot, func(i, j int) bool {
		if hot[i].Updated.Equal(hot[j].Updated) {
			return hot[i].Name < hot[j].Name
		}
		return hot[i].Updated.After(hot[j].Updated)
	})
	// One slot of diagHotKeep is reserved for the fact just saved.
	for i := diagHotKeep - 1; i < len(hot); i++ {
		_, _ = s.ToggleHot(ScopeProject, hot[i].Name) // hot -> cold
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
