package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"

	lyonv1 "lyon-agent/gen/go/lyon/v1"
	"lyon-agent/gen/go/lyon/v1/lyonv1connect"
)

// TodoService persists the global personal todolist at
// $LYON_HOME/todos.json: goals organize the overall objectives, todos
// carry optional date ranges for the month/gantt views and may link
// sessions. The whole document round-trips on every save (todo_write
// precedent: the newest payload replaces everything — no merge, no
// drift). Save normalizes before writing and returns the canonical
// document so the UI re-renders from what actually landed on disk.
const todosFileName = "todos.json"

// Caps. Count limits are hard InvalidArgument errors (a runaway client
// must not pin a multi-MB document); field limits truncate instead so a
// single long note never fails an otherwise valid save.
const (
	todoMaxGoals     = 200
	todoMaxTodos     = 2000
	todoTitleMax     = 120  // runes, per goal
	todoContentMax   = 300  // runes, per todo
	todoNoteMax      = 4000 // runes, per todo
	todoSessionIDMax = 128  // runes, per linked session id
	todoMaxSessions  = 50   // per todo
)

var (
	todoDateRe  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	todoColorRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
)

// todoService is stateless besides the file lock (planService pattern):
// no Server dependency, every call re-reads / re-writes the document.
type todoService struct {
	mu sync.Mutex
}

var _ lyonv1connect.TodoServiceHandler = (*todoService)(nil)

func (s *todoService) ListTodos(context.Context, *connect.Request[lyonv1.ListTodosRequest]) (*connect.Response[lyonv1.ListTodosResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := readTodoDoc()
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&lyonv1.ListTodosResponse{Doc: doc}), nil
}

func (s *todoService) SaveTodos(_ context.Context, req *connect.Request[lyonv1.SaveTodosRequest]) (*connect.Response[lyonv1.SaveTodosResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, err := normalizeTodoDoc(req.Msg.GetDoc())
	if err != nil {
		return nil, err
	}
	path, err := todosPath()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err))
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("encode todos: %w", err))
	}
	// Atomic replace: readers see the old document or the new one, never
	// a half-written file (approval_bridge grant write pattern).
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("write %s: %w", tmp, err))
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("commit %s: %w", path, err))
	}
	return connect.NewResponse(&lyonv1.SaveTodosResponse{Doc: doc}), nil
}

func todosPath() (string, error) {
	home, err := dashHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, todosFileName), nil
}

// readTodoDoc loads the document. A missing file is an empty document
// (fresh install); a corrupt one is Internal with the path in the hint —
// the user fixes or deletes it, Save is the recovery path since it never
// reads the old bytes.
func readTodoDoc() (*lyonv1.TodoDoc, error) {
	path, err := todosPath()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &lyonv1.TodoDoc{}, nil
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("read %s: %w", path, err))
	}
	var doc lyonv1.TodoDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, connect.NewError(connect.CodeInternal,
			fmt.Errorf("todos.json is corrupt (%v) — fix or delete %s", err, path))
	}
	return &doc, nil
}

func todoInvalid(format string, args ...any) error {
	return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(format, args...))
}

// normalizeTodoDoc enforces the save contract:
//
//   - hard errors (InvalidArgument): empty content, unknown status,
//     malformed or impossible dates, count caps;
//   - truncation: content/note/title/session-id field limits;
//   - mechanical fixups: missing or duplicate ids regen, empty-title
//     goals dropped (their todos remap to 未分组), dangling goal_id → "",
//     end < start clamps up, end without start clears, session ids
//     trimmed/deduped/capped, bad colors → "".
func normalizeTodoDoc(in *lyonv1.TodoDoc) (*lyonv1.TodoDoc, error) {
	if in == nil {
		in = &lyonv1.TodoDoc{}
	}
	if n := len(in.GetGoals()); n > todoMaxGoals {
		return nil, todoInvalid("too many goals: %d (max %d)", n, todoMaxGoals)
	}
	if n := len(in.GetTodos()); n > todoMaxTodos {
		return nil, todoInvalid("too many todos: %d (max %d)", n, todoMaxTodos)
	}

	out := &lyonv1.TodoDoc{}
	seenGoals := make(map[string]bool, len(in.GetGoals()))
	for _, g := range in.GetGoals() {
		if g == nil {
			continue
		}
		title := truncateRunes(strings.TrimSpace(g.GetTitle()), todoTitleMax)
		if title == "" {
			continue // dropped — todos below remap via validGoals
		}
		id := strings.TrimSpace(g.GetId())
		if id == "" || seenGoals[id] {
			id = todoNewID("g_")
		}
		seenGoals[id] = true
		color := strings.TrimSpace(g.GetColor())
		if !todoColorRe.MatchString(color) {
			color = ""
		}
		out.Goals = append(out.Goals, &lyonv1.TodoGoal{Id: id, Title: title, Color: color})
	}
	validGoals := make(map[string]bool, len(out.Goals))
	for _, g := range out.Goals {
		validGoals[g.Id] = true
	}

	seenTodos := make(map[string]bool, len(in.GetTodos()))
	for _, t := range in.GetTodos() {
		if t == nil {
			continue
		}
		content := truncateRunes(strings.TrimSpace(t.GetContent()), todoContentMax)
		if content == "" {
			return nil, todoInvalid("todo content must not be empty")
		}
		status := strings.TrimSpace(t.GetStatus())
		switch status {
		case "pending", "in_progress", "completed":
		default:
			return nil, todoInvalid("todo status must be pending|in_progress|completed, got %q", status)
		}
		start, err := todoDate(t.GetStart(), "start")
		if err != nil {
			return nil, err
		}
		end, err := todoDate(t.GetEnd(), "end")
		if err != nil {
			return nil, err
		}
		switch {
		case start == "":
			end = "" // no start ⇒ no span
		case end != "" && end < start:
			end = start // ISO YYYY-MM-DD compares chronologically
		}
		id := strings.TrimSpace(t.GetId())
		if id == "" || seenTodos[id] {
			id = todoNewID("t_")
		}
		seenTodos[id] = true
		goalID := strings.TrimSpace(t.GetGoalId())
		if !validGoals[goalID] {
			goalID = ""
		}
		sessions := make([]string, 0, len(t.GetSessionIds()))
		seenSessions := make(map[string]bool, len(t.GetSessionIds()))
		for _, sid := range t.GetSessionIds() {
			sid = truncateRunes(strings.TrimSpace(sid), todoSessionIDMax)
			if sid == "" || seenSessions[sid] {
				continue
			}
			seenSessions[sid] = true
			sessions = append(sessions, sid)
			if len(sessions) >= todoMaxSessions {
				break
			}
		}
		out.Todos = append(out.Todos, &lyonv1.TodoItem{
			Id:         id,
			Content:    content,
			Status:     status,
			Start:      start,
			End:        end,
			GoalId:     goalID,
			SessionIds: sessions,
			Note:       truncateRunes(t.GetNote(), todoNoteMax),
		})
	}
	return out, nil
}

// todoDate validates one date field: "" is legal (unscheduled), the
// shape must be YYYY-MM-DD and the value must be a real calendar date.
func todoDate(v, field string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	if !todoDateRe.MatchString(v) {
		return "", todoInvalid("%s must be YYYY-MM-DD, got %q", field, v)
	}
	if _, err := time.Parse("2006-01-02", v); err != nil {
		return "", todoInvalid("%s is not a real date: %q", field, v)
	}
	return v, nil
}

func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}

// todoNewID mints g_/t_ prefixed ids from crypto/rand (8 bytes hex).
func todoNewID(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Practically unreachable; time fallback keeps ids unique enough.
		return prefix + fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return prefix + hex.EncodeToString(b[:])
}
