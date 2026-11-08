package server

// POST /v1/mindmap/{id}/extract — M2.5 图 → memory 沉淀 (§10 of
// docs/design/mindmap.md). The extraction itself is pkg/mindmap's pure
// ExtractFacts; this file is the seam that owns everything non-pure:
// resolving the seat's map, the §10.5 eligibility gate, writing specs
// through the SAME memory.Store the save_memory tool uses
// (memory.Open(workspace) — one instance per workspace, Save upserts by
// scope+name), and the docs/ archive files for oversized chapters.
//
// Contract (the web codes against this verbatim):
//
//	POST /v1/mindmap/{id}/extract   {"dry_run": true|false}
//	200  {"eligible": bool, "written": int,
//	      "facts": [{"scope","name","description"}…]}   // body omitted
//	404  unknown seat / no map yet / empty id (mux)
//	400  unparseable body (an empty body is an error — no accidental writes)
//
// eligible = (≥4 alive confirmed decision|fact nodes) AND (no fact in ANY
// scope named 导图-<topic>-… yet) — §10.5's "≥4 且本主题未抽过" prompt
// gate, advisory only: a real run always writes (the click IS the
// consent, and a second run must upsert — M5). dry_run reports the
// would-be list without touching disk or the store.
//
// Auth rides the shared mux: Handler()'s bearer wrapper covers every
// route registered on s.mux (see server.go Handler), no bypass here.
// 图永不 hot (M4): specs carry no Hot and no §10.4 index fact is written.

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"lyon-agent/pkg/memory"
	"lyon-agent/pkg/mindmap"
)

type extractRequest struct {
	DryRun bool `json:"dry_run"`
}

// extractFact is the response's light fact view — no Body (it can be
// KBs with archives); scope/name/description is what the card lists.
type extractFact struct {
	Scope       string `json:"scope"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type extractResponse struct {
	Eligible bool          `json:"eligible"`
	Written  int           `json:"written"`
	Facts    []extractFact `json:"facts"`
}

// minConfirmedForPrompt is §10.5's bar: below it the card only nudges,
// never pre-emptively offers (点击才写).
const minConfirmedForPrompt = 4

func (s *Server) handleMindmapExtract(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeErr(w, http.StatusNotFound, errors.New("mindmap not found"))
		return
	}
	// Same seat resolution as GetMindmap — the map belongs to a live or
	// on-disk session, never to a dangling id.
	if _, err := s.seatRef(id); err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	rev, title, _, nodesJSON, err := mindmap.Open().Snapshot(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if rev == 0 || nodesJSON == "[]" || nodesJSON == "" {
		// A seat with no map yet is a 404 for THIS route: there is no
		// 图 to 沉淀 (Snapshot's revision-0/"[]" is a valid GetMindmap
		// answer, but extract has nothing to read).
		writeErr(w, http.StatusNotFound, errors.New("no mindmap for session"))
		return
	}

	var req extractRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("invalid JSON body: "+err.Error()))
		return
	}

	var nodes []mindmap.Node
	if err := json.Unmarshal([]byte(nodesJSON), &nodes); err != nil {
		writeErr(w, http.StatusInternalServerError, errors.New("mindmap nodes_json corrupt: "+err.Error()))
		return
	}
	specs := mindmap.ExtractFacts(title, nodes)

	st := memory.Open(s.wsFor(r.Header))
	eligible := mindmapExtractEligible(nodes) && !extractAlreadyDone(st, title)
	resp := extractResponse{
		Eligible: eligible,
		Facts:    make([]extractFact, 0, len(specs)),
	}
	if !req.DryRun {
		// Archives first: a fact body links its 明细 path, so a failed
		// archive write must not leave facts pointing at nothing.
		for _, sp := range specs {
			if sp.Archive == nil {
				continue
			}
			p := filepath.Join(s.wsFor(r.Header), filepath.FromSlash(sp.Archive.RelPath))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				writeErr(w, http.StatusInternalServerError, errors.New("archive dir: "+err.Error()))
				return
			}
			if err := os.WriteFile(p, []byte(sp.Archive.Body), 0o644); err != nil {
				writeErr(w, http.StatusInternalServerError, errors.New("archive write: "+err.Error()))
				return
			}
		}
		for _, sp := range specs {
			if _, err := st.Save(memory.Fact{
				Name:        sp.Name,
				Description: sp.Description,
				Scope:       sp.Scope,
				Tags:        sp.Tags,
				Hot:         false, // M4: 图永不 hot; Save preserves an old hot flag, none exists here
				Body:        sp.Body,
			}); err != nil {
				writeErr(w, http.StatusInternalServerError, err)
				return
			}
		}
		resp.Written = len(specs)
	}
	for _, sp := range specs {
		resp.Facts = append(resp.Facts, extractFact{
			Scope: sp.Scope, Name: sp.Name, Description: sp.Description,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// mindmapExtractEligible is the §10.5 count half: enough confirmed
// content to be worth persisting (per the route's shared predicate, so
// "eligible" counts exactly what a run would write).
func mindmapExtractEligible(nodes []mindmap.Node) bool {
	n := 0
	for _, node := range nodes {
		if mindmap.EligibleContent(node) {
			n++
		}
	}
	return n >= minConfirmedForPrompt
}

// extractAlreadyDone is the §10.5 本主题未抽过 half: ANY scope holding a
// 导图-<topic>-… fact means this map was extracted before (M5: the next
// run updates rather than forks, so this only gates the prompt, not the
// write).
func extractAlreadyDone(st *memory.Store, title string) bool {
	prefix := mindmap.FactNamePrefix(title)
	for _, f := range st.List("") {
		if strings.HasPrefix(f.Name, prefix) {
			return true
		}
	}
	return false
}
