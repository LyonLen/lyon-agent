package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"lyon-agent/internal/mockapi"
)

// drainUntilStop reads the session's event stream until it closes (or until
// stop is closed, whichever first) and returns everything collected.
func drainUntilStop(sess *Session, stop <-chan struct{}) []Event {
	var got []Event
	for {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				return got
			}
			got = append(got, ev)
		case <-stop:
			// Drain whatever is buffered without blocking forever.
			for {
				select {
				case ev, ok := <-sess.Events():
					if !ok {
						return got
					}
					got = append(got, ev)
				default:
					return got
				}
			}
		}
	}
}

// TestDispatchFanOutAttribution: one run_subagents call runs every
// subtask concurrently — each gets its own SubID token from
// EventSubagentStart to the matching EventSubagentEnd, the routed model
// rides the events, and the merged result keeps task order. This is the
// #53 leader-dispatch contract plus the 并发倾向 requirement: a serial
// fan-out would be the defect.
func TestDispatchFanOutAttribution(t *testing.T) {
	mock := mockapi.New(mockapi.Config{
		Script:  []mockapi.Turn{{Text: []string{"worker answer"}}}, // exhaustion repeats: all nested calls answer text
		Latency: 20 * time.Millisecond,                             // force real overlap between the two runs
	})
	t.Cleanup(mock.Close)
	t.Setenv("ANTHROPIC_BASE_URL", mock.URL())
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "dispatch-test-token")

	var mu sync.Mutex
	var routed []string
	cfg := Config{
		Model:         "main-x",
		SubagentModel: "sub-x",
		EventBuffer:   64,
		RouteFor: func(task string) string {
			mu.Lock()
			routed = append(routed, task)
			mu.Unlock()
			if strings.Contains(task, "alpha") {
				return "model-alpha" // evidence says alpha runs here
			}
			return "" // no evidence → subagent tier
		},
	}
	eng := NewEngine(cfg, nil)
	sess := eng.NewSession(nil)
	defer sess.Close()

	stop := make(chan struct{})
	var events []Event
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		events = drainUntilStop(sess, stop)
	}()

	input := json.RawMessage(`{"tasks":[
		{"task":"alpha research about tea history"},
		{"task":"beta survey about coffee varieties"}]}`)
	res, err := sess.dispatch(context.Background(), input)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	close(stop)
	wg.Wait()

	// Merged result keeps task order with both sections and the workers' answers.
	var text strings.Builder
	for _, b := range res {
		if b.OfText != nil {
			text.WriteString(b.OfText.Text)
		}
	}
	out := text.String()
	for _, want := range []string{"## 1/2 alpha research", "## 2/2 beta survey", "worker answer"} {
		if !strings.Contains(out, want) {
			t.Fatalf("merged result missing %q:\n%s", want, out)
		}
	}

	// RouteFor saw BOTH subtasks' texts (the leader's split reaches the router).
	mu.Lock()
	got := strings.Join(routed, "|")
	mu.Unlock()
	if !strings.Contains(got, "alpha research") || !strings.Contains(got, "beta survey") {
		t.Fatalf("RouteFor saw %q, want both task texts", got)
	}

	// Attribution: two SubStart tokens, two SubEnds pairing on the same
	// tokens, and the routed model on alpha's events.
	starts := map[string]string{} // subID → model
	ends := map[string]bool{}
	for _, ev := range events {
		switch ev.Type {
		case EventSubagentStart:
			if !ev.Sub || ev.SubID == "" {
				t.Fatalf("SubStart must carry Sub+SubID: %+v", ev)
			}
			if ev.Detail == "" {
				t.Fatalf("SubStart %q missing task preview", ev.SubID)
			}
			starts[ev.SubID] = ev.ToolName
		case EventSubagentEnd:
			if ev.SubID == "" {
				t.Fatalf("dispatch SubEnd missing SubID: %+v", ev)
			}
			if ev.IsError {
				t.Fatalf("SubEnd %q errored: %s", ev.SubID, ev.Err)
			}
			ends[ev.SubID] = true
		}
	}
	if len(starts) != 2 {
		t.Fatalf("SubStart tokens = %d (%v), want 2 distinct", len(starts), starts)
	}
	if len(ends) != 2 {
		t.Fatalf("SubEnd tokens = %d (%v), want 2", len(ends), ends)
	}
	for id := range starts {
		if !ends[id] {
			t.Fatalf("no SubEnd for token %q", id)
		}
	}
	// alpha's token carried the routed model; beta's stayed on the tier.
	for _, model := range starts {
		if model == "model-alpha" {
			return // routed model observed on the events
		}
	}
	t.Fatalf("no event carried routed model model-alpha (starts=%v)", starts)
}

// TestDispatchRoutedModelReachesAPI: a single-task fan-out lets us pin
// the request body — the RouteFor pick is the model the nested loop
// actually bills, not just a badge on the events.
func TestDispatchRoutedModelReachesAPI(t *testing.T) {
	mock := mockapi.New(mockapi.Config{Script: []mockapi.Turn{{Text: []string{"solo worker"}}}})
	t.Cleanup(mock.Close)
	t.Setenv("ANTHROPIC_BASE_URL", mock.URL())
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "dispatch-test-token")

	cfg := Config{
		Model:         "main-x",
		SubagentModel: "sub-x",
		EventBuffer:   64,
		RouteFor: func(string) string {
			return "model-solo"
		},
	}
	eng := NewEngine(cfg, nil)
	sess := eng.NewSession(nil)
	defer sess.Close()

	if _, err := sess.dispatch(context.Background(),
		json.RawMessage(`{"tasks":[{"task":"one task"}]}`)); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	var body struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(mock.LastBody(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Model != "model-solo" {
		t.Fatalf("billed model = %q, want model-solo", body.Model)
	}
}

// TestDispatchRejectsBadInput: the fan-out cap and the empty-task guard —
// one tool call can never open an unbounded storm of API loops.
func TestDispatchRejectsBadInput(t *testing.T) {
	eng := NewEngine(Config{EventBuffer: 64}, nil)
	sess := eng.NewSession(nil)
	defer sess.Close()

	var big strings.Builder
	big.WriteString(`{"tasks":[`)
	for i := 0; i < maxDispatchTasks+1; i++ {
		if i > 0 {
			big.WriteString(",")
		}
		big.WriteString(`{"task":"t"}`)
	}
	big.WriteString(`]}`)
	if _, err := sess.dispatch(context.Background(), json.RawMessage(big.String())); err == nil {
		t.Fatal("over-cap fan-out must be rejected")
	}
	if _, err := sess.dispatch(context.Background(),
		json.RawMessage(`{"tasks":[{"task":"  "}]}`)); err == nil {
		t.Fatal("blank task must be rejected")
	}
	if _, err := sess.dispatch(context.Background(),
		json.RawMessage(`{"tasks":[]}`)); err == nil {
		t.Fatal("empty fan-out must be rejected")
	}
}

// TestDispatchAllFailedCarriesDiagnosis: when every subtask of a fan-out
// dies (here: all silenced past the injected idle budget), the merged
// error must fold in each task's cause — "all N failed" with no reason is
// undiagnosable, which is how the plan-mode triple timeout read on the
// surface. Regress the watchdog diagnosis through the dispatch path too.
func TestDispatchAllFailedCarriesDiagnosis(t *testing.T) {
	mock := mockapi.New(mockapi.Config{
		Script:  []mockapi.Turn{{Text: []string{"never arrives"}}},
		Latency: 3 * time.Second, // beyond the 300ms idle budget (2×150ms)
	})
	t.Cleanup(mock.Close)
	t.Setenv("ANTHROPIC_BASE_URL", mock.URL())
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "dispatch-timeout-test-token")

	sess := subTimeoutSession(t, subTimeoutConfig(t, 150*time.Millisecond), nil)

	_, err := sess.dispatch(context.Background(), json.RawMessage(`{"tasks":[
		{"task":"survey the tea archives"},
		{"task":"compare coffee brewing methods"}]}`))
	if err == nil {
		t.Fatal("a fan-out silenced past the idle budget must fail")
	}
	msg := err.Error()
	for _, want := range []string{
		"all 2 subtasks failed", // the aggregate
		"no progress",           // each task's watchdog diagnosis
		"tea archives",          // first subtask's name and cause
		"coffee brewing",        // second subtask's name and cause
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("fan-out error missing %q:\n%v", want, msg)
		}
	}
}

// TestAskModelRidesRequestedModel: the contest grades every candidate on
// ITS model — AskModel must put that model on the wire, and Ask keeps
// defaulting to the subagent tier.
func TestAskModelRidesRequestedModel(t *testing.T) {
	mock := mockapi.New(mockapi.Config{Script: []mockapi.Turn{{Text: []string{"the answer"}}}})
	t.Cleanup(mock.Close)
	t.Setenv("ANTHROPIC_BASE_URL", mock.URL())
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "askmodel-test-token")

	eng := NewEngine(Config{Model: "main-x", SubagentModel: "sub-fixed", EventBuffer: 64}, nil)
	sess := eng.NewSession(nil)
	defer sess.Close()

	sawModel := func(want string) {
		t.Helper()
		var body struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(mock.LastBody(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Model != want {
			t.Fatalf("billed model = %q, want %q", body.Model, want)
		}
	}

	if _, err := sess.AskModel(context.Background(), "candidate-z", "sys", "q", 100); err != nil {
		t.Fatal(err)
	}
	sawModel("candidate-z")

	if _, err := sess.Ask(context.Background(), "sys", "q", 100); err != nil {
		t.Fatal(err)
	}
	sawModel("sub-fixed")
}

// TestSubmitOnOverridesModelForOnePrompt: the TUI's routing consumption —
// one prompt rides the recommended model, the next unpinned prompt falls
// back to the session's config model.
func TestSubmitOnOverridesModelForOnePrompt(t *testing.T) {
	mock := mockapi.New(mockapi.Config{Script: []mockapi.Turn{{Text: []string{"ok"}}}})
	t.Cleanup(mock.Close)
	t.Setenv("ANTHROPIC_BASE_URL", mock.URL())
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "submiton-test-token")

	eng := NewEngine(Config{Model: "main-fixed", SubagentModel: "sub-x", EventBuffer: 64}, nil)
	sess := eng.NewSession(nil)
	defer sess.Close()

	billedModel := func() string {
		var body struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(mock.LastBody(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Model
	}

	if err := sess.SubmitOn("route me", "routed-y"); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, sess, EventPromptEnd)
	if got := billedModel(); got != "routed-y" {
		t.Fatalf("routed prompt billed %q, want routed-y", got)
	}

	if err := sess.Submit("plain prompt"); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, sess, EventPromptEnd)
	if got := billedModel(); got != "main-fixed" {
		t.Fatalf("plain prompt billed %q, want main-fixed", got)
	}
}
