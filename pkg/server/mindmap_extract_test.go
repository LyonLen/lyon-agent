package server

// Route-level acceptance for POST /v1/mindmap/{id}/extract (M2.5 §10):
// dry_run vs real, the §10.5 eligible gate flipping after the first run,
// 404/400 edges, and the store actually receiving cold facts. The pure
// extraction rules (M1–M5) live in pkg/mindmap/extract_test.go; this
// covers only the HTTP seam.
//
// The fixture pins Config.Workspace to a temp dir: the default resolves
// to "." (the package dir under go test), and a real run writes project-
// scope facts under <workspace>/.lyon/memory — never the repo.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lyon-agent/gen/go/lyon/v1/lyonv1connect"
	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/memory"
	"lyon-agent/pkg/mindmap"
	"lyon-agent/pkg/tools"
)

// newExtractFixture serves a server with an isolated workspace and one
// created seat; returns the HTTP client, the workspace and the session id.
func newExtractFixture(t *testing.T) (*testClient, string, string) {
	t.Helper()
	scratchHome(t)
	ws := t.TempDir()
	eng := agent.NewEngine(agent.Config{Workspace: ws}, nil)
	st := NewStore(0, 0)
	t.Cleanup(st.CloseAll)
	s := New(eng, st)
	s.SetObserver(tools.NewMetrics())
	c := newHTTPTestServer(t, s)

	code, body := c.postJSON("/v1/sessions", "{}")
	if code != http.StatusCreated {
		t.Fatalf("create session: %d %s", code, body)
	}
	return c, ws, jsonGetString(t, body, "session_id")
}

type extractResp struct {
	Eligible bool `json:"eligible"`
	Written  int  `json:"written"`
	Facts    []struct {
		Scope       string `json:"scope"`
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"facts"`
}

func postExtract(t *testing.T, c *testClient, sid, body string) (int, extractResp) {
	t.Helper()
	code, raw := c.postJSON("/v1/mindmap/"+sid+"/extract", body)
	var resp extractResp
	if code == http.StatusOK {
		if err := json.Unmarshal([]byte(raw), &resp); err != nil {
			t.Fatalf("response JSON: %v\n%s", err, raw)
		}
	}
	return code, resp
}

// TestMindmapExtractRoute: seed 4 confirmed + 2 proposed nodes, then —
// dry run lists without writing; real run writes exactly the confirmed
// chapter; a second real run keeps the count (upsert) while eligible
// flips false (本主题已抽过); unknown seat 404s; a bad body 400s.
func TestMindmapExtractRoute(t *testing.T) {
	c, ws, sid := newExtractFixture(t)
	mm := lyonv1connect.NewMindmapServiceClient(http.DefaultClient, c.ts.URL)

	// user batch: root + chapter + 4 confirmed facts (user adds pin →
	// confirmed), then agent proposes 2 more (stay proposed).
	updateMindmap(t, mm, sid, 0, "user", `[
		{"op":"add","id":"root","label":"抽取样例","kind":"branch"},
		{"op":"add","id":"ch","parent_id":"root","label":"需求","kind":"branch"},
		{"op":"add","id":"c1","parent_id":"ch","label":"确认事实一","kind":"fact"},
		{"op":"add","id":"c2","parent_id":"ch","label":"确认事实二","kind":"fact"},
		{"op":"add","id":"c3","parent_id":"ch","label":"确认决策三","kind":"decision"},
		{"op":"add","id":"c4","parent_id":"ch","label":"确认决策四","kind":"decision"}
	]`)
	updateMindmap(t, mm, sid, 1, "agent", `[
		{"op":"add","id":"g1","parent_id":"ch","label":"模型猜的一","kind":"fact"},
		{"op":"add","id":"g2","parent_id":"ch","label":"模型猜的二","kind":"decision"}
	]`)
	// Map titles come from distill rounds; a title-only diff is enough.
	if res := mindmap.Open().ApplyDistill(sid, 2, mindmap.Diff{Title: "抽取样例"}); !res.Changed {
		t.Fatal("title seed must land")
	}

	st := memory.Open(ws)
	if n := len(st.List("")); n != 0 {
		t.Fatalf("store pre-seeded: %d facts", n)
	}

	// dry_run: lists the would-be facts, writes nothing.
	code, resp := postExtract(t, c, sid, `{"dry_run":true}`)
	if code != http.StatusOK {
		t.Fatalf("dry run: %d", code)
	}
	if !resp.Eligible {
		t.Error("4 confirmed content nodes must be eligible")
	}
	if resp.Written != 0 {
		t.Errorf("dry run written = %d, want 0", resp.Written)
	}
	if len(resp.Facts) != 1 {
		t.Fatalf("dry facts = %d, want 1 (one chapter)", len(resp.Facts))
	}
	wantName := mindmap.FactNamePrefix("抽取样例") + "需求"
	if resp.Facts[0].Name != wantName {
		t.Errorf("fact name = %q, want %q", resp.Facts[0].Name, wantName)
	}
	if resp.Facts[0].Scope != memory.ScopeProject || resp.Facts[0].Description == "" {
		t.Errorf("fact view = %+v", resp.Facts[0])
	}
	if n := len(st.List("")); n != 0 {
		t.Errorf("dry run wrote %d facts", n)
	}
	if _, err := os.Stat(filepath.Join(ws, "docs")); err == nil {
		t.Error("dry run created docs/ archives")
	}

	// real run: writes, and eligible still reads true (computed pre-write).
	code, resp = postExtract(t, c, sid, `{"dry_run":false}`)
	if code != http.StatusOK {
		t.Fatalf("real run: %d", code)
	}
	if resp.Written != 1 || !resp.Eligible || len(resp.Facts) != 1 {
		t.Fatalf("real run: written=%d eligible=%v facts=%d", resp.Written, resp.Eligible, len(resp.Facts))
	}
	facts := st.List("")
	if len(facts) != 1 {
		t.Fatalf("store facts = %d, want 1", len(facts))
	}
	f := facts[0]
	if f.Name != wantName || f.Hot {
		t.Errorf("stored fact: name=%q hot=%v (must be cold)", f.Name, f.Hot)
	}
	if len(f.Tags) != 2 || f.Tags[0] != "mindmap" || f.Tags[1] != "抽取样例" {
		t.Errorf("tags = %v", f.Tags)
	}
	for _, banned := range []string{"模型猜的一", "模型猜的二"} {
		if strings.Contains(f.Body, banned) {
			t.Errorf("proposed content %q written to store", banned)
		}
	}

	// second real run: upsert — count stable, eligible flips false
	// (本主题已抽过 gates the §10.5 prompt, never the write itself).
	code, resp = postExtract(t, c, sid, `{"dry_run":false}`)
	if code != http.StatusOK {
		t.Fatalf("second run: %d", code)
	}
	if resp.Eligible {
		t.Error("eligible must flip false after the topic was extracted")
	}
	if resp.Written != 1 {
		t.Errorf("second run written = %d, want 1 (upsert, not fork)", resp.Written)
	}
	if n := len(st.List("")); n != 1 {
		t.Errorf("store facts after re-run = %d, want 1", n)
	}

	// edges: unknown seat → 404, malformed body → 400, empty body → 400.
	if code, _ := postExtract(t, c, "deadbeefdeadbeef", `{"dry_run":true}`); code != http.StatusNotFound {
		t.Errorf("unknown seat: %d, want 404", code)
	}
	if code, _ := postExtract(t, c, sid, `not-json`); code != http.StatusBadRequest {
		t.Errorf("bad body: %d, want 400", code)
	}
	if code, _ := postExtract(t, c, sid, ``); code != http.StatusBadRequest {
		t.Errorf("empty body: %d, want 400 (no accidental writes)", code)
	}
}
