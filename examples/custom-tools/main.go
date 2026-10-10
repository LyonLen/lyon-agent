// Command custom-tools demonstrates embedding lyon as a library with
// custom business tools registered from plain Go functions.
//
//	go run ./examples/custom-tools
//
// It registers two typed tools (read-only, parallel-safe), runs one prompt
// through a live session, and prints the event stream plus tool metrics.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"lyon-agent/pkg/agent"
	"lyon-agent/pkg/tools"
)

// quoteInput is the typed input of the get_quote tool; its jsonschema tags
// become the JSON schema the model validates against.
type quoteInput struct {
	Topic string `json:"topic" jsonschema:"topic to find a quote about"`
}

type wordCountInput struct {
	Text string `json:"text" jsonschema:"text to count words in"`
}

func main() {
	reg := tools.NewRegistry()

	// A canned business tool: read-only, safe to run concurrently with
	// other tools in the same turn.
	err := tools.RegisterFunc(reg, "get_quote",
		"Return a famous quote about the given topic.",
		true,
		func(ctx context.Context, in quoteInput) (string, error) {
			quotes := map[string]string{
				"courage": "Courage is grace under pressure. — Ernest Hemingway",
				"speed":   "Speed is the only currency that never devalues. — anon",
			}
			q, ok := quotes[strings.ToLower(in.Topic)]
			if !ok {
				return "", fmt.Errorf("no quote about %q", in.Topic)
			}
			return q, nil
		})
	if err != nil {
		fatal(err)
	}

	// A second custom tool, also parallel-safe.
	err = tools.RegisterFunc(reg, "word_count",
		"Count the words in the given text.",
		true,
		func(ctx context.Context, in wordCountInput) (string, error) {
			n := len(strings.Fields(in.Text))
			return fmt.Sprintf("%d words", n), nil
		})
	if err != nil {
		fatal(err)
	}

	eng := agent.NewEngine(agent.DefaultConfig(), reg)
	fmt.Println("custom tools:", strings.Join(eng.Registry().Names(), ", "))

	metrics := tools.NewMetrics()
	sess := eng.NewSession(metrics)
	defer sess.Close()

	prompt := "Use get_quote for the topic \"courage\", then use word_count on " +
		"the phrase \"high performance agents\", and report both results."
	if len(os.Args) > 1 {
		prompt = strings.Join(os.Args[1:], " ")
	}
	if err := sess.Submit(prompt); err != nil {
		fatal(err)
	}

	start := time.Now()
	done := false
	for ev := range sess.Events() {
		switch ev.Type {
		case agent.EventToolExecStart:
			fmt.Printf("▸ exec #%d %s %s\n", ev.ExecID, ev.ToolName, ev.Input)
		case agent.EventToolExecEnd:
			if ev.IsError {
				fmt.Printf("✘ done #%d %s (%dms): %s\n", ev.ExecID, ev.ToolName, ev.DurationMS, ev.Err)
			} else {
				fmt.Printf("✔ done #%d %s (%dms)\n", ev.ExecID, ev.ToolName, ev.DurationMS)
			}
		case agent.EventTurnEnd:
			u := ev.Usage
			fmt.Printf("[turn %d] in=%d out=%d cache_read=%d stop=%s (%dms)\n",
				ev.Turn, u.Input, u.Output, u.CacheRead, ev.StopReason, ev.DurationMS)
		case agent.EventTextDelta:
			os.Stdout.WriteString(ev.Text)
		case agent.EventError:
			fmt.Fprintln(os.Stderr, "error:", ev.Err)
		case agent.EventPromptEnd:
			if ev.Err != "" {
				fmt.Fprintln(os.Stderr, "prompt failed:", ev.Err)
				os.Exit(1)
			}
			fmt.Printf("\n✓ done in %s (%d API calls)\n",
				time.Since(start).Round(time.Millisecond), ev.Iterations)
			// The events channel stays open until Close(); stop here.
			done = true
		}
		if done {
			break
		}
	}

	for _, m := range metrics.Snapshot() {
		fmt.Printf("metric %s: calls=%d errors=%d avg=%s p95=%s\n",
			m.Name, m.Calls, m.Errors, m.Avg().Round(time.Millisecond), m.P95.Round(time.Millisecond))
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
