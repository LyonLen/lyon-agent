package agent

import "testing"

// TestBuiltinTodoWrite: the plan tool ships with the default tool set —
// every engine session can publish readable step feedback (#66).
func TestBuiltinTodoWrite(t *testing.T) {
	eng := NewEngine(DefaultConfig(), nil)
	if _, ok := eng.reg.Get("todo_write"); !ok {
		t.Fatal("todo_write not registered as a builtin")
	}
}

// TestTodosRideSnapshot: the plan rides SetTodos → Export, comes back
// through Restore, and Reset clears it — same lifecycle as Steps (#66).
func TestTodosRideSnapshot(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Workspace = tempDir(t)
	eng := NewEngine(cfg, nil)
	sess := eng.NewSession(nil)
	defer sess.Close()

	plan := `[{"content":"read the spec","status":"completed"},{"content":"wire the tool","status":"in_progress"}]`
	sess.SetTodos([]byte(plan))
	if got := string(sess.Export().Todos); got != plan {
		t.Fatalf("Export lost todos: %q", got)
	}

	sess2 := eng.NewSession(nil)
	defer sess2.Close()
	sess2.Restore(sess.Export())
	if got := string(sess2.Export().Todos); got != plan {
		t.Fatalf("Restore dropped todos: %q", got)
	}

	sess2.Reset()
	if len(sess2.Export().Todos) != 0 {
		t.Fatalf("Reset kept todos: %q", sess2.Export().Todos)
	}
}
