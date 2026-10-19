package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"lyon-agent/internal/mockapi"
	"lyon-agent/pkg/memory"
)

// D1 hot-tier refresh: within ONE session, save_memory / forget_memory /
// distill writes must be visible to the next turn — content through
// buildHistory's store re-read, perception through the PushSystemNote
// acknowledgment queued by the tools' success callback.

// seedHotFact pins one fact into the hot tier directly through the store —
// the same Save path save_memory takes (a new fact carries Hot through
// Save; only re-saves preserve the stored flag).
func seedHotFact(t *testing.T, ws, title, body string) {
	t.Helper()
	if _, err := memory.Open(ws).Save(memory.Fact{
		Name:        memory.Slug(title),
		Description: title,
		Scope:       memory.ScopeProject,
		Body:        body,
		Hot:         true,
	}); err != nil {
		t.Fatalf("seed hot fact %q: %v", title, err)
	}
}

// TestSaveMemoryVisibleNextTurn walks the real path end to end: the
// scripted model calls save_memory in round 1, and round 2's API request
// must carry both the new fact (content) and the acknowledgment note
// (perception) — the EVOLUTION-GAPS D1 acceptance case.
func TestSaveMemoryVisibleNextTurn(t *testing.T) {
	mock := newCompactMock(t, []mockapi.Turn{
		{Tool: &mockapi.ToolCall{Name: "save_memory", Input: `{"title":"Deploy Gate","content":"DEPLOY-CODE-7749 gates every deploy","hot":true}`}},
		{Text: []string{"saved"}},
		{Text: []string{"round two"}},
	})
	t.Setenv("LYON_HOME", tempDir(t)) // memory dirs stay inside the sandbox
	ws := tempDir(t)
	eng := NewEngine(Config{Workspace: ws}, nil)
	t.Cleanup(eng.Close)
	sess := eng.NewSession(nil)
	t.Cleanup(sess.Close)

	if err := sess.Submit("remember the deploy gate"); err != nil {
		t.Fatalf("submit 1: %v", err)
	}
	waitPromptEnd(t, sess)
	// Round 1's history was assembled from an empty store: the injected
	// block is guidance-only since the cold-start fix, and must not carry
	// the not-yet-saved fact (the fact body itself is in there via the
	// tool_use echo — the rendered-fact marker is what must be absent).
	if b := string(mock.LastBody()); strings.Contains(b, "## (project)") {
		t.Fatalf("round-1 request carries the not-yet-saved fact as an injection: %s", b)
	}

	if err := sess.Submit("what did I just remember?"); err != nil {
		t.Fatalf("submit 2: %v", err)
	}
	waitPromptEnd(t, sess)

	body := string(mock.LastBody())
	if !strings.Contains(body, "[persistent-memory") {
		t.Fatalf("round-2 request misses the refreshed memory injection block (D1 content visibility): %s", body)
	}
	if !strings.Contains(body, "## (project) deploy-gate") {
		t.Fatalf("round-2 injection block does not render the new fact: %s", body)
	}
	if !strings.Contains(body, "DEPLOY-CODE-7749") {
		t.Fatalf("round-2 request misses the new hot fact: %s", body)
	}
	if !strings.Contains(body, "新记忆已存：deploy-gate") {
		t.Fatalf("round-2 request misses the save acknowledgment note (D1 perception): %s", body)
	}
}

// TestForgetMemoryRefreshNextTurn is the forget-side twin: round 1
// injects a seeded hot fact and forgets it; round 2 must carry the
// acknowledgment and the session's snapshot must drop the fact.
func TestForgetMemoryRefreshNextTurn(t *testing.T) {
	mock := newCompactMock(t, []mockapi.Turn{
		{Tool: &mockapi.ToolCall{Name: "forget_memory", Input: `{"title":"Deploy Gate"}`}},
		{Text: []string{"gone"}},
		{Text: []string{"round two"}},
	})
	t.Setenv("LYON_HOME", tempDir(t))
	ws := tempDir(t)
	seedHotFact(t, ws, "Deploy Gate", "DEPLOY-CODE-7749 gates every deploy")
	eng := NewEngine(Config{Workspace: ws}, nil)
	t.Cleanup(eng.Close)
	sess := eng.NewSession(nil)
	t.Cleanup(sess.Close)

	if err := sess.Submit("forget the deploy gate"); err != nil {
		t.Fatalf("submit 1: %v", err)
	}
	waitPromptEnd(t, sess)
	// The seeded fact WAS in round 1's injection — it is history from now
	// on; what D1 fixes is the snapshot the NEXT injection is built from.
	if b := string(mock.LastBody()); !strings.Contains(b, "DEPLOY-CODE-7749") {
		t.Fatalf("round-1 request never injected the seeded fact: %s", b)
	}

	if err := sess.Submit("done?"); err != nil {
		t.Fatalf("submit 2: %v", err)
	}
	waitPromptEnd(t, sess)

	body := string(mock.LastBody())
	if !strings.Contains(body, "已忘：deploy-gate") {
		t.Fatalf("round-2 request misses the forget acknowledgment note: %s", body)
	}
	sess.histMu.Lock()
	stale := sess.memoryText
	sess.histMu.Unlock()
	if strings.Contains(stale, "DEPLOY-CODE-7749") {
		t.Fatalf("memoryText still carries the forgotten fact: %q", stale)
	}
}

// TestDistillWriteVisibleNextPrompt covers the third writer of the hot
// tier: distill.go's applyDistill has no callback seam of its own (its
// file is another task's delivery), so the content path must pick up its
// Save+ToggleHot promotion from the store on the very next prompt.
func TestDistillWriteVisibleNextPrompt(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))
	ws := tempDir(t)
	sess := newTestSessionForRunner(t, Config{Workspace: ws})

	if before := sess.buildHistory("", "start"); strings.Contains(plainText(before[0]), "DISTILLED-BODY-7749") {
		t.Fatal("fact present before distill ran")
	}
	rep := sess.applyDistill(memory.Open(ws), []distillFact{{
		Name:        "distilled-gate",
		Description: "distilled deploy gate",
		Scope:       memory.ScopeProject,
		Body:        "DISTILLED-BODY-7749",
		Hot:         true,
	}})
	if rep.Saved != 1 || len(rep.Promoted) != 1 {
		t.Fatalf("applyDistill report = %+v", rep)
	}

	hist := sess.buildHistory("", "what did we distill?")
	if !strings.Contains(plainText(hist[0]), "DISTILLED-BODY-7749") {
		t.Fatalf("distill promotion invisible to the next prompt: %q", plainText(hist[0]))
	}
}

// TestMemoryCallbackSeamDirect exercises the callback seam without an API:
// the raw registry handlers see the session's listener through streamCtx
// (save → note + injected content, forget → note + emptied snapshot), and
// a ctx without the seam is a silent no-op.
func TestMemoryCallbackSeamDirect(t *testing.T) {
	t.Setenv("LYON_HOME", tempDir(t))
	ws := tempDir(t)
	sess := newTestSessionForRunner(t, Config{Workspace: ws})
	reg := sess.eng.Registry()

	save, ok := reg.Get("save_memory")
	if !ok {
		t.Fatal("save_memory not registered")
	}
	if _, err := save.Execute(sess.streamCtx, json.RawMessage(
		`{"title":"Deploy Gate","content":"DEPLOY-CODE-7749","hot":true}`)); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got := sess.takePendingNote(); got != "新记忆已存：deploy-gate" {
		t.Fatalf("save note = %q", got)
	}
	hist := sess.buildHistory("", "q1")
	if !strings.Contains(plainText(hist[0]), "DEPLOY-CODE-7749") {
		t.Fatalf("saved fact not injected next prompt: %q", plainText(hist[0]))
	}

	forget, ok := reg.Get("forget_memory")
	if !ok {
		t.Fatal("forget_memory not registered")
	}
	if _, err := forget.Execute(sess.streamCtx, json.RawMessage(
		`{"title":"Deploy Gate"}`)); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if got := sess.takePendingNote(); got != "已忘：deploy-gate" {
		t.Fatalf("forget note = %q", got)
	}
	_ = sess.buildHistory("", "q2")
	// Since the cold-start fix the snapshot falls back to guidance-only
	// text, not "" — the fact itself must be gone either way.
	if strings.Contains(sess.memoryText, "DEPLOY-CODE-7749") {
		t.Fatalf("memoryText not emptied after forget: %q", sess.memoryText)
	}

	// No session on the ctx: the same handler must run cleanly without a
	// listener (direct/probe callers), queuing nothing.
	if _, err := save.Execute(context.Background(), json.RawMessage(
		`{"title":"Solo Fact","content":"body","hot":true}`)); err != nil {
		t.Fatalf("save without seam: %v", err)
	}
	if got := sess.takePendingNote(); got != "" {
		t.Fatalf("phantom note without seam: %q", got)
	}
}
