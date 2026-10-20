import type { TodoItem } from "../todos";

// Plan progress for the resident header: how many done, and what the
// model is on right now (first in_progress item, TUI todoProgress).
export function todoProgress(items: TodoItem[]): {
  done: number;
  total: number;
  current: string;
} {
  let done = 0;
  let current = "";
  for (const it of items) {
    if (it.status === "completed") done++;
    if (it.status === "in_progress" && !current) current = it.content;
  }
  return { done, total: items.length, current };
}

// PlanPane renders the seat's todo list as its own workspace pane (was
// the resident corner of SidePanel; the tiling workspace promotes it to
// a first-class pane so it can sit anywhere).
export default function PlanPane({ todos }: { todos: TodoItem[] }) {
  if (todos.length === 0) {
    return (
      <div className="plan-corner empty">
        <span className="plan-head">📋 plan · none yet</span>
      </div>
    );
  }
  const { done, total, current } = todoProgress(todos);
  return (
    <div className="plan-corner">
      <div className="plan-head">
        <span className="plan-scale">
          📋 plan {done}/{total}
        </span>
        {current && <span className="plan-current">▸ {current}</span>}
      </div>
      <ul className="plan-items">
        {todos.map((it, i) => (
          <li key={i} className={`plan-item ${it.status}`}>
            <span className="plan-glyph" aria-hidden="true">
              {it.status === "completed" ? "☑" : it.status === "in_progress" ? "◐" : "☐"}
            </span>
            {it.content}
          </li>
        ))}
      </ul>
    </div>
  );
}
