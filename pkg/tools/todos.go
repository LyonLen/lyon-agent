package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// todoMaxItems caps one plan — enough for a real breakdown, small enough
// that the rendered block stays readable.
const todoMaxItems = 20

// TodoWrite is the readable step-feedback tool (#66): the model writes
// its subtask breakdown as a plan of pending / in_progress / completed
// items and keeps it current, so the user watches semantic progress
// instead of a wall of exec rows. The tool is stateless — it validates
// and acknowledges; renderers derive the visible plan from the event
// input, which always carries the COMPLETE list.
type TodoWrite struct{}

func (t *TodoWrite) Name() string { return "todo_write" }

func (t *TodoWrite) Description() string {
	return "Replace the visible task plan — the complete list of subtasks, each with status pending / in_progress / completed. " +
		"Use it for any multi-step task: once the request is clear, write the breakdown BEFORE starting the work; " +
		"then rewrite the whole list after finishing each subtask (mark it completed, set exactly the next one to in_progress). " +
		"The user sees this plan as the live progress display; when the task ends, every item should read completed."
}

// ParallelSafe: the call mutates only the session's plan display — no
// files, no shell — so it batches with reads and passes plan mode.
func (t *TodoWrite) ParallelSafe() bool { return true }

type todoItemInput struct {
	Content string `json:"content" jsonschema:"required,description=One subtask, phrased as a short action (e.g. \"wire snapshot round-trip\")"`
	Status  string `json:"status" jsonschema:"required,description=pending | in_progress | completed — exactly one item is in_progress while work is underway"`
}

type todoWriteInput struct {
	Todos []todoItemInput `json:"todos" jsonschema:"required,description=The COMPLETE plan, replacing the previous one"`
}

func (t *TodoWrite) InputSchema() anthropic.BetaToolInputSchemaParam {
	return schemaFor[todoWriteInput]()
}

// Execute validates the whole list (the display treats it as the truth,
// so a malformed plan must surface as a retryable error, not half-render)
// and acks with the new progress scale — the model reads its own plan
// state back before the next step.
func (t *TodoWrite) Execute(ctx context.Context, input json.RawMessage) ([]anthropic.BetaToolResultBlockParamContentUnion, error) {
	var in todoWriteInput
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, fmt.Errorf("todo_write: %w", err)
	}
	if len(in.Todos) == 0 {
		return nil, fmt.Errorf("todo_write: empty plan — send the full list, at least one item")
	}
	if len(in.Todos) > todoMaxItems {
		return nil, fmt.Errorf("todo_write: %d items exceeds the %d-item cap", len(in.Todos), todoMaxItems)
	}
	var done, total int
	current := ""
	for i, it := range in.Todos {
		content := strings.TrimSpace(it.Content)
		if content == "" {
			return nil, fmt.Errorf("todo_write: item %d has empty content", i+1)
		}
		switch it.Status {
		case "pending", "in_progress", "completed":
		default:
			return nil, fmt.Errorf("todo_write: item %d has invalid status %q (want pending|in_progress|completed)", i+1, it.Status)
		}
		if it.Status == "in_progress" && current == "" {
			current = content
		}
		if it.Status == "completed" {
			done++
		}
		total++
	}
	ack := fmt.Sprintf("plan updated: %d/%d completed", done, total)
	if current != "" {
		ack += fmt.Sprintf(" · in progress: %q", current)
	}
	return TextResult(ack), nil
}
