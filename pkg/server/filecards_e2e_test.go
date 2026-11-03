package server

// End-to-end file-card test: the real wire, not the hub seam.
//
// What the existing filecards_test.go proves is the diff + the tap: hub in,
// event out. What a user actually does is submit a prompt over ConnectRPC
// and have the model run a shell command that writes a file. This test
// covers that path — scripted mock gateway → engine tool loop → run_shell
// writes into the workspace → prompt_end → scheduleFileCards → the
// files_changed event arriving on WatchEvents with its rows parsed back.
//
// It is deliberately gated on bash being resolvable: pkg/tools' shell
// detector needs one, and a Windows box without git-bash would otherwise
// fail this for an unrelated reason.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"

	lyonv1 "lyon-agent/gen/go/lyon/v1"
	"lyon-agent/gen/go/lyon/v1/lyonv1connect"

	"lyon-agent/internal/mockapi"
	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/tools"
)

// newFileCardServer starts a lyond-shaped server whose workspace is a fresh
// git repo, against a mock gateway that will ask for one run_shell call.
func newFileCardServer(t *testing.T) (lyonv1connect.SessionServiceClient, string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash on PATH — the shell tool cannot run here")
	}
	scratchHome(t)
	repo, _ := newFileCardRepo(t)

	script := []mockapi.Turn{
		{Tool: &mockapi.ToolCall{
			Name:  "run_shell",
			Input: `{"command":"printf 'deck-v1\\n' > out-deck.md"}`,
		}},
		{Text: []string{"wrote the deck"}},
	}
	mock := mockapi.New(mockapi.Config{Script: script, Latency: time.Millisecond})
	t.Cleanup(mock.Close)
	t.Setenv("ANTHROPIC_BASE_URL", mock.URL())
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "filecard-e2e-token")

	cfg := agent.DefaultConfig()
	cfg.Workspace = repo
	eng := agent.NewEngine(cfg, nil)
	st := NewStore(0, 0)
	s := New(eng, st)
	s.SetObserver(tools.NewMetrics())
	// Close sessions BEFORE the TempDir cleanup (LIFO), or the async dump
	// writer races RemoveAll on Windows.
	t.Cleanup(st.CloseAll)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	return lyonv1connect.NewSessionServiceClient(http.DefaultClient, ts.URL), repo
}

// TestFileCardsOverTheWire: one prompt, one shell tool call that creates a
// file, and the card must come back on the event stream with that file in
// it — sorted, untracked, and sequenced above the prompt_end it annotates.
func TestFileCardsOverTheWire(t *testing.T) {
	client, repo := newFileCardServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cres, err := client.CreateSession(ctx, connect.NewRequest(&lyonv1.CreateSessionRequest{}))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	id := cres.Msg.Session.Id

	// Watch from -1 before submitting so the opening frames are not missed.
	wctx, wcancel := context.WithCancel(ctx)
	defer wcancel()
	events := make(chan *lyonv1.SessionEvent, 512)
	go func() {
		stream, serr := client.WatchEvents(wctx, connect.NewRequest(&lyonv1.WatchEventsRequest{
			SessionId: id, FromSeq: -1,
		}))
		if serr != nil {
			return
		}
		for stream.Receive() {
			events <- stream.Msg().Event
		}
	}()

	if _, err := client.SubmitPrompt(ctx, connect.NewRequest(&lyonv1.SubmitPromptRequest{
		SessionId: id,
		Prompt:    "write a file called out-deck.md via run_shell then say done",
	})); err != nil {
		t.Fatalf("SubmitPrompt: %v", err)
	}

	var (
		sawEnd   bool
		endSeq   int64
		cardEv   *lyonv1.SessionEvent
		toolRuns int
	)
	deadline := time.After(50 * time.Second)
	for cardEv == nil {
		select {
		case <-deadline:
			t.Fatalf("no files_changed on the wire (saw prompt_end=%v, tool events=%d)", sawEnd, toolRuns)
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("stream closed with no files_changed")
			}
			switch ev.TypeName {
			case "tool_start", "tool_end":
				toolRuns++
			case "prompt_end":
				sawEnd = true
				endSeq = ev.Seq
			case "files_changed":
				cardEv = ev
			}
		}
	}
	if !sawEnd {
		t.Fatalf("files_changed arrived with no prompt_end before it")
	}
	if cardEv.Seq <= endSeq {
		t.Fatalf("files_changed seq %d not above prompt_end seq %d", cardEv.Seq, endSeq)
	}

	var cards []fileCard
	raw := ""
	for k, v := range cardEv.Attrs {
		if k == "files" {
			raw = v
		}
	}
	if raw == "" {
		t.Fatalf("files_changed carried no attrs.files (attrs=%v)", cardEv.Attrs)
	}
	if err := json.Unmarshal([]byte(raw), &cards); err != nil {
		t.Fatalf("attrs.files is not the documented JSON rows: %v\n%s", err, raw)
	}
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want exactly the one file the run wrote", cards)
	}
	c := cards[0]
	if c.Path != "out-deck.md" {
		t.Fatalf("card path = %q, want out-deck.md", c.Path)
	}
	if c.Kind != "untracked" || c.XY != "??" {
		t.Fatalf("card = %+v, want an untracked ?? row (gitBadge reads this)", c)
	}
	if c.Staged {
		t.Fatalf("card = %+v, want staged=false for a new untracked file", c)
	}
	// The card is about the workspace the engine was configured with.
	if _, err := filepath.Abs(filepath.Join(repo, c.Path)); err != nil {
		t.Fatalf("card path unusable: %v", err)
	}
}

// TestFileCardsOverTheWireClassification: the badge fields have to survive
// the wire for a tracked file too — an untracked-only test would pass even
// if classifyChange were never reached. One run touches an existing tracked
// file (modified, unstaged) and stages a new one (added, staged).
func TestFileCardsOverTheWireClassification(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash on PATH — the shell tool cannot run here")
	}
	scratchHome(t)
	repo, run := newFileCardRepo(t)
	// A committed baseline file the run will then modify.
	if err := os.WriteFile(filepath.Join(repo, "notes.md"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "notes.md")
	run("commit", "-m", "seed")

	script := []mockapi.Turn{
		{Tool: &mockapi.ToolCall{
			Name:  "run_shell",
			Input: `{"command":"printf 'v2\n' > notes.md && printf 'x\n' > staged.md && git add staged.md"}`,
		}},
		{Text: []string{"updated and staged"}},
	}
	mock := mockapi.New(mockapi.Config{Script: script, Latency: time.Millisecond})
	t.Cleanup(mock.Close)
	t.Setenv("ANTHROPIC_BASE_URL", mock.URL())
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "filecard-e2e-token")

	cfg := agent.DefaultConfig()
	cfg.Workspace = repo
	st := NewStore(0, 0)
	s := New(agent.NewEngine(cfg, nil), st)
	s.SetObserver(tools.NewMetrics())
	t.Cleanup(st.CloseAll)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	cards := runPromptExpectCards(t, lyonv1connect.NewSessionServiceClient(http.DefaultClient, ts.URL),
		"modify notes.md and stage a new staged.md", 30*time.Second)
	if len(cards) != 2 {
		t.Fatalf("cards = %+v, want the two files the run touched", cards)
	}
	byPath := map[string]fileCard{}
	for _, c := range cards {
		byPath[c.Path] = c
	}
	mod, ok := byPath["notes.md"]
	if !ok {
		t.Fatalf("cards = %+v, want notes.md", cards)
	}
	if mod.Kind != "modified" || mod.Staged {
		t.Fatalf("notes.md card = %+v, want modified/unstaged", mod)
	}
	add, ok := byPath["staged.md"]
	if !ok {
		t.Fatalf("cards = %+v, want staged.md", cards)
	}
	if add.Kind != "added" || !add.Staged {
		t.Fatalf("staged.md card = %+v, want added/staged", add)
	}
}

// TestFileCardsOverTheWireNoChange: a run that writes nothing must stay
// quiet. An empty card would teach the reader to ignore the surface.
func TestFileCardsOverTheWireNoChange(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash on PATH — the shell tool cannot run here")
	}
	scratchHome(t)
	repo, _ := newFileCardRepo(t)

	script := []mockapi.Turn{
		{Tool: &mockapi.ToolCall{Name: "run_shell", Input: `{"command":"printf 'read only\n'"}`}},
		{Text: []string{"nothing to write"}},
	}
	mock := mockapi.New(mockapi.Config{Script: script, Latency: time.Millisecond})
	t.Cleanup(mock.Close)
	t.Setenv("ANTHROPIC_BASE_URL", mock.URL())
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "filecard-e2e-token")

	cfg := agent.DefaultConfig()
	cfg.Workspace = repo
	st := NewStore(0, 0)
	s := New(agent.NewEngine(cfg, nil), st)
	s.SetObserver(tools.NewMetrics())
	t.Cleanup(st.CloseAll)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	cards := runPromptExpectCards(t, lyonv1connect.NewSessionServiceClient(http.DefaultClient, ts.URL),
		"just report, do not write anything", 8*time.Second)
	if len(cards) != 0 {
		t.Fatalf("cards = %+v, want none for a run that wrote nothing", cards)
	}
}

// runPromptExpectCards drives one prompt end to end and returns whatever
// files_changed reported — empty means no card ever fired, which the
// no-change test asserts on purpose.
func runPromptExpectCards(t *testing.T, client lyonv1connect.SessionServiceClient, prompt string, expectBound time.Duration) []fileCard {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cres, err := client.CreateSession(ctx, connect.NewRequest(&lyonv1.CreateSessionRequest{}))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	id := cres.Msg.Session.Id

	wctx, wcancel := context.WithCancel(ctx)
	defer wcancel()
	events := make(chan *lyonv1.SessionEvent, 512)
	go func() {
		stream, serr := client.WatchEvents(wctx, connect.NewRequest(&lyonv1.WatchEventsRequest{
			SessionId: id, FromSeq: -1,
		}))
		if serr != nil {
			return
		}
		for stream.Receive() {
			events <- stream.Msg().Event
		}
	}()

	if _, err := client.SubmitPrompt(ctx, connect.NewRequest(&lyonv1.SubmitPromptRequest{
		SessionId: id, Prompt: prompt,
	})); err != nil {
		t.Fatalf("SubmitPrompt: %v", err)
	}

	// Settlement bound: the card's git pass is bounded by fileCardTimeout
	// (2s) plus one more capture, so a few seconds past prompt_end is the
	// honest ceiling. Reaching it means "no card for this run".
	deadline := time.After(expectBound)
	for {
		select {
		case <-deadline:
			return nil
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			if ev.TypeName != "files_changed" {
				continue
			}
			var cards []fileCard
			for k, v := range ev.Attrs {
				if k == "files" {
					if err := json.Unmarshal([]byte(v), &cards); err != nil {
						t.Fatalf("attrs.files is not the documented JSON rows: %v\n%s", err, v)
					}
				}
			}
			return cards
		}
	}
}
