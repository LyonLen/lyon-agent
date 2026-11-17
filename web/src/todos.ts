// todoItem mirrors one plan entry of a todo_write call (#66 readable step
// feedback, cmd/lyon/todos.go): the input always carries the COMPLETE
// list, so the newest event replaces the whole plan — no merge, no drift.
export type TodoItem = {
  content: string;
  status: "pending" | "in_progress" | "completed";
};

// ── the todos pane document ($LYON_HOME/todos.json over TodoService) ──
// Plain structural types: the pane owns them as React state and hands
// them to saveTodos as the request init shape (protobuf-es accepts
// plain objects); the wire messages stay behind the RPC boundary.

export type TodoStatus = "pending" | "in_progress" | "completed";

export type TodoGoal = {
  id: string;
  title: string;
  color: string; // #rrggbb or "" → hash the title into GOAL_PALETTE
};

export type TodoEntry = {
  id: string;
  content: string;
  status: TodoStatus;
  start: string; // YYYY-MM-DD, "" = unscheduled (rail only, no bar)
  end: string; // "" = same day as start
  goalId: string; // "" = 未分组
  sessionIds: string[];
  note: string;
};

export type TodosDoc = {
  goals: TodoGoal[];
  todos: TodoEntry[];
};

export const emptyTodosDoc = (): TodosDoc => ({ goals: [], todos: [] });

// deep copy — mutate-then-replace updates keep React re-rendering and
// never alias the previous state.
export function cloneTodosDoc(d: TodosDoc): TodosDoc {
  return {
    goals: d.goals.map((g) => ({ ...g })),
    todos: d.todos.map((t) => ({ ...t, sessionIds: [...t.sessionIds] })),
  };
}

// GOAL_PALETTE — the weekcal design package's PROJECT_PALETTE verbatim:
// ten hues, one per goal, hashed from the title when the goal carries
// no explicit color (same colorForProject rule as the reference app).
export const GOAL_PALETTE = [
  "#0b3d91", "#0e7c7b", "#7b61ff", "#f2994a", "#2d9cdb",
  "#27ae60", "#eb5757", "#9b51e0", "#00a3a3", "#b8860b",
];

export function goalColor(g?: { title: string; color?: string }): string {
  if (!g) return GOAL_PALETTE[0];
  if (g.color && /^#[0-9a-fA-F]{6}$/.test(g.color)) return g.color;
  let h = 0;
  const s = g.title || "";
  for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) >>> 0;
  return GOAL_PALETTE[h % GOAL_PALETTE.length];
}

export const STATUS_GLYPH: Record<TodoStatus, string> = {
  pending: "☐",
  in_progress: "◐",
  completed: "☑",
};

export function nextStatus(s: TodoStatus): TodoStatus {
  return s === "pending" ? "in_progress" : s === "in_progress" ? "completed" : "pending";
}

export function isTodoStatus(v: unknown): v is TodoStatus {
  return v === "pending" || v === "in_progress" || v === "completed";
}

// uid mints client-side ids (t_/g_ prefixed, same shape the server
// fills in); the server still regenerates empties/dups on save.
export function uid(prefix: string): string {
  return prefix + Math.random().toString(36).slice(2, 10) + Date.now().toString(36);
}

// endsOf: the inclusive [start, end] span of an entry as day strings
// (end falls back to start — "end = same day as start" contract).
export function spanOf(t: TodoEntry): [string, string] | null {
  if (!t.start) return null;
  return [t.start, t.end && t.end >= t.start ? t.end : t.start];
}

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
