// TodosPane — the global personal todolist as a GLOBAL OVERLAY (not a
// workspace pane: layouts are session-bound, a tile would vanish on seat
// switch; the doc is global, the UI is too). Entry points: topbar「待办」,
// /todos, sidebar「＋」. Contents:
//   · goals rail (整体目标): add / rename / two-click delete, filter
//   · 月历 view: Monday-first month grid, day cells, chips, quick-add
//   · 甘特 view: auto-fit timeline grouped by goal, weekend shading,
//     today line, undated section
//   · session links: every entry may reference session ids; the modal
//     jumps to a linked session or links the current one.
//   · 会话填充 (拿会话填充待办): a toolbar popover, plus the sidebar
//     「＋」 queue, push sessions into the doc as entries linked by
//     session id — deduped per session so one session never lands twice.
// Persistence is $LYON_HOME/todos.json over TodoService — every
// mutation saves the WHOLE document (todo_write precedent: the newest
// payload replaces everything, no merge, no drift).

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { errText, todoClient } from "../api";
import type { Session } from "../gen/lyon/v1/session_pb";
import type { TodoDoc as ProtoTodoDoc } from "../gen/lyon/v1/todo_pb";
import { addDays, daysBetween, parseYmd, ymd } from "../cal";
import {
  cloneTodosDoc,
  emptyTodosDoc,
  goalColor,
  isTodoStatus,
  nextStatus,
  spanOf,
  uid,
  type TodoEntry,
  type TodosDoc,
} from "../todos";
import TodoMonth, { TODO_DND_TYPE } from "./TodoMonth";
import TodoGantt from "./TodoGantt";
import TodoModal from "./TodoModal";

// fromProto maps the wire message into the pane's plain document —
// status falls back to pending on a stray value so one bad row can't
// break the whole calendar.
function fromProto(p: ProtoTodoDoc | undefined): TodosDoc {
  if (!p) return emptyTodosDoc();
  return {
    goals: (p.goals ?? []).map((g) => ({ id: g.id, title: g.title, color: g.color })),
    todos: (p.todos ?? []).map((t) => ({
      id: t.id,
      content: t.content,
      status: isTodoStatus(t.status) ? t.status : "pending",
      start: t.start,
      end: t.end,
      goalId: t.goalId,
      sessionIds: [...(t.sessionIds ?? [])],
      note: t.note,
    })),
  };
}

type GoalFilter = "all" | "ungrouped" | string; // string = a goal id

// Short local timestamp — the fill popover needs "when", to the minute
// (same helper as the session sidebar).
function shortWhen(d: Date): string {
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

export default function TodosPane({
  sessions,
  selectedId,
  onSelect,
  fillQueue,
  onFillConsumed,
  onClose,
}: {
  sessions: Session[];
  selectedId: string;
  onSelect: (id: string) => void;
  // Sessions queued by the sidebar「＋」— consumed here because this
  // component owns the full-doc save (a second writer would clobber it).
  fillQueue: Session[];
  onFillConsumed: () => void;
  onClose: () => void;
}) {
  const [doc, setDoc] = useState<TodosDoc>(emptyTodosDoc);
  const docRef = useRef<TodosDoc>(emptyTodosDoc());
  const [err, setErr] = useState("");
  // Initial listTodos landed? Fill actions wait for it so an append can't
  // save over a document that never made it into docRef.
  const [loaded, setLoaded] = useState(false);
  const [saving, setSaving] = useState(false);
  const [view, setView] = useState<"month" | "gantt">("month");
  const [cursor, setCursor] = useState(() => {
    const n = new Date();
    return { y: n.getFullYear(), m: n.getMonth() };
  });
  const [search, setSearch] = useState("");
  const [filterGoal, setFilterGoal] = useState<GoalFilter>("all");
  const [draft, setDraft] = useState<TodoEntry | null>(null);
  const [draftNew, setDraftNew] = useState(false);
  const [popDate, setPopDate] = useState<string | null>(null);
  const [arm, setArm] = useState(""); // two-click delete: armed key
  const armTimer = useRef(0);
  // 会话填充 popover + one-shot green status line (fill feedback)
  const [fillOpen, setFillOpen] = useState(false);
  const [fillQ, setFillQ] = useState("");
  const [flash, setFlash] = useState("");
  const flashTimer = useRef(0);
  // rail inline states
  const [goalAdding, setGoalAdding] = useState(false);
  const [goalInput, setGoalInput] = useState("");
  const [renameId, setRenameId] = useState("");
  const [renameVal, setRenameVal] = useState("");

  // ── persistence ────────────────────────────────────────────────
  const seq = useRef(0);
  const persist = useCallback((next: TodosDoc) => {
    const token = ++seq.current;
    setSaving(true);
    todoClient
      .saveTodos({ doc: next })
      .then(() => {
        if (token === seq.current) {
          setErr("");
          setSaving(false);
        }
      })
      .catch((e) => {
        if (token === seq.current) {
          setErr(errText(e));
          setSaving(false);
        }
      });
  }, []);

  // mutate composes through docRef (not the setState updater): rapid
  // clicks must not nest side effects, and StrictMode's double invoke
  // must not double-save.
  const mutate = useCallback(
    (fn: (d: TodosDoc) => TodosDoc) => {
      const next = fn(cloneTodosDoc(docRef.current));
      docRef.current = next;
      setDoc(next);
      persist(next);
    },
    [persist],
  );

  useEffect(() => {
    let live = true;
    todoClient
      .listTodos({})
      .then((r) => {
        if (!live) return;
        const d = fromProto(r.doc);
        docRef.current = d;
        setDoc(d);
        // Only a successful load opens the fill valve — on failure docRef
        // is still empty and a fill-save would replace the real document.
        setLoaded(true);
      })
      .catch((e) => {
        if (live) setErr(errText(e));
      });
    return () => {
      live = false;
    };
  }, []);

  useEffect(
    () => () => {
      window.clearTimeout(armTimer.current);
      window.clearTimeout(flashTimer.current);
    },
    [],
  );

  // ── filtering ──────────────────────────────────────────────────
  const visible = useMemo(() => {
    const q = search.trim().toLowerCase();
    return doc.todos.filter((t) => {
      if (filterGoal === "ungrouped" && t.goalId) return false;
      if (filterGoal !== "all" && filterGoal !== "ungrouped" && t.goalId !== filterGoal) {
        return false;
      }
      if (q && !t.content.toLowerCase().includes(q) && !t.note.toLowerCase().includes(q)) {
        return false;
      }
      return true;
    });
  }, [doc, filterGoal, search]);

  // Undated entries (all会话填充 rows are unscheduled by design) never
  // land in the month grid — without this tray the default view looks
  // empty after a fill ("加不进来" report). The gantt shows the same
  // set under 未排期; here they ride above the calendar.
  const unscheduled = useMemo(() => visible.filter((t) => !t.start), [visible]);

  const goalStats = useMemo(() => {
    const map = new Map<string, { total: number; done: number }>();
    let ungrouped = { total: 0, done: 0 };
    for (const t of doc.todos) {
      const bucket = t.goalId ? (map.get(t.goalId) ?? { total: 0, done: 0 }) : ungrouped;
      bucket.total++;
      if (t.status === "completed") bucket.done++;
      if (t.goalId) map.set(t.goalId, bucket);
      else ungrouped = bucket;
    }
    return { map, ungrouped };
  }, [doc]);

  // ── two-click delete (repo pattern: arm → confirm within 3s) ──
  const armOrRun = useCallback(
    (key: string, run: () => void) => {
      window.clearTimeout(armTimer.current);
      if (arm === key) {
        setArm("");
        run();
        return;
      }
      setArm(key);
      armTimer.current = window.setTimeout(() => setArm(""), 3000);
    },
    [arm],
  );

  // ── 会话填充 (sessions → entries) ──────────────────────────────
  const flashMsg = useCallback((msg: string) => {
    setFlash(msg);
    window.clearTimeout(flashTimer.current);
    flashTimer.current = window.setTimeout(() => setFlash(""), 2800);
  }, []);

  // The rail filter carries into filled entries, same as + 待办.
  const newGoalId = filterGoal !== "all" && filterGoal !== "ungrouped" ? filterGoal : "";

  // One todo per session: content = session title, linked by session id,
  // unscheduled (月历不显示；侧栏计数与甘特「未排期」可见).
  const sessionEntry = useCallback(
    (s: Session): TodoEntry => ({
      id: uid("t_"),
      content: (s.title || "").trim() || "(untitled)",
      status: "pending",
      start: "",
      end: "",
      goalId: newGoalId,
      sessionIds: [s.id],
      note: "",
    }),
    [newGoalId],
  );

  // session → its first linking entry (dedupe + "already there" state)
  const linkedBy = useMemo(() => {
    const m = new Map<string, TodoEntry>();
    for (const t of doc.todos) {
      for (const sid of t.sessionIds) {
        if (!m.has(sid)) m.set(sid, t);
      }
    }
    return m;
  }, [doc]);

  // Popover rows: every session matching the search (linked ones show ✓);
  // fillTargets narrows to the not-yet-linked ones for the bulk button.
  const fillList = useMemo(() => {
    const q = fillQ.trim().toLowerCase();
    return sessions.filter((s) => !q || (s.title || "").toLowerCase().includes(q));
  }, [sessions, fillQ]);
  const fillTargets = useMemo(
    () => fillList.filter((s) => !linkedBy.has(s.id)),
    [fillList, linkedBy],
  );
  const linkedCount = useMemo(
    () => sessions.reduce((n, s) => n + (linkedBy.has(s.id) ? 1 : 0), 0),
    [sessions, linkedBy],
  );

  const addFromSession = useCallback(
    (s: Session) => {
      if (!loaded) return; // doc not in hand yet — appending would clobber
      if (linkedBy.has(s.id)) {
        flashMsg("会话已在待办中");
        return;
      }
      mutate((d) => {
        d.todos.push(sessionEntry(s));
        return d;
      });
      setErr("");
      flashMsg(`已加入待办：${(s.title || "").trim() || s.id.slice(0, 8)}`);
    },
    [loaded, linkedBy, mutate, sessionEntry, flashMsg],
  );

  // Bulk fill (two-click armed — repo delete pattern): appends every
  // not-yet-linked row currently visible in the popover.
  const fillAll = useCallback(() => {
    if (!loaded || fillTargets.length === 0) return;
    const targets = fillTargets;
    mutate((d) => {
      for (const s of targets) d.todos.push(sessionEntry(s));
      return d;
    });
    setErr("");
    flashMsg(`已填充 ${targets.length} 条会话`);
    setFillOpen(false);
    setFillQ("");
  }, [loaded, fillTargets, mutate, sessionEntry, flashMsg]);

  // The sidebar「＋」queues sessions here. Dedupe runs against docRef
  // (mutate writes it synchronously), and the queue identity ref keeps
  // StrictMode's double effect from appending the same session twice.
  const handledQueue = useRef<Session[] | null>(null);
  useEffect(() => {
    if (!loaded || fillQueue.length === 0) return;
    if (handledQueue.current === fillQueue) return;
    handledQueue.current = fillQueue;
    const pending = fillQueue.filter(
      (s) => !docRef.current.todos.some((t) => t.sessionIds.includes(s.id)),
    );
    if (pending.length > 0) {
      mutate((d) => {
        for (const s of pending) d.todos.push(sessionEntry(s));
        return d;
      });
      setErr("");
      flashMsg(
        pending.length === fillQueue.length
          ? `已填充 ${pending.length} 条会话`
          : `已填充 ${pending.length} 条会话（${fillQueue.length - pending.length} 条已在列）`,
      );
    } else {
      flashMsg("会话已在待办中");
    }
    onFillConsumed();
  }, [loaded, fillQueue, mutate, sessionEntry, flashMsg, onFillConsumed]);

  // ESC ladder: modal → popover → overlay. The guards matter: TodoModal
  // listens on document too (it runs after this effect on the same
  // keypress), and one press must only collapse the topmost open layer.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "Escape") return;
      if (draft) return; // the modal's own listener closes it
      if (fillOpen) {
        setFillOpen(false);
        return;
      }
      onClose();
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [draft, fillOpen, onClose]);

  // ── entry operations ───────────────────────────────────────────
  const openEntry = useCallback((t: TodoEntry) => {
    setDraft({ ...t, sessionIds: [...t.sessionIds] });
    setDraftNew(false);
    setPopDate(null);
    setFillOpen(false);
  }, []);

  const newEntry = useCallback(
    (start?: string) => {
      setDraft({
        id: "",
        content: "",
        status: "pending",
        start: start ?? "",
        end: start ?? "",
        goalId: filterGoal !== "all" && filterGoal !== "ungrouped" ? filterGoal : "",
        sessionIds: [],
        note: "",
      });
      setDraftNew(true);
      setPopDate(null);
      setFillOpen(false);
    },
    [filterGoal],
  );

  const cycleStatus = useCallback(
    (id: string) => {
      mutate((d) => {
        const t = d.todos.find((x) => x.id === id);
        if (t) t.status = nextStatus(t.status);
        return d;
      });
    },
    [mutate],
  );

  const saveDraft = useCallback(() => {
    if (!draft) return;
    const content = draft.content.trim();
    if (!content) {
      setErr("内容不能为空");
      return;
    }
    mutate((d) => {
      const entry: TodoEntry = { ...draft, content, sessionIds: [...draft.sessionIds] };
      if (draftNew || !d.todos.some((t) => t.id === entry.id)) {
        if (!entry.id) entry.id = uid("t_");
        d.todos.push(entry);
      } else {
        const i = d.todos.findIndex((t) => t.id === entry.id);
        d.todos[i] = entry;
      }
      return d;
    });
    setErr("");
    setDraft(null);
    setDraftNew(false);
  }, [draft, draftNew, mutate]);

  const deleteDraft = useCallback(() => {
    if (!draft || !draft.id) {
      setDraft(null);
      setDraftNew(false);
      return;
    }
    mutate((d) => {
      d.todos = d.todos.filter((t) => t.id !== draft.id);
      return d;
    });
    setDraft(null);
    setDraftNew(false);
  }, [draft, mutate]);

  const quickAdd = useCallback(
    (date: string, content: string) => {
      const c = content.trim();
      if (!c) return;
      mutate((d) => {
        d.todos.push({
          id: uid("t_"),
          content: c,
          status: "pending",
          start: date,
          end: date,
          goalId: filterGoal !== "all" && filterGoal !== "ungrouped" ? filterGoal : "",
          sessionIds: [],
          note: "",
        });
        return d;
      });
      setErr("");
    },
    [filterGoal, mutate],
  );

  // Drag-to-schedule: drop a 未排期 tray chip (or a grid chip) on a day
  // cell. Undated → single day on that date (quickAdd contract); an
  // existing span keeps its length and shifts; dropping inside the
  // current span is a no-op. YYYY-MM-DD strings compare chronologically.
  const dropTodoOn = useCallback(
    (id: string, date: string) => {
      if (!loaded) return;
      const cur = docRef.current.todos.find((t) => t.id === id);
      if (!cur) return;
      const span = spanOf(cur);
      if (span && date >= span[0] && date <= span[1]) return;
      mutate((d) => {
        const t = d.todos.find((x) => x.id === id);
        if (!t) return d;
        if (span) {
          const len = daysBetween(span[0], span[1]);
          const end = parseYmd(date);
          t.start = date;
          t.end = end ? ymd(addDays(end, len)) : date;
        } else {
          t.start = date;
          t.end = date;
        }
        return d;
      });
      setErr("");
      flashMsg(span ? `已移动到 ${date}` : `已排期 ${date}`);
    },
    [loaded, mutate, flashMsg],
  );

  // ── goal operations ────────────────────────────────────────────
  const addGoal = useCallback(() => {
    const title = goalInput.trim();
    if (!title) return;
    mutate((d) => {
      d.goals.push({ id: uid("g_"), title, color: "" });
      return d;
    });
    setGoalInput("");
    setGoalAdding(false);
    setErr("");
  }, [goalInput, mutate]);

  const commitRename = useCallback(() => {
    const title = renameVal.trim();
    if (renameId && title) {
      mutate((d) => {
        const g = d.goals.find((x) => x.id === renameId);
        if (g) g.title = title;
        return d;
      });
      setErr("");
    }
    setRenameId("");
    setRenameVal("");
  }, [renameId, renameVal, mutate]);

  const deleteGoal = useCallback(
    (id: string) => {
      mutate((d) => {
        d.goals = d.goals.filter((g) => g.id !== id);
        for (const t of d.todos) if (t.goalId === id) t.goalId = "";
        return d;
      });
      if (filterGoal === id) setFilterGoal("all");
      setErr("");
    },
    [filterGoal, mutate],
  );

  // ── month navigation ──────────────────────────────────────────
  const stepMonth = useCallback((delta: number) => {
    setCursor((c) => {
      const d = new Date(c.y, c.m + delta, 1);
      return { y: d.getFullYear(), m: d.getMonth() };
    });
    setPopDate(null);
  }, []);
  const goToday = useCallback(() => {
    const n = new Date();
    setCursor({ y: n.getFullYear(), m: n.getMonth() });
    setPopDate(null);
  }, []);

  const monthLabel = `${cursor.y}年${cursor.m + 1}月`;

  return (
    <div className="td-overlay" role="dialog" aria-modal="true" aria-label="全局待办">
      <div className="td-obackdrop" onClick={onClose} />
      <div className="td-opanel">
        <div className="td-root">
      <div className="td-tbar">
        <input
          className="td-search"
          placeholder="搜索待办…"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
        />
        <button type="button" className="td-btn" onClick={() => newEntry()}>
          + 待办
        </button>
        <button
          type="button"
          className="td-btn"
          onClick={() => {
            setGoalAdding(true);
            setFilterGoal("all");
          }}
        >
          + 目标
        </button>
        <div className="td-fillwrap">
          <button
            type="button"
            className="td-btn"
            aria-expanded={fillOpen}
            onClick={() => setFillOpen((v) => !v)}
          >
            从会话填充
          </button>
          {fillOpen && (
            <>
              {/* click-catcher under the panel: any outside click closes */}
              <div className="td-fillback" onClick={() => setFillOpen(false)} />
              <div className="td-fillpop" role="dialog" aria-label="从会话填充待办">
                <input
                  autoFocus
                  placeholder="搜索会话…"
                  value={fillQ}
                  onChange={(e) => setFillQ(e.target.value)}
                />
                <div className="td-filllist">
                  {fillList.map((s) => {
                    const hit = linkedBy.get(s.id);
                    return (
                      <button
                        key={s.id}
                        type="button"
                        className={"td-fillrow" + (hit ? " on" : "")}
                        title={hit ? "已在待办 — 点击打开" : "点击加入待办（未排期，可再排期）"}
                        onClick={() => {
                          if (hit) {
                            setFillOpen(false);
                            openEntry(hit);
                          } else {
                            addFromSession(s);
                          }
                        }}
                      >
                        <span className="td-fr-t">{s.title || "(untitled)"}</span>
                        <span className="td-fr-m">
                          {s.lastActive ? shortWhen(timestampDate(s.lastActive)) : s.mode}
                        </span>
                        <span className="td-fr-a">{hit ? "✓" : "+"}</span>
                      </button>
                    );
                  })}
                  {fillList.length === 0 && (
                    <div className="td-fillnone">没有匹配的会话</div>
                  )}
                </div>
                <div className="td-fillfoot">
                  <span>
                    {linkedCount}/{sessions.length} 已入列
                  </span>
                  <button
                    type="button"
                    className={`td-btn sm${arm === "fill-all" ? " danger armed" : ""}`}
                    disabled={fillTargets.length === 0}
                    onClick={() => armOrRun("fill-all", fillAll)}
                  >
                    {arm === "fill-all"
                      ? `确认填充 ${fillTargets.length}?`
                      : `填充未入列 (${fillTargets.length})`}
                  </button>
                </div>
              </div>
            </>
          )}
        </div>
        <div className="td-toggle" role="tablist" aria-label="视图切换">
          <button
            type="button"
            role="tab"
            aria-selected={view === "month"}
            className={view === "month" ? "on" : ""}
            onClick={() => setView("month")}
          >
            月历
          </button>
          <button
            type="button"
            role="tab"
            aria-selected={view === "gantt"}
            className={view === "gantt" ? "on" : ""}
            onClick={() => setView("gantt")}
          >
            甘特
          </button>
        </div>
        {view === "month" && (
          <div className="td-nav">
            <button type="button" className="td-btn sq" aria-label="上个月" onClick={() => stepMonth(-1)}>
              ‹
            </button>
            <span className="td-month">{monthLabel}</span>
            <button type="button" className="td-btn sq" aria-label="下个月" onClick={() => stepMonth(1)}>
              ›
            </button>
            <button type="button" className="td-btn" onClick={goToday}>
              今天
            </button>
          </div>
        )}
        <div className="td-status">
          {flash && <span className="td-flash">{flash}</span>}
          {saving && <span className="td-saving">保存中…</span>}
          {err && <span className="td-err">{err}</span>}
        </div>
        <button
          type="button"
          className="td-btn sq"
          aria-label="关闭待办"
          title="关闭 (Esc)"
          onClick={onClose}
        >
          ×
        </button>
      </div>

      <div className="td-body">
        <div className="td-rail">
          <button
            type="button"
            className={`td-goal${filterGoal === "all" ? " on" : ""}`}
            onClick={() => setFilterGoal("all")}
          >
            <span className="td-dot all" />
            <span className="td-gt">全部</span>
            <span className="td-gn">{doc.todos.length}</span>
          </button>
          {doc.goals.map((g) => {
            const st = goalStats.map.get(g.id) ?? { total: 0, done: 0 };
            return renameId === g.id ? (
              <input
                key={g.id}
                className="td-rename"
                autoFocus
                value={renameVal}
                onChange={(e) => setRenameVal(e.target.value)}
                onBlur={commitRename}
                onKeyDown={(e) => {
                  if (e.key === "Enter") commitRename();
                  if (e.key === "Escape") {
                    setRenameId("");
                    setRenameVal("");
                  }
                }}
              />
            ) : (
              <div
                key={g.id}
                className={`td-goal${filterGoal === g.id ? " on" : ""}`}
                role="button"
                tabIndex={0}
                onClick={() => setFilterGoal(filterGoal === g.id ? "all" : g.id)}
                onKeyDown={(e) => {
                  if (e.key === "Enter" || e.key === " ") {
                    e.preventDefault();
                    setFilterGoal(filterGoal === g.id ? "all" : g.id);
                  }
                }}
              >
                <span className="td-dot" style={{ background: goalColor(g) }} />
                <span className="td-gt" title={g.title}>
                  {g.title}
                </span>
                <span className="td-gn">
                  {st.done}/{st.total}
                </span>
                <span className="td-gacts">
                  <button
                    type="button"
                    className="td-mini"
                    aria-label="重命名目标"
                    onClick={(e) => {
                      e.stopPropagation();
                      setRenameId(g.id);
                      setRenameVal(g.title);
                    }}
                  >
                    ✎
                  </button>
                  <button
                    type="button"
                    className={`td-mini danger${arm === "g:" + g.id ? " armed" : ""}`}
                    aria-label="删除目标"
                    onClick={(e) => {
                      e.stopPropagation();
                      armOrRun("g:" + g.id, () => deleteGoal(g.id));
                    }}
                  >
                    {arm === "g:" + g.id ? "确认?" : "×"}
                  </button>
                </span>
              </div>
            );
          })}
          <button
            type="button"
            className={`td-goal${filterGoal === "ungrouped" ? " on" : ""}`}
            onClick={() => setFilterGoal(filterGoal === "ungrouped" ? "all" : "ungrouped")}
          >
            <span className="td-dot none" />
            <span className="td-gt">未分组</span>
            <span className="td-gn">
              {goalStats.ungrouped.done}/{goalStats.ungrouped.total}
            </span>
          </button>
          {goalAdding && (
            <input
              className="td-rename"
              autoFocus
              placeholder="新目标名称…"
              value={goalInput}
              onChange={(e) => setGoalInput(e.target.value)}
              onBlur={() => {
                if (goalInput.trim()) addGoal();
                else {
                  setGoalAdding(false);
                  setGoalInput("");
                }
              }}
              onKeyDown={(e) => {
                if (e.key === "Enter") addGoal();
                if (e.key === "Escape") {
                  setGoalAdding(false);
                  setGoalInput("");
                }
              }}
            />
          )}
        </div>

        <div className="td-main">
          {view === "month" ? (
            <>
              {unscheduled.length > 0 && (
                <div className="td-utray" aria-label="未排期待办">
                  <span className="td-ulab">
                    未排期 <b>{unscheduled.length}</b>
                  </span>
                  <div className="td-uchips">
                    {unscheduled.map((t) => (
                      <button
                        key={t.id}
                        type="button"
                        className={`td-uchip st-${t.status}`}
                        title={`${t.content} — 拖到日历某天排期，或点击编辑`}
                        draggable
                        onDragStart={(e) => {
                          e.dataTransfer.setData(TODO_DND_TYPE, t.id);
                          e.dataTransfer.effectAllowed = "move";
                        }}
                        onClick={() => openEntry(t)}
                      >
                        <span className="td-uc-t">{t.content}</span>
                      </button>
                    ))}
                  </div>
                </div>
              )}
              <TodoMonth
                todos={visible}
                goals={doc.goals}
                cursor={cursor}
                popDate={popDate}
                onPop={setPopDate}
                onOpen={openEntry}
                onQuickAdd={quickAdd}
                onCycle={cycleStatus}
                arm={arm}
                armOrRun={armOrRun}
                onDelete={(id) =>
                  mutate((d) => {
                    d.todos = d.todos.filter((t) => t.id !== id);
                    return d;
                  })
                }
                onDropTodo={dropTodoOn}
              />
            </>
          ) : (
            <TodoGantt todos={visible} goals={doc.goals} onOpen={openEntry} />
          )}
          {doc.todos.length === 0 && (
            <div className="td-empty">
              还没有待办 — 点「+ 待办」、日历格子快速添加，或「从会话填充」把会话拿进来。
            </div>
          )}
        </div>
      </div>

      {draft && (
        <TodoModal
          draft={draft}
          isNew={draftNew}
          goals={doc.goals}
          sessions={sessions}
          selectedId={selectedId}
          onChange={setDraft}
          onSave={saveDraft}
          onDelete={deleteDraft}
          onClose={() => {
            setDraft(null);
            setDraftNew(false);
          }}
          onSelect={(id) => {
            onSelect(id);
            onClose(); // the jump lands on the session — reveal it
          }}
        />
      )}
        </div>
      </div>
    </div>
  );
}
