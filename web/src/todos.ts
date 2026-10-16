// todoItem mirrors one plan entry of a todo_write call (#66 readable step
// feedback, cmd/lyon/todos.go): the input always carries the COMPLETE
// list, so the newest event replaces the whole plan — no merge, no drift.
export type TodoItem = {
  content: string;
  status: "pending" | "in_progress" | "completed";
};

// parseTodos reads a todo_write input payload, dropping malformed items —
// a stray status must not hide the rest of the plan from the reader.
// Returns null when there is nothing renderable.
export function parseTodos(raw: string): TodoItem[] | null {
  let inTodos: { todos?: unknown };
  try {
    inTodos = JSON.parse(raw);
  } catch {
    return null;
  }
  if (!Array.isArray(inTodos.todos) || inTodos.todos.length === 0) return null;
  const out: TodoItem[] = [];
  for (const item of inTodos.todos as Array<Record<string, unknown>>) {
    const content = String(item.content ?? "").trim();
    if (!content) continue;
    const status = item.status;
    if (status !== "pending" && status !== "in_progress" && status !== "completed") {
      continue;
    }
    out.push({ content: content.slice(0, 120), status });
  }
  return out.length > 0 ? out : null;
}
