// Package memory is the layered, indexed memory system: team, personal and
// project scopes each hold one-fact-per-file markdown documents, searched
// with a local BM25 inverted index (CJK bigram tokenizer — ranked recall,
// no grep-speed or substring coupling), and a hot tier that renders
// straight into the session's system prompt.
//
// Layout:
//
//	$LYON_HOME/memory/team/*.md       shared across projects and agents
//	$LYON_HOME/memory/personal/*.md   this user, every project
//	<workspace>/.lyon/memory/*.md     this project (closest scope wins ties)
//
// File format — flat frontmatter a human can hand-edit:
//
//	---
//	name: toast-notification-preference
//	description: 用户要求通知用右下角 toast，不要老式弹窗
//	scope: personal
//	tags: ui, notification
//	hot: true
//	created: 2026-10-10T18:00:00Z
//	updated: 2026-10-10T18:30:00Z
//	hits: 3
//	---
//	正文……
package memory

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// Scopes, outermost to closest. Search ties prefer the closer scope.
const (
	ScopeTeam     = "team"
	ScopePersonal = "personal"
	ScopeProject  = "project"
)

// Facts is one durable memory. Name is unique per scope and doubles as
// the file's base name.
type Fact struct {
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Scope       string    `json:"scope"`
	Tags        []string  `json:"tags,omitempty"`
	Hot         bool      `json:"hot,omitempty"`
	Created     time.Time `json:"created"`
	Updated     time.Time `json:"updated"`
	Hits        int       `json:"hits,omitempty"`
	Body        string    `json:"body"`
}

// scopeRank orders facts for tie-breaks: the project layer wins over
// personal, personal over team (closest context resolves conflicts).
func scopeRank(scope string) int {
	switch scope {
	case ScopeProject:
		return 3
	case ScopePersonal:
		return 2
	case ScopeTeam:
		return 1
	}
	return 0
}

var (
	unsafeName = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)
	multiDash  = regexp.MustCompile(`_+`)
)

// sanitizeName makes a fact name safe as a file base across platforms —
// CJK is kept (Windows and POSIX both accept it), path-hostile bytes go.
func sanitizeName(name string) string {
	name = strings.TrimSpace(name)
	name = unsafeName.ReplaceAllString(name, "_")
	name = strings.Trim(name, ". ")
	return multiDash.ReplaceAllString(name, "_")
}

func validScope(scope string) bool {
	return scope == ScopeTeam || scope == ScopePersonal || scope == ScopeProject
}

// encode renders the fact as the on-disk markdown file.
func (f Fact) encode() []byte {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "name: %s\n", oneLine(f.Name))
	if f.Description != "" {
		fmt.Fprintf(&b, "description: %s\n", oneLine(f.Description))
	}
	fmt.Fprintf(&b, "scope: %s\n", f.Scope)
	if len(f.Tags) > 0 {
		fmt.Fprintf(&b, "tags: %s\n", strings.Join(f.Tags, ", "))
	}
	if f.Hot {
		b.WriteString("hot: true\n")
	}
	if !f.Created.IsZero() {
		fmt.Fprintf(&b, "created: %s\n", f.Created.UTC().Format(time.RFC3339))
	}
	if !f.Updated.IsZero() {
		fmt.Fprintf(&b, "updated: %s\n", f.Updated.UTC().Format(time.RFC3339))
	}
	if f.Hits > 0 {
		fmt.Fprintf(&b, "hits: %d\n", f.Hits)
	}
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimSpace(f.Body))
	b.WriteString("\n")
	return []byte(b.String())
}

// decodeFact parses a fact file. Frontmatter is optional for hand-written
// files: without it, name comes from the file base and the whole text is
// the body.
func decodeFact(path, scope string, raw []byte) (Fact, error) {
	base := strings.TrimSuffix(filepath.Base(path), ".md")
	text := string(raw)
	f := Fact{Scope: scope, Name: base}
	if !strings.HasPrefix(text, "---\n") {
		f.Body = strings.TrimSpace(text)
		if f.Body == "" {
			return f, fmt.Errorf("empty memory file %s", path)
		}
		return f, nil
	}
	rest := text[4:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		f.Body = strings.TrimSpace(text) // unterminated frontmatter: keep raw
		return f, nil
	}
	fm := rest[:end]
	body := rest[end+4:]
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		body = body[i+1:] // skip the newline right after ---
	}
	f.Body = strings.TrimSpace(body)
	for _, line := range strings.Split(fm, "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)
		switch key {
		case "name":
			if val != "" {
				f.Name = val
			}
		case "description":
			f.Description = val
		case "scope":
			if validScope(val) {
				f.Scope = val
			}
		case "tags":
			for _, t := range strings.Split(val, ",") {
				if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
					f.Tags = append(f.Tags, t)
				}
			}
		case "hot":
			f.Hot = val == "true" || val == "yes" || val == "1"
		case "created":
			if ts, err := time.Parse(time.RFC3339, val); err == nil {
				f.Created = ts
			}
		case "updated":
			if ts, err := time.Parse(time.RFC3339, val); err == nil {
				f.Updated = ts
			}
		case "hits":
			fmt.Sscanf(val, "%d", &f.Hits)
		}
	}
	if strings.TrimSpace(f.Name) == "" {
		f.Name = base
	}
	return f, nil
}

// oneLine keeps frontmatter values on a single line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// Slug derives the fact id from a title (tool args, legacy import):
// lowercase words joined by dashes, CJK preserved.
func Slug(title string) string { return slug(title) }

// slug is the unexported implementation of Slug.
func slug(title string) string {
	var b strings.Builder
	lastDash := true // no leading dash
	for _, r := range strings.ToLower(strings.TrimSpace(title)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
