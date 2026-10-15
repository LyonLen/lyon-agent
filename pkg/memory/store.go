package memory

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store is the process-wide handle on one workspace's three memory layers.
// One Store per workspace (cached by Open) keeps a single inverted index
// hot in memory; dir-mtime staleness checks pick up facts another agent
// process wrote without a full rescan per query.
type Store struct {
	mu        sync.RWMutex
	workspace string
	dirs      map[string]string // scope -> directory
	facts     map[string]map[string]*Fact
	ix        index
	scanTime  time.Time
	dirTime   map[string]time.Time
	dirty     map[string]bool // facts whose hit counter hasn't reached disk
}

var (
	storesMu sync.Mutex
	stores   = map[string]*Store{}
)

// Open returns the cached store for a workspace, loading all three scopes
// and building the index on first use. Never fails: a broken directory
// yields an empty layer, not an error — memory is best-effort by design.
func Open(workspace string) *Store {
	key, err := filepath.Abs(workspace)
	if err != nil {
		key = workspace
	}
	storesMu.Lock()
	defer storesMu.Unlock()
	if s := stores[key]; s != nil {
		return s
	}
	s := &Store{
		workspace: key,
		dirs:      map[string]string{},
		facts:     map[string]map[string]*Fact{},
		dirTime:   map[string]time.Time{},
		dirty:     map[string]bool{},
	}
	if home := homeDir(); home != "" {
		s.dirs[ScopeTeam] = filepath.Join(home, "memory", "team")
		s.dirs[ScopePersonal] = filepath.Join(home, "memory", "personal")
	}
	s.dirs[ScopeProject] = filepath.Join(key, ".lyon", "memory")
	s.rescanLocked()
	migrateLegacy(key, s)
	stores[key] = s
	return s
}

// homeDir mirrors pkg/hooks: $LYON_HOME or ~/.lyon.
func homeDir() string {
	if h := os.Getenv("LYON_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".lyon")
}

// Reload forces a rescan — call after an external editor changed files
// (dir mtimes only catch adds/removes, not in-place edits).
func (s *Store) Reload() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rescanLocked()
}

// rescanLocked rebuilds facts + index from disk.
func (s *Store) rescanLocked() {
	prev := s.facts
	s.facts = map[string]map[string]*Fact{}
	var all []*Fact
	for scope, dir := range s.dirs {
		s.facts[scope] = map[string]*Fact{}
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			f, err := decodeFact(filepath.Join(dir, e.Name()), scope, raw)
			if err != nil || strings.TrimSpace(f.Body) == "" {
				continue
			}
			f.Scope = scope
			cp := f
			s.facts[scope][strings.ToLower(f.Name)] = &cp
			all = append(all, &cp)
		}
		if st, err := os.Stat(dir); err == nil {
			s.dirTime[scope] = st.ModTime()
		} else {
			s.dirTime[scope] = time.Time{}
		}
	}
	// Preserve hit counters from this process's memory when disk hasn't
	// recorded newer numbers (Search bumps hits without rewriting files).
	for scope, m := range prev {
		for name, old := range m {
			if cur, ok := s.facts[scope][name]; ok && cur.Hits < old.Hits {
				cur.Hits = old.Hits
			}
		}
	}
	s.ix.build(all)
	s.scanTime = time.Now()
}

// staleLocked reports whether another process added/removed files since
// the last scan (three stats, sub-millisecond).
func (s *Store) staleLocked() bool {
	for scope, dir := range s.dirs {
		st, err := os.Stat(dir)
		if err != nil {
			continue
		}
		if st.ModTime().After(s.dirTime[scope]) {
			return true
		}
	}
	return false
}

func (s *Store) refreshLocked() {
	if s.staleLocked() {
		s.rescanLocked()
	}
}

// Save upserts a fact (by scope+name) and reindexes. Scope defaults to
// project; Created is preserved on update.
func (s *Store) Save(f Fact) (Fact, error) {
	if !validScope(f.Scope) {
		f.Scope = ScopeProject
	}
	f.Name = sanitizeName(f.Name)
	if f.Name == "" {
		return f, errors.New("memory name is required")
	}
	if strings.TrimSpace(f.Body) == "" {
		return f, errors.New("memory body is required")
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	if old, ok := s.facts[f.Scope][strings.ToLower(f.Name)]; ok {
		f.Created = old.Created
		f.Hits = old.Hits
		f.Hot = old.Hot // hot tier moves only through ToggleHot, not Save
	} else if f.Created.IsZero() {
		f.Created = now
	}
	if f.Updated.IsZero() {
		f.Updated = now
	}
	for i, t := range f.Tags {
		f.Tags[i] = strings.ToLower(strings.TrimSpace(t))
	}
	dir := s.dirs[f.Scope]
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return f, fmt.Errorf("memory dir: %w", err)
	}
	path := filepath.Join(dir, f.Name+".md")
	if err := atomicWrite(path, f.encode()); err != nil {
		return f, err
	}
	s.insertLocked(f)
	// This write just carried f's hits to disk; Save is also the explicit
	// persist point for counters Search/Touch parked on other facts.
	delete(s.dirty, dirtyKey(f.Scope, f.Name))
	s.flushDirtyLocked()
	return f, nil
}

// insertLocked indexes one fact without a full rescan.
func (s *Store) insertLocked(f Fact) {
	if s.facts[f.Scope] == nil {
		s.facts[f.Scope] = map[string]*Fact{}
	}
	cp := f
	s.facts[f.Scope][strings.ToLower(f.Name)] = &cp
	var all []*Fact
	for _, m := range s.facts {
		for _, fp := range m {
			all = append(all, fp)
		}
	}
	s.ix.build(all)
	if st, err := os.Stat(s.dirs[f.Scope]); err == nil {
		s.dirTime[f.Scope] = st.ModTime()
	}
}

// Delete removes one fact; missing names error so the caller can report.
func (s *Store) Delete(scope, name string) error {
	if !validScope(scope) {
		scope = ScopeProject
	}
	name = sanitizeName(name)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	if _, ok := s.facts[scope][strings.ToLower(name)]; !ok {
		return fmt.Errorf("no memory %q in scope %q", name, scope)
	}
	os.Remove(filepath.Join(s.dirs[scope], name+".md"))
	s.rescanLocked() // full rescan keeps dir-time bookkeeping honest
	return nil
}

// Get returns one fact by scope+name.
func (s *Store) Get(scope, name string) (Fact, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	if m, ok := s.facts[scope]; ok {
		if f := m[strings.ToLower(name)]; f != nil {
			return *f, true
		}
	}
	return Fact{}, false
}

// List returns the facts of one scope ("" = all scopes), closer scopes
// first, then by name.
func (s *Store) List(scope string) []Fact {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	var out []Fact
	for sc, m := range s.facts {
		if scope != "" && sc != scope {
			continue
		}
		for _, f := range m {
			out = append(out, *f)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if scopeRank(out[i].Scope) != scopeRank(out[j].Scope) {
			return scopeRank(out[i].Scope) > scopeRank(out[j].Scope)
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Search runs a BM25 query across all scopes and returns the top k hits.
// A query that finds a fact bumps its hit counter in memory — the counter
// reaches disk only at a persist point (Save, ToggleHot, Flush), so a
// query storm never writes. The counter feeds hot-tier promotion decisions.
func (s *Store) Search(query string, k int) []Hit {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	hits := s.ix.search(query, k)
	for _, h := range hits {
		if m, ok := s.facts[h.Fact.Scope]; ok {
			if f := m[strings.ToLower(h.Fact.Name)]; f != nil {
				f.Hits++
				s.markDirtyLocked(f)
			}
		}
	}
	return hits
}

// Touch bumps one fact's hit counter (tool-driven reads). Like Search the
// counter accumulates in memory and lands at the next persist point.
func (s *Store) Touch(scope, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	f, ok := s.facts[scope][strings.ToLower(name)]
	if !ok {
		return
	}
	f.Hits++
	s.markDirtyLocked(f)
}

// ToggleHot flips the hot flag; hot facts render into the system prompt.
func (s *Store) ToggleHot(scope, name string) (bool, error) {
	if !validScope(scope) {
		scope = ScopeProject
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	f, ok := s.facts[scope][strings.ToLower(sanitizeName(name))]
	if !ok {
		return false, fmt.Errorf("no memory %q in scope %q", name, scope)
	}
	f.Hot = !f.Hot
	f.Updated = time.Now().UTC()
	path := filepath.Join(s.dirs[scope], f.Name+".md")
	if err := atomicWrite(path, f.encode()); err != nil {
		return f.Hot, err
	}
	// The write above carried f's hits along; other queued counters land
	// now too — ToggleHot is a persist point by virtue of writing anyway.
	delete(s.dirty, dirtyKey(f.Scope, f.Name))
	s.flushDirtyLocked()
	return f.Hot, nil
}

// Flush persists the hit counters Search/Touch accumulated in memory.
// Save and ToggleHot flush as part of their own writes, so Flush is the
// process-exit safety net: call it right before the process leaves.
// No-op when nothing is pending.
func (s *Store) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushDirtyLocked()
}

// dirtyKey identifies a fact whose hit counter has not reached disk.
func dirtyKey(scope, name string) string {
	return scope + "\x00" + strings.ToLower(name)
}

// markDirtyLocked queues f's hit counter for the next persist point.
func (s *Store) markDirtyLocked(f *Fact) {
	if s.dirty == nil {
		s.dirty = map[string]bool{}
	}
	s.dirty[dirtyKey(f.Scope, f.Name)] = true
}

// flushDirtyLocked writes every queued counter. A fact deleted in the
// meantime drops out of the set; a failed write keeps its fact queued so
// the in-memory count survives and the next persist point retries it.
func (s *Store) flushDirtyLocked() {
	for key := range s.dirty {
		scope, lower, _ := strings.Cut(key, "\x00")
		f := s.facts[scope][lower]
		if f == nil {
			delete(s.dirty, key)
			continue
		}
		if err := atomicWrite(filepath.Join(s.dirs[scope], f.Name+".md"), f.encode()); err == nil {
			delete(s.dirty, key)
		}
	}
}

// HotText renders hot facts for injection into the system prompt, closest
// scopes first, until maxBytes is reached (whole facts only; 0 = default
// 4KB). This is the "hot tier": curated, resident, always in context.
func (s *Store) HotText(maxBytes int) string {
	if maxBytes <= 0 {
		maxBytes = 4096
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	order := []string{ScopeProject, ScopePersonal, ScopeTeam}
	var b strings.Builder
	b.WriteString("[persistent-memory — hot tier; full store via read_memory/search]\n")
	used := b.Len()
	any := false
	for _, scope := range order {
		m := s.facts[scope]
		// Stable order within a scope: most recently updated first.
		var fs []*Fact
		for _, f := range m {
			if f.Hot {
				fs = append(fs, f)
			}
		}
		sort.Slice(fs, func(i, j int) bool { return fs[i].Updated.After(fs[j].Updated) })
		for _, f := range fs {
			entry := "\n## (" + scope + ") " + f.Name + "\n"
			if f.Description != "" {
				entry += f.Description + "\n"
			}
			entry += strings.TrimSpace(f.Body) + "\n"
			if used+len(entry) > maxBytes && any {
				return strings.TrimRight(b.String(), "\n")
			}
			b.WriteString(entry)
			used += len(entry)
			any = true
		}
	}
	if !any {
		return ""
	}
	return strings.TrimRight(b.String(), "\n")
}

// Stats summarizes the store for /memory's header.
type Stats struct {
	Team, Personal, Project int
	Hot                     int
}

// StoreStats counts facts per scope.
func (s *Store) StoreStats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	var st Stats
	for scope, m := range s.facts {
		for _, f := range m {
			switch scope {
			case ScopeTeam:
				st.Team++
			case ScopePersonal:
				st.Personal++
			default:
				st.Project++
			}
			if f.Hot {
				st.Hot++
			}
		}
	}
	return st
}

func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// migrateLegacy imports the old single-file workspace memory
// (<workspace>/.lyon/memory.json) as project-scope facts, one per entry,
// and parks the original aside so the import runs exactly once.
func migrateLegacy(workspace string, s *Store) {
	legacy := filepath.Join(workspace, ".lyon", "memory.json")
	raw, err := os.ReadFile(legacy)
	if err != nil {
		return
	}
	var f struct {
		Entries []struct {
			Title     string    `json:"title"`
			Content   string    `json:"content"`
			UpdatedAt time.Time `json:"updated_at"`
		} `json:"entries"`
	}
	if json.Unmarshal(raw, &f) != nil || len(f.Entries) == 0 {
		os.Rename(legacy, legacy+".imported")
		return
	}
	for _, e := range f.Entries {
		name := slug(e.Title)
		if name == "" {
			continue
		}
		if _, exists := s.Get(ScopeProject, name); exists {
			continue
		}
		s.Save(Fact{
			Name:        name,
			Description: e.Title,
			Scope:       ScopeProject,
			Body:        e.Content,
			Created:     e.UpdatedAt,
			Updated:     e.UpdatedAt,
		})
	}
	os.Rename(legacy, legacy+".imported")
}
