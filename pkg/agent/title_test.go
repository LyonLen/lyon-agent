package agent

import (
	"context"
	"fmt"
	"testing"

	"lyon-agent/pkg/hooks"
)

func TestParseTitleReply(t *testing.T) {
	// Plain object.
	if changed, title := parseTitleReply(`{"changed":true,"title":"修复登录页布局"}`); !changed || title != "修复登录页布局" {
		t.Fatalf("plain: %v %q", changed, title)
	}
	// Continuation: no update.
	if changed, _ := parseTitleReply(`{"changed":false}`); changed {
		t.Fatal("changed=false parsed as change")
	}
	// Fenced (model habit).
	changed, title := parseTitleReply("```json\n{\"changed\":true,\"title\":\"add OpenAI provider\"}\n```")
	if !changed || title != "add OpenAI provider" {
		t.Fatalf("fenced: %v %q", changed, title)
	}
	// Prose around the object.
	if changed, title := parseTitleReply(`Sure! {"changed":true,"title":"update TUI scroll"}`); !changed || title != "update TUI scroll" {
		t.Fatalf("prose: %v %q", changed, title)
	}
	// Missing changed field is not a change.
	if changed, _ := parseTitleReply(`{"title":"x"}`); changed {
		t.Fatal("missing changed parsed as change")
	}
	// Malformed output never clobbers: parse as no-change.
	for _, junk := range []string{"", "no json", `{"changed":`, `{"changed":true`} {
		if changed, _ := parseTitleReply(junk); changed {
			t.Fatalf("junk %q parsed as change", junk)
		}
	}
	// changed:true without a title still reports the change (caller keeps old).
	if changed, title := parseTitleReply(`{"changed":true}`); !changed || title != "" {
		t.Fatalf("empty title: %v %q", changed, title)
	}
}

func TestRefreshTitleGates(t *testing.T) {
	mk := func(t *testing.T) *Session {
		t.Helper()
		return &Session{eng: &Engine{cfg: Config{Workspace: tempDir(t)}, hooks: hooks.New(tempDir(t))}}
	}
	// LYON_TITLE=off disables before any client work.
	t.Setenv("LYON_TITLE", "off")
	s := mk(t)
	if title, changed, err := s.RefreshTitle(context.Background()); title != "" || changed || err != nil {
		t.Fatalf("off: %q %v %v", title, changed, err)
	}
	// Enabled but no prompt yet: returns before touching the (zero) client.
	t.Setenv("LYON_TITLE", "")
	if title, changed, err := mk(t).RefreshTitle(context.Background()); title != "" || changed || err != nil {
		t.Fatalf("no prompt: %q %v %v", title, changed, err)
	}
}

func TestTitleSnapshotRoundtrip(t *testing.T) {
	ws := tempDir(t)
	eng := &Engine{cfg: Config{Workspace: ws, Model: "m"}, hooks: hooks.New(ws)}
	s := &Session{eng: eng}
	s.mu.Lock()
	s.title = "修复登录页布局"
	s.firstPrompt = "帮我修一下登录"
	s.mu.Unlock()

	snap := s.Export()
	if snap.Title != "修复登录页布局" || snap.FirstPrompt != "帮我修一下登录" {
		t.Fatalf("export: %+v", snap)
	}

	// Restore (resume) carries the title into the new session.
	s2 := &Session{eng: eng}
	s2.Restore(snap)
	if got := s2.Title(); got != "修复登录页布局" {
		t.Fatalf("restored title: %q", got)
	}

	// Reset clears it with the conversation.
	s2.Reset()
	if got := s2.Title(); got != "" {
		t.Fatalf("title after reset: %q", got)
	}
}

// TestTitleRecorder: Submit records the prompts RefreshTitle reads —
// last tracks every prompt, first stays the session anchor. Drive is
// simulated by draining reqs and clearing busy between submissions.
func TestTitleRecorder(t *testing.T) {
	ws := tempDir(t)
	s := &Session{
		eng:  &Engine{cfg: Config{Workspace: ws}, hooks: hooks.New(ws)},
		ctx:  context.Background(),
		reqs: make(chan submitReq, 1),
	}
	t.Cleanup(s.eng.hooks.Flush)

	submit := func(p string) {
		t.Helper()
		if err := s.Submit(p); err != nil {
			t.Fatalf("Submit(%q): %v", p, err)
		}
		<-s.reqs // the drive goroutine's pickup
		s.busy.Store(false)
	}
	submit("第一个话题")
	submit("换话题了")

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.firstPrompt != "第一个话题" || s.lastPrompt != "换话题了" {
		t.Fatalf("first=%q last=%q", s.firstPrompt, s.lastPrompt)
	}
	if len(s.recentPrompts) != 2 || s.recentPrompts[0] != "第一个话题" || s.recentPrompts[1] != "换话题了" {
		t.Fatalf("recentPrompts = %q", s.recentPrompts)
	}
}

func TestAppendPromptTail(t *testing.T) {
	var tail []string
	for i := 1; i <= 7; i++ {
		tail = appendPromptTail(tail, fmt.Sprintf("p%d", i))
	}
	if len(tail) != titleRecentN || tail[0] != "p3" || tail[len(tail)-1] != "p7" {
		t.Fatalf("cap: %q", tail)
	}
	// Blank prompts never enter the tail (and never shift the window).
	tail = appendPromptTail(nil, "  ", "", " keep ", "\n")
	if len(tail) != 1 || tail[0] != "keep" {
		t.Fatalf("blank skip: %q", tail)
	}
}

func TestIntervalTitleGates(t *testing.T) {
	mk := func(t *testing.T) *Session {
		t.Helper()
		return &Session{eng: &Engine{cfg: Config{Workspace: tempDir(t)}, hooks: hooks.New(tempDir(t))}}
	}
	// LYON_TITLE=off disables before any client work.
	t.Setenv("LYON_TITLE", "off")
	s := mk(t)
	if title, changed, err := s.IntervalTitle(context.Background()); title != "" || changed || err != nil {
		t.Fatalf("off: %q %v %v", title, changed, err)
	}
	// Enabled but no prompt yet: returns before touching the (zero) client.
	t.Setenv("LYON_TITLE", "")
	if title, changed, err := mk(t).IntervalTitle(context.Background()); title != "" || changed || err != nil {
		t.Fatalf("no prompt: %q %v %v", title, changed, err)
	}
	// A restored session predates the tail: the input falls back to the
	// first prompt instead of refusing (recentPrompts isn't snapshotted).
	r := mk(t)
	r.mu.Lock()
	r.firstPrompt = "restored topic"
	r.mu.Unlock()
	current, recent := r.intervalTitleInput()
	if current != "" || len(recent) != 1 || recent[0] != "restored topic" {
		t.Fatalf("restore fallback: current=%q recent=%q", current, recent)
	}
}
