package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestTodoWriteExecute: a valid plan acks with the progress scale and the
// in-progress item — the model reads its own plan state back.
func TestTodoWriteExecute(t *testing.T) {
	tw := &TodoWrite{}
	out, err := tw.Execute(context.Background(), []byte(`{"todos":[
		{"content":"read the spec","status":"completed"},
		{"content":"wire the tool","status":"in_progress"},
		{"content":"write tests","status":"pending"},
		{"content":"rebuild binaries","status":"pending"},
		{"content":"run the suite","status":"pending"}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	text := out[0].OfText.Text
	if !strings.Contains(text, "1/5") {
		t.Fatalf("ack = %q, want 1/5 scale", text)
	}
	if !strings.Contains(text, "wire the tool") {
		t.Fatalf("ack = %q, want the in-progress item", text)
	}
}

// TestTodoWriteValidation: a malformed plan is a retryable error, never
// half-accepted — the display would treat it as the truth.
func TestTodoWriteValidation(t *testing.T) {
	over := make([]todoItemInput, todoMaxItems+1)
	for i := range over {
		over[i] = todoItemInput{Content: "item", Status: "pending"}
	}
	overJSON, err := json.Marshal(todoWriteInput{Todos: over})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", `{"todos":[]}`, "empty plan"},
		{"bad status", `{"todos":[{"content":"x","status":"doing"}]}`, "invalid status"},
		{"empty content", `{"todos":[{"content":"  ","status":"pending"}]}`, "empty content"},
		{"not json", `todos`, "todo_write"},
		{"over cap", string(overJSON), "exceeds"},
	}
	tw := &TodoWrite{}
	for _, c := range cases {
		if _, err := tw.Execute(context.Background(), []byte(c.in)); err == nil ||
			!strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

// TestTodoWriteSchema: the enum and required fields ride into the schema
// so the model sees the contract before calling.
func TestTodoWriteSchema(t *testing.T) {
	tw := &TodoWrite{}
	raw, err := json.Marshal(tw.InputSchema())
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, "in_progress") || !strings.Contains(s, "todos") {
		t.Fatalf("schema missing plan contract: %s", s)
	}
}
