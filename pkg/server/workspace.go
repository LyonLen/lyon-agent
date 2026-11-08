package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// wsHeader names the per-session workspace override carried by RPC calls.
// The web injects it from the selected session's workspace — a header, not
// a proto field, so every file/git/memory surface follows the session
// without regenerating wire code.
const wsHeader = "x-lyon-workspace"

// wsFor resolves the filesystem root for one RPC: the x-lyon-workspace
// header (the session's chosen workspace — must be an existing absolute
// directory) when present, else the engine default.
func (s *Server) wsFor(h http.Header) string {
	ws := h.Get(wsHeader)
	if ws == "" {
		return s.eng.Config().Workspace
	}
	abs, err := filepath.Abs(ws)
	if err != nil {
		return s.eng.Config().Workspace
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return s.eng.Config().Workspace
	}
	return abs
}

// handleGitClone clones a repository and answers the clone's absolute
// path — the "clone from git" branch of the new-session workspace
// chooser. POST {"url", "dir"}; dir is optional and defaults to a
// sibling of the engine workspace, named after the repo.
func (s *Server) handleGitClone(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, errors.New("POST only"))
		return
	}
	var body struct {
		URL string `json:"url"`
		Dir string `json:"dir"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("invalid JSON body: "+err.Error()))
		return
	}
	url := strings.TrimSpace(body.URL)
	if !isCloneURL(url) {
		writeErr(w, http.StatusBadRequest,
			errors.New("a git URL (https://, http://, git@, ssh://, file:// or a local path) is required"))
		return
	}
	name := repoNameFromURL(url)
	if name == "" {
		writeErr(w, http.StatusBadRequest, errors.New("cannot derive a repository name from the URL"))
		return
	}
	target := strings.TrimSpace(body.Dir)
	var parent string
	if target != "" {
		abs, err := filepath.Abs(target)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		target = abs
		parent = filepath.Dir(abs)
	} else {
		ws := s.eng.Config().Workspace
		if abs, err := filepath.Abs(ws); err == nil {
			ws = abs
		}
		parent = filepath.Dir(ws)
		target = filepath.Join(parent, name)
	}
	if _, err := os.Stat(target); err == nil {
		writeErr(w, http.StatusConflict, fmt.Errorf("target already exists: %s", target))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	if out, err := gitRun(ctx, parent, "clone", url, target); err != nil {
		msg := out
		if len(msg) > 400 {
			msg = msg[:400]
		}
		writeErr(w, http.StatusBadGateway, fmt.Errorf("git clone: %v: %s", err, msg))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"workspace": target})
}

// isCloneURL accepts the remote shapes git does, plus a bare local path
// (offline clones and tests).
func isCloneURL(u string) bool {
	if u == "" {
		return false
	}
	for _, p := range []string{"https://", "http://", "git@", "ssh://", "file://"} {
		if strings.HasPrefix(u, p) {
			return true
		}
	}
	return !strings.Contains(u, "://")
}

// repoNameFromURL derives the clone directory name from a remote URL.
func repoNameFromURL(u string) string {
	s := strings.TrimRight(u, "/")
	if i := strings.LastIndexAny(s, ":/"); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimSuffix(s, ".git")
	if s == "" || s == "." || s == ".." {
		return ""
	}
	return s
}

// handleSessionWorkspace reads (GET) or sets (POST) one session's
// workspace root — the per-session workspace selection seam. POST body
// {"workspace": "..."}; "" resets the override back to the engine default.
func (s *Server) handleSessionWorkspace(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := s.acquire(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if r.Method == http.MethodPost {
		var body struct {
			Workspace string `json:"workspace"`
		}
		if r.ContentLength > 0 {
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
				writeErr(w, http.StatusBadRequest, errors.New("invalid JSON body: "+err.Error()))
				return
			}
		}
		if err := sess.SetWorkspace(body.Workspace); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"workspace": sess.Workspace()})
}
