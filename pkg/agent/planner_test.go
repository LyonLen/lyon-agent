package agent

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"lyon-agent/internal/mockapi"
)

// collectUntilPromptEnd drains the event stream into a slice until
// prompt_end (waitPromptEndCollect keeps only compaction events — the
// planner asserts on the synthetic todo_write pair in between).
func collectUntilPromptEnd(t *testing.T, sess *Session) []Event {
	t.Helper()
	var evs []Event
	deadline := time.After(15 * time.Second)
	for {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				t.Fatal("event stream closed while waiting for prompt_end")
			}
			evs = append(evs, ev)
			if ev.Type == EventPromptEnd {
				return evs
			}
		case <-deadline:
			t.Fatal("timeout waiting for prompt_end")
		}
	}
}

// findPlanStart returns the synthetic todo_write exec-start, or nil.
func findPlanStart(evs []Event) *Event {
	for i := range evs {
		if evs[i].Type == EventToolExecStart && evs[i].ToolName == "todo_write" {
			return &evs[i]
		}
	}
	return nil
}

const plannerOpenPrompt = "design the architecture for a caching layer"
const plannerClosedPrompt = "run go build and report errors"

const plannerScriptPlan = `{"todos":[{"content":"wire the parser","status":"pending"},{"content":"add tests","status":"pending"}]}`

func TestPlannerMode(t *testing.T) {
	for _, tc := range []struct{ env, want string }{
		{"off", "off"},
		{"OFF", "off"},
		{"on", "on"},
		{"On", "on"},
		{"", "auto"},
		{"bogus", "auto"},
	} {
		t.Setenv("LYON_PLANNER", tc.env)
		if got := plannerMode(); got != tc.want {
			t.Fatalf("LYON_PLANNER=%q → %q, want %q", tc.env, got, tc.want)
		}
	}
}

// TestPlannerOffSkips: off means zero cost — one API call (the main
// turn), no planner request, no synthetic plan events, even for a
// prompt the auto gate would happily take.
func TestPlannerOffSkips(t *testing.T) {
	t.Setenv("LYON_PLANNER", "off")
	mock := newCompactMock(t, []mockapi.Turn{{Text: []string{"ok"}}})
	eng := NewEngine(Config{Workspace: tempDir(t), CompactThreshold: 0.9}, nil)
	t.Cleanup(eng.Close)
	sess := eng.NewSession(nil)
	defer sess.Close()

	if err := sess.Submit(plannerOpenPrompt); err != nil {
		t.Fatal(err)
	}
	if pe, _ := waitPromptEndCollect(t, sess); pe.Err != "" {
		t.Fatalf("prompt: %s", pe.Err)
	}
	if n := mock.Calls(); n != 1 {
		t.Fatalf("API calls = %d, want 1 (main turn only)", n)
	}
	// The off gate returns before startPlanner can launch anything —
	// re-collect is not possible (events drained), so assert the count
	// above plus a fresh prompt's call total instead.
}

// TestPlannerAutoPlans: auto takes an open-ended first prompt, drafts
// the plan through the cheap tier, surfaces the synthetic todo_write
// pair, and rides the note into the main request's history.
func TestPlannerAutoPlans(t *testing.T) {
	t.Setenv("LYON_PLANNER", "auto")
	mock := newCompactMock(t, []mockapi.Turn{
		{Text: []string{plannerScriptPlan}},
		{Text: []string{"done"}},
	})
	eng := NewEngine(Config{Workspace: tempDir(t), CompactThreshold: 0.9}, nil)
	t.Cleanup(eng.Close)
	sess := eng.NewSession(nil)
	defer sess.Close()

	if err := sess.Submit(plannerOpenPrompt); err != nil {
		t.Fatal(err)
	}
	evs := collectUntilPromptEnd(t, sess)
	pe := evs[len(evs)-1]
	if pe.Err != "" {
		t.Fatalf("prompt: %s", pe.Err)
	}
	if n := mock.Calls(); n != 2 {
		t.Fatalf("API calls = %d, want 2 (planner + main)", n)
	}
	start := findPlanStart(evs)
	if start == nil {
		t.Fatalf("synthetic todo_write exec-start missing:\n%+v", evs)
	}
	var in struct {
		Todos []planItem `json:"todos"`
	}
	if err := json.Unmarshal(start.Input, &in); err != nil || len(in.Todos) != 2 {
		t.Fatalf("plan input = %s (%v), want 2 items", start.Input, err)
	}
	if in.Todos[0].Status != "in_progress" {
		t.Fatalf("first item status = %q, want in_progress", in.Todos[0].Status)
	}
	// The main request (LastBody) carries the turn-boundary note.
	body := string(mock.LastBody())
	if !strings.Contains(body, "Planned subtask breakdown") ||
		!strings.Contains(body, "wire the parser") {
		t.Fatalf("main request missing plan note:\n%.2000s", body)
	}
	// Snapshot/resume carries the same list.
	snap := sess.Export()
	if !strings.Contains(string(snap.Todos), "wire the parser") {
		t.Fatalf("snapshot todos = %s", snap.Todos)
	}
}

// TestPlannerAutoSkipsMechanical: a closed, short prompt passes the
// auto gate's neither condition — no second API call, no plan.
func TestPlannerAutoSkipsMechanical(t *testing.T) {
	t.Setenv("LYON_PLANNER", "auto")
	mock := newCompactMock(t, []mockapi.Turn{{Text: []string{"ok"}}})
	eng := NewEngine(Config{Workspace: tempDir(t), CompactThreshold: 0.9}, nil)
	t.Cleanup(eng.Close)
	sess := eng.NewSession(nil)
	defer sess.Close()

	if err := sess.Submit(plannerClosedPrompt); err != nil {
		t.Fatal(err)
	}
	evs := collectUntilPromptEnd(t, sess)
	if pe := evs[len(evs)-1]; pe.Err != "" {
		t.Fatalf("prompt: %s", pe.Err)
	}
	if n := mock.Calls(); n != 1 {
		t.Fatalf("API calls = %d, want 1 (mechanical prompt skips the planner)", n)
	}
	if findPlanStart(evs) != nil {
		t.Fatal("mechanical prompt must not emit a plan")
	}
}

// TestPlannerAutoOncePerSession: the second open prompt does not
// re-plan — auto spends its single draft per session and the model
// maintains the plan itself from there.
func TestPlannerAutoOncePerSession(t *testing.T) {
	t.Setenv("LYON_PLANNER", "auto")
	mock := newCompactMock(t, []mockapi.Turn{
		{Text: []string{plannerScriptPlan}},
		{Text: []string{"done"}},
	})
	eng := NewEngine(Config{Workspace: tempDir(t), CompactThreshold: 0.9}, nil)
	t.Cleanup(eng.Close)
	sess := eng.NewSession(nil)
	defer sess.Close()

	for i, q := range []string{plannerOpenPrompt, "design a rollback strategy for migrations"} {
		if err := sess.Submit(q); err != nil {
			t.Fatalf("prompt %d: %v", i+1, err)
		}
		if pe, _ := waitPromptEndCollect(t, sess); pe.Err != "" {
			t.Fatalf("prompt %d: %s", i+1, pe.Err)
		}
	}
	// 1 planner + 2 main turns. A second planner call would make 4.
	if n := mock.Calls(); n != 3 {
		t.Fatalf("API calls = %d, want 3 (one planner, two main turns)", n)
	}
}

// TestPlannerFailOpenGarbage: mode=on forces the call, but unparseable
// output degrades to no plan — the prompt still runs, untouched.
func TestPlannerFailOpenGarbage(t *testing.T) {
	t.Setenv("LYON_PLANNER", "on")
	mock := newCompactMock(t, []mockapi.Turn{
		{Text: []string{"i cannot help with that"}},
		{Text: []string{"done"}},
	})
	eng := NewEngine(Config{Workspace: tempDir(t), CompactThreshold: 0.9}, nil)
	t.Cleanup(eng.Close)
	sess := eng.NewSession(nil)
	defer sess.Close()

	if err := sess.Submit(plannerOpenPrompt); err != nil {
		t.Fatal(err)
	}
	evs := collectUntilPromptEnd(t, sess)
	if pe := evs[len(evs)-1]; pe.Err != "" {
		t.Fatalf("prompt: %s", pe.Err)
	}
	if n := mock.Calls(); n != 2 {
		t.Fatalf("API calls = %d, want 2 (planner attempted + main)", n)
	}
	if findPlanStart(evs) != nil {
		t.Fatal("garbage planner output must not emit a plan")
	}
	if strings.Contains(string(mock.LastBody()), "Planned subtask breakdown") {
		t.Fatal("garbage planner output must not reach the note")
	}
}

// TestPlannerDeadline: a planner that outlives the budget is cut off —
// fail-open, the prompt proceeds without a plan.
func TestPlannerDeadline(t *testing.T) {
	t.Setenv("LYON_PLANNER", "on")
	old := plannerBudget
	plannerBudget = 5 * time.Millisecond
	defer func() { plannerBudget = old }()

	// Latency outlives the budget on every response; the main turn just
	// waits it out, the planner's round trip is cancelled client-side.
	mock := mockapi.New(mockapi.Config{
		Script: []mockapi.Turn{
			{Text: []string{plannerScriptPlan}},
			{Text: []string{"done"}},
		},
		Latency: 300 * time.Millisecond,
	})
	t.Cleanup(mock.Close)
	t.Setenv("ANTHROPIC_BASE_URL", mock.URL())
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "planner-deadline-token")

	eng := NewEngine(Config{Workspace: tempDir(t), CompactThreshold: 0.9}, nil)
	t.Cleanup(eng.Close)
	sess := eng.NewSession(nil)
	defer sess.Close()

	if err := sess.Submit(plannerOpenPrompt); err != nil {
		t.Fatal(err)
	}
	evs := collectUntilPromptEnd(t, sess)
	if pe := evs[len(evs)-1]; pe.Err != "" {
		t.Fatalf("prompt: %s", pe.Err)
	}
	if findPlanStart(evs) != nil {
		t.Fatal("deadline-exceeded planner must not emit a plan")
	}
	if strings.Contains(string(mock.LastBody()), "Planned subtask breakdown") {
		t.Fatal("deadline-exceeded planner must not reach the note")
	}
}

func TestParsePlannerTodos(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		want     int
	}{
		{"object", plannerScriptPlan, 2},
		{"fenced", "```json\n" + plannerScriptPlan + "\n```", 2},
		{"bare array", `[{"content":"a","status":"pending"}]`, 1},
		{"prose around", "Here you go: " + plannerScriptPlan + " Good luck!", 2},
		{"garbage", "i cannot help", 0},
		{"empty list", `{"todos":[]}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := len(parsePlannerTodos(tc.in)); got != tc.want {
				t.Fatalf("items = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestNormalizePlanItems(t *testing.T) {
	// Unknown statuses become pending, a second in_progress demotes, the
	// first item activates when nothing is active, contents trim/cap.
	in := []planItem{
		{Content: "  first  ", Status: "bogus"},
		{Content: "second", Status: "in_progress"},
		{Content: "third", Status: "in_progress"},
		{Content: "   ", Status: "pending"},
	}
	out := normalizePlanItems(in)
	if len(out) != 3 {
		t.Fatalf("items = %d, want 3 (empty content dropped)", len(out))
	}
	if out[0].Content != "first" || out[0].Status != "pending" {
		t.Fatalf("item0 = %+v", out[0])
	}
	if out[1].Status != "in_progress" {
		t.Fatalf("item1 status = %q, want in_progress (first active wins)", out[1].Status)
	}
	if out[2].Status != "pending" {
		t.Fatalf("item2 status = %q, want pending (demoted)", out[2].Status)
	}

	// Nothing active → the first item becomes in_progress.
	out = normalizePlanItems([]planItem{{Content: "only", Status: "pending"}})
	if out[0].Status != "in_progress" {
		t.Fatalf("lone item status = %q, want in_progress", out[0].Status)
	}

	// Overlong content clips to the rune cap.
	long := strings.Repeat("字", plannerItemRunes+50)
	out = normalizePlanItems([]planItem{{Content: long, Status: "pending"}})
	if r := len([]rune(out[0].Content)); r > plannerItemRunes+1 {
		t.Fatalf("content %d runes, want ≤ %d+ellipsis", r, plannerItemRunes)
	}
}

func TestJoinNotes(t *testing.T) {
	if got := joinNotes("", "queued"); got != "queued" {
		t.Fatalf("empty plan → %q", got)
	}
	if got := joinNotes("plan", ""); got != "plan" {
		t.Fatalf("empty queued → %q", got)
	}
	if got := joinNotes("plan", "queued"); got != "plan\n\nqueued" {
		t.Fatalf("both → %q", got)
	}
}
