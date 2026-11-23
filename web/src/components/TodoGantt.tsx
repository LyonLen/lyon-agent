// TodoGantt — day-granular timeline for the todos pane. Auto-fit range
// (data ±3d around today, today ±14d when nothing is dated), 40px/day,
// sticky 170px label column, weekend column tint, today line drawn
// per-track so it survives horizontal scroll, goal sections with a
// progress rule, undated rows collected at the bottom. Interactions
// (the 甘特图同理 report): double-click a track adds — an undated row
// gets scheduled that day, a dated row opens an inline quick-add;
// press-drag a bar's body moves the whole span (day-snapped); the
// bar's left/right edges resize the span.

import {
  useMemo,
  useRef,
  useState,
  type MouseEvent as RMouseEvent,
  type PointerEvent as RPointerEvent,
} from "react";
import { addDays, daysBetween, isWeekendYmd, parseYmd, todayYmd, ymd } from "../cal";
import { goalColor, spanOf, STATUS_GLYPH, type TodoEntry, type TodoGoal } from "../todos";

const DAY_W = 40;

type Props = {
  todos: TodoEntry[]; // already filtered by the pane's search/goal rail
  goals: TodoGoal[];
  onOpen: (t: TodoEntry) => void;
  // Move/schedule — same contract as the month grid's drop: undated →
  // single day on that date, a span keeps its length and shifts.
  onDropTodo: (id: string, date: string) => void;
  onResize: (id: string, side: "start" | "end", date: string) => void;
  onQuickAdd: (date: string, content: string, goalId?: string) => void;
};

// Live gesture on a bar. move drags the whole span (pixel-delta preview,
// commit snaps to days); rz drags one edge (hovered day index preview).
// dirty gates the commit: a press with no meaningful change is a plain
// click / silent release.
type GDrag =
  | {
      kind: "move";
      id: string;
      origIdx: number;
      spanLen: number;
      startX: number;
      dx: number;
      dirty: boolean;
    }
  | {
      kind: "rz";
      id: string;
      edge: "l" | "r";
      startIdx: number;
      endIdx: number;
      targetIdx: number;
      dirty: boolean;
    };

type Section = {
  key: string;
  title: string;
  color: string;
  rows: TodoEntry[]; // dated rows laid on the timeline
  total: number; // all visible entries of this group (rail-parity count)
  done: number;
};

export default function TodoGantt({
  todos,
  goals,
  onOpen,
  onDropTodo,
  onResize,
  onQuickAdd,
}: Props) {
  const today = todayYmd();
  const [gd, setGd] = useState<GDrag | null>(null);
  // Inline quick-add opened by double-clicking a dated row's track —
  // anchored to the row (goalId) so the new entry lands in its section.
  const [addAt, setAddAt] = useState<{
    rowId: string;
    date: string;
    goalId: string;
    x: number; // px offset inside the track
  } | null>(null);
  const [addTxt, setAddTxt] = useState("");
  // A moved bar ends with a click on itself (down and up both land on
  // it — the preview follows the pointer). Swallow that synthetic click
  // briefly so releasing a move never opens the detail modal.
  const suppressUntil = useRef(0);

  const { cols, weekendIdx, todayOff } = useMemo(() => {
    let minS = ymd(addDays(new Date(), -3));
    let maxS = ymd(addDays(new Date(), 3));
    for (const t of todos) {
      const span = spanOf(t);
      if (!span) continue;
      if (span[0] < minS) minS = span[0];
      if (span[1] > maxS) maxS = span[1];
    }
    const start = parseYmd(minS)!;
    const n = daysBetween(minS, maxS) + 1;
    const list = Array.from({ length: n }, (_, i) => ymd(addDays(start, i)));
    const we: number[] = [];
    list.forEach((c, i) => {
      if (isWeekendYmd(c)) we.push(i);
    });
    const off = list.indexOf(today);
    return { cols: list, weekendIdx: we, todayOff: off >= 0 ? off : -1 };
  }, [todos, today]);

  const rangeStart = cols[0];

  // Sections: goals in doc order (dated rows only), then 未分组 dated,
  // then 未排期 — undated entries keep their goal dot for context.
  const { sections, undated } = useMemo(() => {
    const dated = todos.filter((t) => t.start);
    const und = todos.filter((t) => !t.start);
    const byGoal = new Map<string, TodoEntry[]>();
    for (const t of dated) {
      const k = t.goalId || "";
      const arr = byGoal.get(k);
      if (arr) arr.push(t);
      else byGoal.set(k, [t]);
    }
    const allOf = (gid: string) => todos.filter((t) => t.goalId === gid);
    const stat = (list: TodoEntry[]) => ({
      total: list.length,
      done: list.filter((t) => t.status === "completed").length,
    });
    const secs: Section[] = [];
    for (const g of goals) {
      const rows = byGoal.get(g.id);
      if (!rows || rows.length === 0) continue;
      const st = stat(allOf(g.id));
      secs.push({ key: g.id, title: g.title, color: goalColor(g), rows, ...st });
    }
    const ungroupedRows = byGoal.get("");
    if (ungroupedRows && ungroupedRows.length > 0) {
      const st = stat(allOf(""));
      secs.push({
        key: "__none",
        title: "未分组",
        color: "var(--fg-faint)",
        rows: ungroupedRows,
        ...st,
      });
    }
    return { sections: secs, undated: und };
  }, [todos, goals]);

  const colorOf = useMemo(() => {
    const m = new Map<string, string>();
    for (const g of goals) m.set(g.id, goalColor(g));
    return m;
  }, [goals]);

  if (todos.length === 0) {
    return <div className="td-gempty">当前筛选下没有待办。</div>;
  }

  const clamp = (v: number, lo: number, hi: number) => Math.max(lo, Math.min(v, hi));

  // Day index under a clientX inside a track element.
  const colAtX = (clientX: number, track: HTMLElement) =>
    clamp(Math.floor((clientX - track.getBoundingClientRect().left) / DAY_W), 0, cols.length - 1);

  const barGeom = (t: TodoEntry) => {
    const span = spanOf(t);
    if (!span) return null;
    const left = daysBetween(rangeStart, span[0]) * DAY_W;
    const width = (daysBetween(span[0], span[1]) + 1) * DAY_W - 3;
    const overdue = span[1] < today && t.status !== "completed";
    return { left, width: Math.max(width, 10), overdue };
  };

  // ── bar gestures (pointer events; the bar itself is not draggable) ──
  const barDown = (e: RPointerEvent<HTMLElement>, id: string, span: [string, string]) => {
    if (e.button !== 0) return;
    try {
      e.currentTarget.setPointerCapture(e.pointerId);
    } catch {
      // best-effort — without capture a drag that leaves the bar stops
    }
    setGd({
      kind: "move",
      id,
      origIdx: daysBetween(rangeStart, span[0]),
      spanLen: daysBetween(span[0], span[1]) + 1,
      startX: e.clientX,
      dx: 0,
      dirty: false,
    });
  };

  const barMove = (e: RPointerEvent<HTMLElement>) => {
    if (!gd) return;
    if (gd.kind === "move") {
      const rawLeft = gd.origIdx * DAY_W + (e.clientX - gd.startX);
      const left = clamp(rawLeft, 0, (cols.length - gd.spanLen) * DAY_W);
      const dx = left - gd.origIdx * DAY_W;
      const dirty = gd.dirty || Math.abs(e.clientX - gd.startX) > 3;
      if (dx === gd.dx && dirty === gd.dirty) return;
      setGd({ ...gd, dx, dirty });
    } else {
      const track = e.currentTarget.closest(".td-g-track");
      if (!(track instanceof HTMLElement)) return;
      let idx = colAtX(e.clientX, track);
      // an edge can never cross the other one — overshoot pins there
      if (gd.edge === "r") idx = Math.max(idx, gd.startIdx);
      else idx = Math.min(idx, gd.endIdx);
      const origEdge = gd.edge === "r" ? gd.endIdx : gd.startIdx;
      const dirty = gd.dirty || idx !== origEdge;
      if (idx === gd.targetIdx && dirty === gd.dirty) return;
      setGd({ ...gd, targetIdx: idx, dirty });
    }
  };

  const barUp = () => {
    if (!gd) return;
    const st = gd;
    setGd(null);
    if (st.kind === "move") {
      if (!st.dirty) return; // plain click → the onClick opens the modal
      suppressUntil.current = Date.now() + 350;
      const finalLeft = st.origIdx * DAY_W + st.dx;
      const newIdx = clamp(
        Math.round(finalLeft / DAY_W),
        0,
        cols.length - st.spanLen,
      );
      // Always call: snapping back onto the current span is the month
      // grid's "仍在 … 内" no-op flash, not silence.
      onDropTodo(st.id, cols[newIdx]);
    } else {
      if (!st.dirty) return;
      onResize(st.id, st.edge === "l" ? "start" : "end", cols[st.targetIdx]);
    }
  };

  const dragCancel = () => setGd(null);

  // Edge handle: kill the compat click (no modal from an edge grab) and
  // stop the bar's own down-handler from starting a move. Moves/up ride
  // the capture target back up to the bar's handlers — one dispatch.
  const hDown = (
    e: RPointerEvent<HTMLElement>,
    edge: "l" | "r",
    id: string,
    span: [string, string],
  ) => {
    if (e.button !== 0) return;
    e.preventDefault();
    e.stopPropagation();
    try {
      e.currentTarget.setPointerCapture(e.pointerId);
    } catch {
      // best-effort
    }
    const startIdx = daysBetween(rangeStart, span[0]);
    const endIdx = daysBetween(rangeStart, span[1]);
    setGd({
      kind: "rz",
      id,
      edge,
      startIdx,
      endIdx,
      targetIdx: edge === "r" ? endIdx : startIdx,
      dirty: false,
    });
  };

  const trackDoubleClick = (e: RMouseEvent<HTMLElement>, t: TodoEntry) => {
    if (e.target instanceof Element && e.target.closest(".td-bar")) return;
    const idx = colAtX(e.clientX, e.currentTarget);
    const date = cols[idx];
    if (!t.start) {
      // undated row: the double-click IS the schedule (the未排期 box
      // in the report) — one gesture, no dialog.
      onDropTodo(t.id, date);
      return;
    }
    setAddAt({ rowId: t.id, date, goalId: t.goalId, x: idx * DAY_W });
    setAddTxt("");
  };

  const renderTrackBg = () => (
    <>
      {weekendIdx.map((i) => (
        <i key={i} className="td-g-we" style={{ left: i * DAY_W, width: DAY_W }} />
      ))}
      {todayOff >= 0 && <i className="td-g-today" style={{ left: todayOff * DAY_W }} />}
    </>
  );

  const renderRow = (t: TodoEntry) => {
    const g = barGeom(t);
    const color = t.goalId ? colorOf.get(t.goalId) ?? "var(--fg-faint)" : "var(--fg-faint)";
    const span = spanOf(t);
    // Live preview: the dragged bar follows the pointer (move) or the
    // hovered day (resize) without touching the document until release.
    let left = g?.left ?? 0;
    let width = g?.width ?? 0;
    let dragCls = "";
    if (g && gd && gd.id === t.id) {
      if (gd.kind === "move") {
        left = g.left + gd.dx;
        dragCls = " moving";
      } else if (gd.edge === "r") {
        width = Math.max((gd.targetIdx - gd.startIdx + 1) * DAY_W - 3, 10);
        dragCls = " rz";
      } else {
        left = gd.targetIdx * DAY_W;
        width = Math.max((gd.endIdx - gd.targetIdx + 1) * DAY_W - 3, 10);
        dragCls = " rz";
      }
    }
    return (
      <div key={t.id} className="td-g-row">
        <button
          type="button"
          className="td-g-lab"
          title={t.content}
          onClick={() => onOpen(t)}
        >
          <span className={`td-g-glyph st-${t.status}`}>{STATUS_GLYPH[t.status]}</span>
          <i className="td-g-dot" style={{ background: color }} />
          <span className="td-g-txt">{t.content}</span>
        </button>
        <div className="td-g-track" onDoubleClick={(e) => trackDoubleClick(e, t)}>
          {renderTrackBg()}
          {g && span && (
            <button
              type="button"
              className={[
                "td-bar",
                `st-${t.status}`,
                g.overdue ? "over" : "",
                dragCls,
              ]
                .filter(Boolean)
                .join(" ")}
              style={{ left, width, ["--gc" as string]: color }}
              title={`${t.content} · ${t.start} → ${t.end || t.start}`}
              onPointerDown={(e) => barDown(e, t.id, span)}
              onPointerMove={barMove}
              onPointerUp={barUp}
              onPointerCancel={dragCancel}
              onClick={(e) => {
                if (Date.now() < suppressUntil.current) {
                  e.stopPropagation();
                  return;
                }
                onOpen(t);
              }}
            >
              <i
                className="td-h l"
                title="拖动改开始日期"
                onPointerDown={(e) => hDown(e, "l", t.id, span)}
                onClick={(e) => e.stopPropagation()}
              />
              <i
                className="td-h r"
                title="拖动改结束日期"
                onPointerDown={(e) => hDown(e, "r", t.id, span)}
                onClick={(e) => e.stopPropagation()}
              />
              {width >= 90 && <span className="td-bartxt">{t.content}</span>}
            </button>
          )}
          {addAt?.rowId === t.id && (
            <form
              className="td-g-add"
              style={{
                left: clamp(addAt.x, 0, Math.max(0, cols.length * DAY_W - 210)),
              }}
              onClick={(e) => e.stopPropagation()}
              onDoubleClick={(e) => e.stopPropagation()}
              onKeyDown={(e) => {
                if (e.key === "Escape") {
                  e.stopPropagation(); // close THIS, not the overlay
                  setAddAt(null);
                }
              }}
              onSubmit={(e) => {
                e.preventDefault();
                onQuickAdd(addAt.date, addTxt, addAt.goalId);
                setAddTxt("");
              }}
              onBlur={() => setAddAt(null)}
            >
              <input
                autoFocus
                placeholder={`快速添加 ${addAt.date}，回车确认…`}
                value={addTxt}
                onChange={(e) => setAddTxt(e.target.value)}
              />
            </form>
          )}
        </div>
      </div>
    );
  };

  const renderSection = (sec: Section) => {
    const ratio = sec.total > 0 ? sec.done / sec.total : 0;
    // goal span rule: min start → max end of this section's dated rows
    let spanL = -1;
    let spanR = -1;
    for (const t of sec.rows) {
      const g = barGeom(t);
      if (!g) continue;
      if (spanL < 0 || g.left < spanL) spanL = g.left;
      const r = g.left + g.width;
      if (r > spanR) spanR = r;
    }
    return (
      <div key={sec.key} className="td-g-sec">
        <div className="td-g-secrow">
          <div className="td-g-lab head">
            <i className="td-g-dot" style={{ background: sec.color }} />
            <span className="td-g-txt">{sec.title}</span>
            <span className="td-g-prog">
              {sec.done}/{sec.total}
            </span>
          </div>
          <div className="td-g-track">
            {renderTrackBg()}
            {spanL >= 0 && (
              <div className="td-g-rule" style={{ left: spanL, width: spanR - spanL }}>
                <i style={{ width: `${Math.round(ratio * 100)}%` }} />
              </div>
            )}
          </div>
        </div>
        {sec.rows.map(renderRow)}
      </div>
    );
  };

  return (
    <div className="td-gantt">
      <div className="td-g-inner" style={{ width: 170 + cols.length * DAY_W }}>
        <div className="td-g-head">
          <div className="td-g-lab head top">任务</div>
          <div className="td-g-hcols">
            {cols.map((c, i) => {
              const d = parseYmd(c)!;
              const day = d.getDate();
              const first = day === 1 || i === 0;
              return (
                <div key={c} className={`td-g-hc${isWeekendYmd(c) ? " we" : ""}`}>
                  <span className="td-g-mo">{first ? `${d.getMonth() + 1}月` : ""}</span>
                  <span className="td-g-dn">{day}</span>
                </div>
              );
            })}
          </div>
        </div>

        {sections.map(renderSection)}

        {undated.length > 0 && (
          <div className="td-g-sec">
            <div className="td-g-secrow">
              <div className="td-g-lab head">
                <i className="td-g-dot" style={{ background: "var(--fg-faint)" }} />
                <span className="td-g-txt">未排期</span>
                <span className="td-g-prog">{undated.length}</span>
              </div>
              <div className="td-g-track">{renderTrackBg()}</div>
            </div>
            {undated.map(renderRow)}
          </div>
        )}
      </div>
    </div>
  );
}
