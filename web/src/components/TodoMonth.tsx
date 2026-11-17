// TodoMonth — Monday-first month grid for the todos pane (the weekcal
// design package's day-cell rules, lyon dark theme): weekend tint,
// today border + badge, chips spanning [start, end], max 3 chips then
// “+N” (opens the day list), click-cell popover with quick-add.

import { useEffect, useMemo, useState } from "react";
import {
  addDays,
  isWeekendYmd,
  parseYmd,
  startOfWeek,
  todayYmd,
  ymd,
} from "../cal";
import {
  goalColor,
  spanOf,
  STATUS_GLYPH,
  type TodoEntry,
  type TodoGoal,
} from "../todos";

// Custom HTML5 DnD payload type: tray chips (未排期) and grid chips
// both announce their todo id under this type; cells only accept drags
// carrying it, so file drags / foreign DnD are ignored.
export const TODO_DND_TYPE = "text/lyon-todo";

type Props = {
  todos: TodoEntry[]; // already filtered by the pane's search/goal rail
  goals: TodoGoal[];
  cursor: { y: number; m: number }; // month cursor, m 0-based
  popDate: string | null; // open quick-add popover's day
  onPop: (date: string | null) => void;
  onOpen: (t: TodoEntry) => void;
  onQuickAdd: (date: string, content: string) => void;
  onCycle: (id: string) => void;
  arm: string; // armed delete key (two-click pattern)
  armOrRun: (key: string, run: () => void) => void;
  onDelete: (id: string) => void;
  onDropTodo: (id: string, date: string) => void; // drag → schedule
};

const WEEK_LABELS = ["一", "二", "三", "四", "五", "六", "日"];

export default function TodoMonth({
  todos,
  goals,
  cursor,
  popDate,
  onPop,
  onOpen,
  onQuickAdd,
  onCycle,
  arm,
  armOrRun,
  onDelete,
  onDropTodo,
}: Props) {
  const today = todayYmd();
  const [quick, setQuick] = useState("");
  // Cell under the current drag — highlight only; cleared on drop and on
  // the document-level dragend (the source may live outside this pane,
  // e.g. a tray chip, so per-element dragleave is not reliable).
  const [dropKey, setDropKey] = useState<string | null>(null);
  useEffect(() => {
    const clear = () => setDropKey(null);
    document.addEventListener("dragend", clear);
    document.addEventListener("drop", clear);
    return () => {
      document.removeEventListener("dragend", clear);
      document.removeEventListener("drop", clear);
    };
  }, []);

  // Index entries by day once: an entry occupies every day of its span.
  const byDay = useMemo(() => {
    const map = new Map<string, TodoEntry[]>();
    for (const t of todos) {
      const span = spanOf(t);
      if (!span) continue; // unscheduled: rail only, never on the grid
      const [from, to] = span;
      let d = parseYmd(from);
      const end = parseYmd(to);
      if (!d || !end) continue;
      let guard = 0;
      while (d && d <= end && guard++ < 400) {
        const key = ymd(d);
        const arr = map.get(key);
        if (arr) arr.push(t);
        else map.set(key, [t]);
        d = addDays(d, 1);
      }
    }
    return map;
  }, [todos]);

  const colorOf = useMemo(() => {
    const m = new Map<string, string>();
    for (const g of goals) m.set(g.id, goalColor(g));
    return m;
  }, [goals]);

  // 42 cells (6 fixed rows): grid starts at Monday of the week holding
  // the 1st; stable height avoids layout jumps between months.
  const cells = useMemo(() => {
    const first = new Date(cursor.y, cursor.m, 1);
    const gridStart = startOfWeek(first);
    return Array.from({ length: 42 }, (_, i) => {
      const d = addDays(gridStart, i);
      return { key: ymd(d), day: d.getDate(), out: d.getMonth() !== cursor.m };
    });
  }, [cursor]);

  const closePop = () => {
    onPop(null);
    setQuick("");
  };

  return (
    <div className="td-month">
      <div className="td-wk">
        {WEEK_LABELS.map((w, i) => (
          <div key={w} className={`td-wkh${i >= 5 ? " we" : ""}`}>
            {w}
          </div>
        ))}
      </div>
      <div className="td-grid">
        {cells.map((c, i) => {
          const dayTodos = byDay.get(c.key) ?? [];
          const weekend = isWeekendYmd(c.key);
          const isToday = c.key === today;
          const row = Math.floor(i / 7);
          const col = i % 7;
          const pop = popDate === c.key;
          return (
            <div
              key={c.key}
              className={[
                "td-cell",
                c.out ? "out" : "",
                weekend ? "we" : "",
                isToday ? "now" : "",
                pop ? "pop" : "",
                dropKey === c.key ? "dropping" : "",
              ]
                .filter(Boolean)
                .join(" ")}
              onClick={() => (pop ? closePop() : onPop(c.key))}
              onDragOver={(e) => {
                if (!e.dataTransfer.types.includes(TODO_DND_TYPE)) return;
                e.preventDefault();
                e.dataTransfer.dropEffect = "move";
                if (dropKey !== c.key) setDropKey(c.key);
              }}
              onDrop={(e) => {
                const id = e.dataTransfer.getData(TODO_DND_TYPE);
                if (!id) return;
                e.preventDefault();
                setDropKey(null);
                onDropTodo(id, c.key);
              }}
            >
              <div className="td-dn">
                {c.day}
                {isToday && <em>今天</em>}
              </div>
              <div className="td-chips">
                {dayTodos.slice(0, 3).map((t) => {
                  const key = "t:" + t.id;
                  const armed = arm === key;
                  return (
                    <div
                      key={t.id}
                      className={`td-chip st-${t.status}`}
                      draggable
                      onDragStart={(e) => {
                        e.dataTransfer.setData(TODO_DND_TYPE, t.id);
                        e.dataTransfer.effectAllowed = "move";
                      }}
                    >
                      <button
                        type="button"
                        className="td-cg"
                        aria-label="切换状态"
                        onClick={(e) => {
                          e.stopPropagation();
                          onCycle(t.id);
                        }}
                      >
                        {STATUS_GLYPH[t.status]}
                      </button>
                      <button
                        type="button"
                        className="td-ct"
                        title={t.content}
                        onClick={(e) => {
                          e.stopPropagation();
                          onOpen(t);
                        }}
                      >
                        <i
                          className="td-cbar"
                          style={{ background: t.goalId ? colorOf.get(t.goalId) ?? "var(--fg-faint)" : "var(--fg-faint)" }}
                        />
                        {t.content}
                      </button>
                      <button
                        type="button"
                        className={`td-cx${armed ? " armed" : ""}`}
                        aria-label="删除待办"
                        onClick={(e) => {
                          e.stopPropagation();
                          armOrRun(key, () => onDelete(t.id));
                        }}
                      >
                        {armed ? "?" : "×"}
                      </button>
                    </div>
                  );
                })}
                {dayTodos.length > 3 && (
                  <button
                    type="button"
                    className="td-more"
                    onClick={(e) => {
                      e.stopPropagation();
                      onPop(c.key);
                    }}
                  >
                    +{dayTodos.length - 3}
                  </button>
                )}
              </div>

              {pop && (
                <div
                  className={`td-pop${row >= 4 ? " up" : ""}${col >= 5 ? " left" : ""}`}
                  onClick={(e) => e.stopPropagation()}
                >
                  <form
                    onSubmit={(e) => {
                      e.preventDefault();
                      onQuickAdd(c.key, quick);
                      setQuick("");
                    }}
                  >
                    <input
                      autoFocus
                      placeholder="快速添加，回车确认…"
                      value={quick}
                      onChange={(e) => setQuick(e.target.value)}
                    />
                  </form>
                  {dayTodos.length > 0 && (
                    <div className="td-poplist">
                      {dayTodos.map((t) => (
                        <button
                          key={t.id}
                          type="button"
                          className={`td-poprow st-${t.status}`}
                          onClick={() => onOpen(t)}
                        >
                          <span className="td-cg">{STATUS_GLYPH[t.status]}</span>
                          <span className="td-pt">{t.content}</span>
                        </button>
                      ))}
                    </div>
                  )}
                  <div className="td-popfoot">{c.key}</div>
                </div>
              )}
            </div>
          );
        })}
      </div>
    </div>
  );
}
