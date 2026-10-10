package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Cloud sync lets several agents (this machine's projects, other machines,
// teammates) share the team layer and the user's personal layer. The
// protocol is deliberately dumb JSON over HTTP so any tiny server (or a
// static file gateway) can implement it:
//
//	POST $LYON_SYNC_URL/memory/push  {"facts":[Fact…]}   → {"accepted":N}
//	POST $LYON_SYNC_URL/memory/pull  {"scopes":["team","personal"]}  → {"facts":[Fact…]}
//
// Merge rule in both directions: newer `updated` wins; equal timestamps
// prefer the side with more hits; nothing is ever deleted remotely by a
// push (deletes propagate only if the server chooses to honor them — our
// client sends only upserts). Authorization: optional
// "Authorization: Bearer $LYON_SYNC_TOKEN".
//
// Scope: team + personal sync (project memory is workspace-bound and
// stays local). Disabled entirely when LYON_SYNC_URL is unset.

// ErrSyncDisabled means no LYON_SYNC_URL configured.
var ErrSyncDisabled = errors.New("memory sync not configured (set LYON_SYNC_URL)")

// syncScopes are the layers that leave the machine.
var syncScopes = []string{ScopeTeam, ScopePersonal}

type syncClient struct {
	base  string
	token string
	http  *http.Client
}

func newSyncClient() *syncClient {
	base := strings.TrimRight(os.Getenv("LYON_SYNC_URL"), "/")
	if base == "" {
		return nil
	}
	return &syncClient{
		base:  base,
		token: os.Getenv("LYON_SYNC_TOKEN"),
		http:  &http.Client{Timeout: 15 * time.Second},
	}
}

// Sync pushes local team/personal facts, pulls remote ones, and merges
// both ways (newer updated wins). Returns counts for the caller to show.
func (s *Store) Sync(ctx context.Context) (pushed, pulled int, err error) {
	c := newSyncClient()
	if c == nil {
		return 0, 0, ErrSyncDisabled
	}
	local := s.exportForSync()
	if len(local) > 0 {
		var resp struct {
			Accepted int `json:"accepted"`
		}
		if err := c.post(ctx, "/memory/push", map[string]any{"facts": local}, &resp); err != nil {
			return 0, 0, fmt.Errorf("push: %w", err)
		}
		pushed = resp.Accepted
		if pushed == 0 {
			pushed = len(local)
		}
	}
	var remote struct {
		Facts []Fact `json:"facts"`
	}
	if err := c.post(ctx, "/memory/pull", map[string]any{"scopes": syncScopes}, &remote); err != nil {
		return pushed, 0, fmt.Errorf("pull: %w", err)
	}
	for _, f := range remote.Facts {
		if !validScope(f.Scope) || f.Scope == ScopeProject {
			continue // protocol abuse guard: project stays local
		}
		if s.mergeIncoming(f) {
			pulled++
		}
	}
	return pushed, pulled, nil
}

// exportForSync snapshots team+personal facts.
func (s *Store) exportForSync() []Fact {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	var out []Fact
	for _, scope := range syncScopes {
		for _, f := range s.facts[scope] {
			out = append(out, *f)
		}
	}
	return out
}

// mergeIncoming upserts a remote fact when it is strictly newer than the
// local copy; a local-only fact is untouched. Returns whether it changed.
func (s *Store) mergeIncoming(f Fact) bool {
	if !validScope(f.Scope) || f.Scope == ScopeProject {
		return false
	}
	f.Name = sanitizeName(f.Name)
	if f.Name == "" || strings.TrimSpace(f.Body) == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	if old, ok := s.facts[f.Scope][strings.ToLower(f.Name)]; ok {
		if f.Updated.Before(old.Updated) || (f.Updated.Equal(old.Updated) && f.Hits <= old.Hits) {
			return false
		}
		f.Created = old.Created
		f.Hot = old.Hot
	}
	if f.Created.IsZero() {
		f.Created = f.Updated
	}
	dir := s.dirs[f.Scope]
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	if err := atomicWrite(filepath.Join(dir, f.Name+".md"), f.encode()); err != nil {
		return false
	}
	s.insertLocked(f)
	return true
}

func (c *syncClient) post(ctx context.Context, path string, body any, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("server %s: %s", resp.Status, trunc(string(data), 200))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
