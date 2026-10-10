package tools

import (
	"context"
	"fmt"
	"strings"

	"lyon-agent/pkg/memory"
)

// Durable memory surfaced to the model through three tools, backed by the
// layered store in pkg/memory (team / personal / project scopes, BM25
// search, hot tier). The session's first prompt carries the hot tier
// (MemoryFacts) so continuity works even before the model touches a tool;
// read_memory with a query searches cold facts ranked by relevance.

const (
	memoryMaxContent = 8192
	memoryMaxTitle   = 80
	memorySearchTop  = 8
	memoryHotBytes   = 4096
)

// MemoryFacts renders the hot tier for injection into a session's first
// prompt; "" when no fact is marked hot.
func MemoryFacts(workspace string) string {
	st := memory.Open(workspace)
	hot := st.HotText(memoryHotBytes)
	if hot == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("[persistent-memory — hot tier, always in context]\n")
	b.WriteString("Durable facts across sessions. Use read_memory with a query to search the cold tiers (team/personal/project), save_memory to persist new facts, forget_memory to delete. /memory in the TUI shows and manages the store.\n")
	b.WriteString(hot)
	return b.String()
}

// clipFact keeps a single fact within the per-entry size budget.
func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "\n... [truncated]"
}

func validScopeArg(scope string) (string, error) {
	switch scope {
	case "", memory.ScopeProject, memory.ScopePersonal, memory.ScopeTeam:
		if scope == "" {
			return memory.ScopeProject, nil
		}
		return scope, nil
	}
	return "", fmt.Errorf("scope must be team, personal or project (got %q)", scope)
}

// resolveName accepts an exact fact name or a title slug ("Build Command"
// → "build-command", CJK preserved).
func resolveName(title string) string {
	title = strings.TrimSpace(title)
	if title == "" {
		return ""
	}
	return title
}

type memorySaveInput struct {
	Title   string `json:"title" jsonschema:"required,description=Short unique label for the fact; re-saving the same title (any case) updates it"`
	Content string `json:"content" jsonschema:"required,description=The durable fact itself; markdown allowed"`
	Scope   string `json:"scope" jsonschema:"description=team (shared across all projects/agents), personal (this user everywhere), project (this codebase only). Default: project"`
	Hot     bool   `json:"hot" jsonschema:"description=true pins the fact into the always-loaded hot tier (keep the hot tier small: preferences and hard constraints, not trivia)"`
}

type memoryReadInput struct {
	Title string `json:"title" jsonschema:"description=Exact fact name (or its title form) to read in full; omit to combine with query or list"`
	Query string `json:"query" jsonschema:"description=Relevance search across all scopes (BM25, CJK-aware) — preferred when you don't know the exact name"`
	Scope string `json:"scope" jsonschema:"description=Restrict to team, personal or project"`
}

type memoryForgetInput struct {
	Title string `json:"title" jsonschema:"required,description=Exact fact name (or its title form) to delete"`
	Scope string `json:"scope" jsonschema:"description=Scope to delete from; without it the first match across scopes is removed (project first)"`
}

// RegisterMemory adds the durable-memory tools backed by the layered
// store for workspace: save_memory (serial — mutates shared state),
// read_memory (parallel — search/list), forget_memory (serial).
func RegisterMemory(r *Registry, workspace string) error {
	if err := RegisterFunc(r, "save_memory",
		"Persist a durable fact across sessions (upsert by title). Scopes: project (this codebase), personal (your preferences everywhere), team (shared across projects). Use for decisions, preferences, conventions — anything the next session must know. hot=true pins it into the always-loaded tier.",
		false,
		func(ctx context.Context, in memorySaveInput) (string, error) {
			scope, err := validScopeArg(in.Scope)
			if err != nil {
				return "", err
			}
			title := strings.TrimSpace(in.Title)
			content := strings.TrimSpace(in.Content)
			if title == "" || content == "" {
				return "", fmt.Errorf("title and content are required")
			}
			if runes := []rune(title); len(runes) > memoryMaxTitle {
				title = string(runes[:memoryMaxTitle])
			}
			st := memory.Open(workspace)
			name := memory.Slug(title)
			if _, err := st.Save(memory.Fact{
				Name:        name,
				Description: title,
				Scope:       scope,
				Body:        clipRunes(content, memoryMaxContent),
			}); err != nil {
				return "", err
			}
			hotNote := ""
			if in.Hot {
				if f, _ := st.Get(scope, name); f.Name != "" && !f.Hot {
					st.ToggleHot(scope, name)
				}
				hotNote = ", hot tier"
			}
			return fmt.Sprintf("saved memory %q (%s scope)%s", title, scope, hotNote), nil
		}); err != nil {
		return err
	}
	if err := RegisterFunc(r, "read_memory",
		"Search or read durable memories: query= does relevance search across scopes (preferred), title= reads one fact in full, neither lists everything compactly.",
		true,
		func(ctx context.Context, in memoryReadInput) (string, error) {
			st := memory.Open(workspace)
			scope := strings.TrimSpace(in.Scope)
			if scope != "" {
				if _, err := validScopeArg(scope); err != nil {
					return "", err
				}
			}
			if q := strings.TrimSpace(in.Query); q != "" {
				hits := st.Search(q, memorySearchTop)
				if scope != "" {
					kept := hits[:0]
					for _, h := range hits {
						if h.Fact.Scope == scope {
							kept = append(kept, h)
						}
					}
					hits = kept
				}
				if len(hits) == 0 {
					return fmt.Sprintf("no memories match %q", q), nil
				}
				var b strings.Builder
				for _, h := range hits {
					writeFactBlock(&b, h.Fact, 1200)
				}
				return strings.TrimSpace(b.String()), nil
			}
			if title := resolveName(in.Title); title != "" {
				name := memory.Slug(title)
				scopes := []string{scope}
				if scope == "" {
					scopes = []string{memory.ScopeProject, memory.ScopePersonal, memory.ScopeTeam}
				}
				for _, sc := range scopes {
					if sc == "" {
						continue
					}
					if f, ok := st.Get(sc, name); ok {
						st.Touch(sc, name)
						var b strings.Builder
						writeFactBlock(&b, f, memoryMaxContent)
						return strings.TrimSpace(b.String()), nil
					}
				}
				return "", fmt.Errorf("no memory titled %q (try query= to search)", title)
			}
			// Compact listing of everything (scoped when asked).
			facts := st.List(scope)
			if len(facts) == 0 {
				return "(no memories saved yet)", nil
			}
			var b strings.Builder
			for _, f := range facts {
				mark := ""
				if f.Hot {
					mark = " [hot]"
				}
				fmt.Fprintf(&b, "(%s) %s%s — %s\n", f.Scope, f.Name, mark, oneLine(f.Description))
			}
			return strings.TrimSpace(b.String()), nil
		}); err != nil {
		return err
	}
	return RegisterFunc(r, "forget_memory",
		"Delete a durable memory by its exact name (list them with read_memory and no arguments).",
		false,
		func(ctx context.Context, in memoryForgetInput) (string, error) {
			title := resolveName(in.Title)
			if title == "" {
				return "", fmt.Errorf("title is required")
			}
			name := memory.Slug(title)
			st := memory.Open(workspace)
			scopes := []string{memory.ScopeProject, memory.ScopePersonal, memory.ScopeTeam}
			if in.Scope != "" {
				if _, err := validScopeArg(in.Scope); err != nil {
					return "", err
				}
				scopes = []string{in.Scope}
			}
			for _, sc := range scopes {
				if _, ok := st.Get(sc, name); ok {
					if err := st.Delete(sc, name); err != nil {
						return "", err
					}
					return fmt.Sprintf("forgot memory %q (%s scope)", name, sc), nil
				}
			}
			return "", fmt.Errorf("no memory named %q", name)
		})
}

func writeFactBlock(b *strings.Builder, f memory.Fact, cap int) {
	fmt.Fprintf(b, "## (%s) %s", f.Scope, f.Name)
	if f.Hot {
		b.WriteString(" [hot]")
	}
	b.WriteString("\n")
	if f.Description != "" && f.Description != f.Name {
		b.WriteString(f.Description + "\n")
	}
	b.WriteString(clipRunes(f.Body, cap) + "\n\n")
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
