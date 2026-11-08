package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	lyonv1 "lyon-agent/gen/go/lyon/v1"
	"lyon-agent/gen/go/lyon/v1/lyonv1connect"
	"lyon-agent/pkg/agent"
)

// TestWorkspaceHeaderRetargetsFileService: the x-lyon-workspace header
// points file RPCs at the session's workspace (per-session workspace
// selection — a header, not a proto field); junk falls back to the
// engine root.
func TestWorkspaceHeaderRetargetsFileService(t *testing.T) {
	def := t.TempDir()
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(def, "def.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "here.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	eng := agent.NewEngine(agent.Config{Workspace: def}, nil)
	t.Cleanup(eng.Close)
	s := New(eng, NewStore(0, 0))
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	client := lyonv1connect.NewFileServiceClient(ts.Client(), ts.URL)
	ctx := context.Background()

	names := func(req *connect.Request[lyonv1.ListDirectoryRequest]) []string {
		res, err := client.ListDirectory(ctx, req)
		if err != nil {
			t.Fatalf("ListDirectory: %v", err)
		}
		out := make([]string, 0, len(res.Msg.Entries))
		for _, e := range res.Msg.Entries {
			out = append(out, e.Name)
		}
		return out
	}

	defList := names(connect.NewRequest(&lyonv1.ListDirectoryRequest{}))
	if len(defList) != 1 || defList[0] != "def.txt" {
		t.Fatalf("default root listing = %v, want [def.txt]", defList)
	}

	withHeader := connect.NewRequest(&lyonv1.ListDirectoryRequest{})
	withHeader.Header().Set(wsHeader, other)
	otherList := names(withHeader)
	if len(otherList) != 1 || otherList[0] != "here.txt" {
		t.Fatalf("header root listing = %v, want [here.txt]", otherList)
	}

	junk := connect.NewRequest(&lyonv1.ListDirectoryRequest{})
	junk.Header().Set(wsHeader, filepath.Join(def, "no-such-dir"))
	junkList := names(junk)
	if len(junkList) != 1 || junkList[0] != "def.txt" {
		t.Fatalf("junk header listing = %v, want default root's [def.txt]", junkList)
	}
}

// TestSessionWorkspaceEndpoint: GET/POST /api/session/{id}/workspace is
// the selection seam — POST retargets the session, GET reads it back.
func TestSessionWorkspaceEndpoint(t *testing.T) {
	def := t.TempDir()
	other := t.TempDir()
	eng := agent.NewEngine(agent.Config{Workspace: def}, nil)
	t.Cleanup(eng.Close)
	s := New(eng, NewStore(0, 0))
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	sess := eng.NewSession(nil)
	id, err := s.store.Create(sess)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	read := func() string {
		res, err := ts.Client().Get(ts.URL + "/v1/sessions/" + id + "/workspace")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer res.Body.Close()
		var body struct {
			Workspace string `json:"workspace"`
		}
		if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return body.Workspace
	}

	otherAbs, _ := filepath.Abs(other)
	post := func(ws string) *http.Response {
		t.Helper()
		body, err := json.Marshal(map[string]string{"workspace": ws})
		if err != nil {
			t.Fatal(err)
		}
		res, err := ts.Client().Post(
			ts.URL+"/v1/sessions/"+id+"/workspace",
			"application/json",
			strings.NewReader(string(body)),
		)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		return res
	}
	res := post(otherAbs)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST status = %d", res.StatusCode)
	}
	if got := read(); got != otherAbs {
		t.Fatalf("workspace after POST = %q, want %q", got, otherAbs)
	}

	// A bad root is rejected and leaves the override alone.
	res2 := post(filepath.Join(def, "no-such-dir"))
	res2.Body.Close()
	if res2.StatusCode == http.StatusOK {
		t.Fatalf("bad workspace must not be accepted")
	}
	if got := read(); got != otherAbs {
		t.Fatalf("workspace after bad POST = %q, want %q", got, otherAbs)
	}
}

// TestGitCloneEndpoint: the new-session chooser's "clone from git" branch
// — POST /v1/git/clone clones into the target dir and answers its
// absolute path; an existing target is a 409, never a clobber.
func TestGitCloneEndpoint(t *testing.T) {
	eng := agent.NewEngine(agent.Config{Workspace: t.TempDir()}, nil)
	t.Cleanup(eng.Close)
	s := New(eng, NewStore(0, 0))
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	// An empty bare repo clones fine and needs no identity config.
	src := filepath.Join(t.TempDir(), "src.git")
	if out, err := exec.Command("git", "init", "--bare", src).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	target := filepath.Join(t.TempDir(), "clone-target")

	post := func(body string) (int, string) {
		t.Helper()
		res, err := ts.Client().Post(ts.URL+"/v1/git/clone", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		raw, _ := io.ReadAll(res.Body)
		res.Body.Close()
		return res.StatusCode, string(raw)
	}

	body, err := json.Marshal(map[string]string{"url": src, "dir": target})
	if err != nil {
		t.Fatal(err)
	}
	code, raw := post(string(body))
	if code != http.StatusOK {
		t.Fatalf("clone status = %d: %s", code, raw)
	}
	var got struct {
		Workspace string `json:"workspace"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	targetAbs, _ := filepath.Abs(target)
	if got.Workspace != targetAbs {
		t.Fatalf("workspace = %q, want %q", got.Workspace, targetAbs)
	}

	if code, _ := post(string(body)); code != http.StatusConflict {
		t.Fatalf("second clone status = %d, want 409", code)
	}
	if code, raw := post(`{"url":""}`); code != http.StatusBadRequest {
		t.Fatalf("empty url status = %d: %s", code, raw)
	}
}
