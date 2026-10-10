package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Durable workspace memory: a small JSON file the model maintains through
// three tools (save/read/forget). Survives sessions; a session's first
// prompt carries a rendered snapshot (MemoryFacts) so continuity works even
// before the model touches a tool.

const (
	memoryFileVersion = 1
	memoryMaxEntries  = 500 // beyond this, the oldest entries are evicted
	memoryMaxContent  = 8192
	memoryMaxTitle    = 80
)

type memoryEntry struct {
	Title     string    `json:"title"`
	Content   string    `json:"content"`
	UpdatedAt time.Time `json:"updated_at"`
}

type memoryFile struct {
	Version int           `json:"version"`
	Entries []memoryEntry `json:"entries"`
}

// memMu serializes memory file access process-wide: several sessions of one
// engine can share a workspace, and read-modify-write must not interleave.
var memMu sync.Mutex

// MemoryPath is where a workspace's durable memory lives.
func MemoryPath(workspace string) string {
	return filepath.Join(workspace, ".lyon", "memory.json")
}

func loadMemory(path string) []memoryEntry {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var f memoryFile
	if json.Unmarshal(data, &f) != nil {
		return nil
	}
	return f.Entries
}

// saveMemory writes atomically (tmp + rename) so a crash mid-write cannot
// truncate an existing memory file.
func saveMemory(path string, entries []memoryEntry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(memoryFile{Version: memoryFileVersion, Entries: entries}, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// saveFact upserts an entry keyed by title (case-insensitive); evicts the
// oldest entries past memoryMaxEntries; clips oversized content.
func saveFact(path, title, content string) (string, error) {
	title = strings.TrimSpace(title)
	content = strings.TrimSpace(content)
	if title == "" || content == "" {
		return "", fmt.Errorf("title and content are required")
	}
	if r := []rune(title); len(r) > memoryMaxTitle {
		title = string(r[:memoryMaxTitle])
	}
	if r := []rune(content); len(r) > memoryMaxContent {
		content = string(r[:memoryMaxContent]) + "\n... [truncated]"
	}
	memMu.Lock()
	defer memMu.Unlock()
	entries := loadMemory(path)
	now := time.Now().UTC()
	for i := range entries {
		if strings.EqualFold(entries[i].Title, title) {
			entries[i].Content = content
			entries[i].UpdatedAt = now
			if err := saveMemory(path, entries); err != nil {
				return "", err
			}
			return fmt.Sprintf("updated memory %q", entries[i].Title), nil
		}
	}
	entries = append(entries, memoryEntry{Title: title, Content: content, UpdatedAt: now})
	if len(entries) > memoryMaxEntries {
		entries = entries[len(entries)-memoryMaxEntries:]
	}
	if err := saveMemory(path, entries); err != nil {
		return "", err
	}
	return fmt.Sprintf("saved memory %q (%d entries)", title, len(entries)), nil
}

// readFacts returns one entry by title, or all of them when title is empty.
// A named-but-missing title errors, so the model gets an is_error result
// and can correct itself.
func readFacts(path, title string) (string, error) {
	memMu.Lock()
	defer memMu.Unlock()
	entries := loadMemory(path)
	title = strings.TrimSpace(title)
	if title != "" {
		for _, e := range entries {
			if strings.EqualFold(e.Title, title) {
				return fmt.Sprintf("## %s\n%s\n(updated %s)", e.Title, e.Content,
					e.UpdatedAt.Format(time.RFC3339)), nil
			}
		}
		return "", fmt.Errorf("no memory titled %q", title)
	}
	if len(entries) == 0 {
		return "(no memories saved yet)", nil
	}
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "## %s\n%s\n\n", e.Title, e.Content)
	}
	return strings.TrimSpace(b.String()), nil
}

// forgetFact deletes one entry by title; missing titles error.
func forgetFact(path, title string) (string, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return "", fmt.Errorf("title is required")
	}
	memMu.Lock()
	defer memMu.Unlock()
	entries := loadMemory(path)
	for i, e := range entries {
		if strings.EqualFold(e.Title, title) {
			entries = append(entries[:i], entries[i+1:]...)
			if err := saveMemory(path, entries); err != nil {
				return "", err
			}
			return fmt.Sprintf("forgot memory %q", e.Title), nil
		}
	}
	return "", fmt.Errorf("no memory titled %q", title)
}

// MemoryFacts renders the workspace's saved memories as the block injected
// into a session's first prompt; "" when nothing is saved.
func MemoryFacts(workspace string) string {
	memMu.Lock()
	entries := loadMemory(MemoryPath(workspace))
	memMu.Unlock()
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[persistent-memory]\n")
	b.WriteString("Durable facts saved across sessions with save_memory, captured at session start. Call read_memory for the current list.\n")
	for _, e := range entries {
		b.WriteString("\n## " + e.Title + "\n" + e.Content + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

type memorySaveInput struct {
	Title   string `json:"title" jsonschema:"required,description=Short label for the fact (e.g. 'project codename'); saved case-insensitively, re-saving the same title updates it"`
	Content string `json:"content" jsonschema:"required,description=The fact itself; markdown allowed"`
}

type memoryReadInput struct {
	Title string `json:"title" jsonschema:"description=Entry title; omit to list all memories"`
}

type memoryForgetInput struct {
	Title string `json:"title" jsonschema:"required,description=Exact title of the memory to delete"`
}

// RegisterMemory adds the durable-memory tools backed by the JSON file at
// path: save_memory (serial — mutates shared state), read_memory (parallel),
// forget_memory (serial).
func RegisterMemory(r *Registry, path string) error {
	if err := RegisterFunc(r, "save_memory",
		"Persist a durable fact across sessions (upsert by title). Use for project context, user preferences, decisions — anything the next session should know.",
		false,
		func(ctx context.Context, in memorySaveInput) (string, error) {
			return saveFact(path, in.Title, in.Content)
		}); err != nil {
		return err
	}
	if err := RegisterFunc(r, "read_memory",
		"Read durable memories saved with save_memory: give a title for one entry, omit it to list everything.",
		true,
		func(ctx context.Context, in memoryReadInput) (string, error) {
			return readFacts(path, in.Title)
		}); err != nil {
		return err
	}
	return RegisterFunc(r, "forget_memory",
		"Delete a durable memory by its exact title.",
		false,
		func(ctx context.Context, in memoryForgetInput) (string, error) {
			return forgetFact(path, in.Title)
		})
}
