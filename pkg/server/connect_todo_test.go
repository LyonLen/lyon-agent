package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	lyonv1 "lyon-agent/gen/go/lyon/v1"
	"lyon-agent/gen/go/lyon/v1/lyonv1connect"
	"lyon-agent/pkg/agent"
)

// newTodoFixture serves a real server on a scratch LYON_HOME — the todo
// service only touches disk, so no engine traffic is needed.
func newTodoFixture(t *testing.T) (lyonv1connect.TodoServiceClient, string) {
	t.Helper()
	home := scratchHome(t)
	eng := agent.NewEngine(agent.DefaultConfig(), nil)
	s := New(eng, NewStore(0, 0))
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return lyonv1connect.NewTodoServiceClient(http.DefaultClient, ts.URL), home
}

func seedTodoDoc() *lyonv1.TodoDoc {
	return &lyonv1.TodoDoc{
		Goals: []*lyonv1.TodoGoal{
			{Id: "g_a", Title: "整体目标一", Color: "#0b3d91"},
			{Id: "g_b", Title: "整体目标二", Color: ""},
		},
		Todos: []*lyonv1.TodoItem{
			{
				Id: "t_1", Content: "排期任务", Status: "in_progress",
				Start: "2026-11-16", End: "2026-11-20",
				GoalId: "g_a", SessionIds: []string{"s1", "s2"}, Note: "备注",
			},
			{
				Id: "t_2", Content: "未排期任务", Status: "pending",
				GoalId: "", SessionIds: nil,
			},
		},
	}
}

// TestConnectTodoRoundTrip: empty list answers an empty document without
// creating the file; a save round-trips byte-for-byte through proto.Equal
// and lands on disk; a second list reads back the same document.
func TestConnectTodoRoundTrip(t *testing.T) {
	client, home := newTodoFixture(t)
	ctx := context.Background()

	got, err := client.ListTodos(ctx, &connect.Request[lyonv1.ListTodosRequest]{})
	if err != nil {
		t.Fatalf("ListTodos(empty): %v", err)
	}
	if len(got.Msg.GetDoc().GetGoals()) != 0 || len(got.Msg.GetDoc().GetTodos()) != 0 {
		t.Fatalf("fresh list = %+v, want empty doc", got.Msg.GetDoc())
	}
	if _, err := os.Stat(filepath.Join(home, todosFileName)); !os.IsNotExist(err) {
		t.Fatalf("fresh list must not create %s (stat err=%v)", todosFileName, err)
	}

	sent := seedTodoDoc()
	saved, err := client.SaveTodos(ctx, &connect.Request[lyonv1.SaveTodosRequest]{Msg: &lyonv1.SaveTodosRequest{Doc: sent}})
	if err != nil {
		t.Fatalf("SaveTodos: %v", err)
	}
	if !proto.Equal(saved.Msg.GetDoc(), sent) {
		t.Fatalf("save response = %v, want canonical %v", saved.Msg.GetDoc(), sent)
	}

	raw, err := os.ReadFile(filepath.Join(home, todosFileName))
	if err != nil {
		t.Fatalf("read todos.json: %v", err)
	}
	var disk lyonv1.TodoDoc
	if err := json.Unmarshal(raw, &disk); err != nil {
		t.Fatalf("disk json: %v", err)
	}
	if !proto.Equal(&disk, sent) {
		t.Fatalf("disk doc = %v, want %v", &disk, sent)
	}

	again, err := client.ListTodos(ctx, &connect.Request[lyonv1.ListTodosRequest]{})
	if err != nil {
		t.Fatalf("ListTodos(after save): %v", err)
	}
	if !proto.Equal(again.Msg.GetDoc(), sent) {
		t.Fatalf("list after save = %v, want %v", again.Msg.GetDoc(), sent)
	}
}

// TestConnectTodoValidation: the hard-error contract — empty content,
// unknown status, malformed/impossible dates and count caps all answer
// InvalidArgument; a corrupt on-disk file answers Internal with a path
// hint (Save overwrites it as the recovery path).
func TestConnectTodoValidation(t *testing.T) {
	client, home := newTodoFixture(t)
	ctx := context.Background()

	base := func(mutate func(*lyonv1.TodoItem)) *lyonv1.TodoDoc {
		doc := seedTodoDoc()
		mutate(doc.Todos[0])
		return doc
	}
	cases := []struct {
		name string
		doc  *lyonv1.TodoDoc
	}{
		{"empty content", base(func(i *lyonv1.TodoItem) { i.Content = "   " })},
		{"bad status", base(func(i *lyonv1.TodoItem) { i.Status = "done" })},
		{"empty status", base(func(i *lyonv1.TodoItem) { i.Status = "" })},
		{"malformed date", base(func(i *lyonv1.TodoItem) { i.Start = "11-16-2026" })},
		{"impossible date", base(func(i *lyonv1.TodoItem) { i.Start = "2026-13-45" })},
		{"impossible day", base(func(i *lyonv1.TodoItem) { i.Start = "2026-02-30" })},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.SaveTodos(ctx, &connect.Request[lyonv1.SaveTodosRequest]{Msg: &lyonv1.SaveTodosRequest{Doc: tc.doc}})
			if got := connectCode(err); got != connect.CodeInvalidArgument {
				t.Fatalf("code = %v, want InvalidArgument (err=%v)", got, err)
			}
		})
	}

	t.Run("too many goals", func(t *testing.T) {
		doc := &lyonv1.TodoDoc{}
		for i := 0; i <= todoMaxGoals; i++ {
			doc.Goals = append(doc.Goals, &lyonv1.TodoGoal{Id: string(rune('a' + i%26)), Title: "g"})
		}
		_, err := client.SaveTodos(ctx, &connect.Request[lyonv1.SaveTodosRequest]{Msg: &lyonv1.SaveTodosRequest{Doc: doc}})
		if got := connectCode(err); got != connect.CodeInvalidArgument {
			t.Fatalf("code = %v, want InvalidArgument (err=%v)", got, err)
		}
	})

	t.Run("corrupt file", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(home, todosFileName), []byte("{not json"), 0o600); err != nil {
			t.Fatalf("seed corrupt: %v", err)
		}
		_, err := client.ListTodos(ctx, &connect.Request[lyonv1.ListTodosRequest]{})
		if got := connectCode(err); got != connect.CodeInternal {
			t.Fatalf("code = %v, want Internal (err=%v)", got, err)
		}
		if err == nil || !strings.Contains(err.Error(), todosFileName) {
			t.Fatalf("error should name the file, got %v", err)
		}
	})
}

// TestConnectTodoNormalize: mechanical fixups land in the canonical doc —
// ids fill, duplicates regen, dangling goal refs clear, date spans clamp,
// sessions dedupe/cap, bad colors drop, empty-title goals vanish with
// their todos remapped to 未分组.
func TestConnectTodoNormalize(t *testing.T) {
	client, _ := newTodoFixture(t)
	ctx := context.Background()

	in := &lyonv1.TodoDoc{
		Goals: []*lyonv1.TodoGoal{
			{Id: "", Title: "  无 id 目标  ", Color: "not-a-color"},
			{Id: "g_dup", Title: "第一个"},
			{Id: "g_dup", Title: "重复 id"},
			{Id: "g_gone", Title: "   "}, // dropped: empty title
		},
		Todos: []*lyonv1.TodoItem{
			{
				Content: "  修 id + 颜色  ", Status: " pending ",
				Start: "2026-11-20", End: "2026-11-18", // end < start → clamp
				GoalId: "g_gone", // ghost after drop → ""
				// no id → filled
			},
			{
				Content: "重复 id", Status: "completed", Id: "t_same",
				Start: "2026-11-16", End: "2026-11-16", GoalId: "g_missing",
				SessionIds: []string{"s1", " s1 ", "", "s2"},
			},
			{
				Content: "end-only", Status: "pending", Id: "t_same", // dup → regen
				Start: "", End: "2026-11-16", // no start → end cleared
			},
			{
				Content: "cap sessions", Status: "pending", Id: "t_cap",
				SessionIds: func() []string {
					var ids []string
					for i := 0; i < todoMaxSessions+10; i++ {
						ids = append(ids, "s"+string(rune('a'+i%26))+strings.Repeat("x", i))
					}
					return ids
				}(),
			},
		},
	}
	saved, err := client.SaveTodos(ctx, &connect.Request[lyonv1.SaveTodosRequest]{Msg: &lyonv1.SaveTodosRequest{Doc: in}})
	if err != nil {
		t.Fatalf("SaveTodos: %v", err)
	}
	out := saved.Msg.GetDoc()

	if len(out.GetGoals()) != 3 {
		t.Fatalf("goals = %d, want 3 (empty-title dropped)", len(out.GetGoals()))
	}
	g0 := out.GetGoals()[0]
	if g0.GetId() == "" || !strings.HasPrefix(g0.GetId(), "g_") {
		t.Errorf("goal[0].id = %q, want g_-prefixed fill", g0.GetId())
	}
	if g0.GetTitle() != "无 id 目标" {
		t.Errorf("goal[0].title = %q, want trimmed", g0.GetTitle())
	}
	if g0.GetColor() != "" {
		t.Errorf("goal[0].color = %q, want bad color cleared", g0.GetColor())
	}
	if out.GetGoals()[1].GetId() == out.GetGoals()[2].GetId() {
		t.Errorf("duplicate goal ids not regen'd: %q", out.GetGoals()[1].GetId())
	}

	todo0 := out.GetTodos()[0]
	if todo0.GetId() == "" || !strings.HasPrefix(todo0.GetId(), "t_") {
		t.Errorf("todo[0].id = %q, want t_-prefixed fill", todo0.GetId())
	}
	if todo0.GetContent() != "修 id + 颜色" {
		t.Errorf("todo[0].content = %q, want trimmed", todo0.GetContent())
	}
	if todo0.GetStatus() != "pending" {
		t.Errorf("todo[0].status = %q, want trimmed pending", todo0.GetStatus())
	}
	if todo0.GetEnd() != "2026-11-20" {
		t.Errorf("todo[0].end = %q, want clamped to start", todo0.GetEnd())
	}
	if todo0.GetGoalId() != "" {
		t.Errorf("todo[0].goal_id = %q, want \"\" (ghost goal)", todo0.GetGoalId())
	}

	todo1 := out.GetTodos()[1]
	if todo1.GetGoalId() != "" {
		t.Errorf("todo[1].goal_id = %q, want \"\" (dangling ref)", todo1.GetGoalId())
	}
	if got := todo1.GetSessionIds(); len(got) != 2 || got[0] != "s1" || got[1] != "s2" {
		t.Errorf("todo[1].sessions = %v, want deduped [s1 s2]", got)
	}

	todo2 := out.GetTodos()[2]
	if todo2.GetId() == "t_same" {
		t.Errorf("duplicate todo id not regen'd")
	}
	if todo2.GetStart() != "" || todo2.GetEnd() != "" {
		t.Errorf("todo[2] span = %q..%q, want cleared (no start)", todo2.GetStart(), todo2.GetEnd())
	}

	todo3 := out.GetTodos()[3]
	if n := len(todo3.GetSessionIds()); n != todoMaxSessions {
		t.Errorf("todo[3].sessions = %d, want cap %d", n, todoMaxSessions)
	}
}
