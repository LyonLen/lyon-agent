// TodoGantt — day-granular timeline for the todos pane. Auto-fit range
// (data ±3d around today, today ±14d when nothing is dated), 40px/day,
// sticky 170px label column, weekend column tint, today line drawn
// per-track so it survives horizontal scroll, goal sections with a
// progress rule, undated rows collected at the bottom. No drag v1.

import { useMemo } from "react";
import { addDays, daysBetween, isWeekendYmd, parseYmd, todayYmd, ymd } from "../cal";
import { goalColor, spanOf, STATUS_GLYPH, type TodoEntry, type TodoGoal } from "../todos";

const DAY_W = 40;

type Props = {
  todos: TodoEntry[]; // already filtered by the pane's search/goal rail
  goals: TodoGoal[];
  onOpen: (t: TodoEntry) => void;
};

type Section = {
  key: string;
  title: string;
  color: string;
  rows: TodoEntry[]; // dated rows laid on the timeline
  total: number; // all visible entries of this group (rail-parity count)
  done: number;
};

export default function TodoGantt({ todos, goals, onOpen }: Props) {
  const today = todayYmd();

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

  const barGeom = (t: TodoEntry) => {
    const span = spanOf(t);
    if (!span) return null;
    const left = daysBetween(rangeStart, span[0]) * DAY_W;
    const width = (daysBetween(span[0], span[1]) + 1) * DAY_W - 3;
    const overdue = span[1] < today && t.status !== "completed";
    return { left, width: Math.max(width, 10), overdue };
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
        <div className="td-g-track">
          {renderTrackBg()}
          {g && (
            <button
              type="button"
              className={[
                "td-bar",
                `st-${t.status}`,
                g.overdue ? "over" : "",
              ]
                .filter(Boolean)
                .join(" ")}
              style={{ left: g.left, width: g.width, ["--gc" as string]: color }}
              title={`${t.content} · ${t.start} → ${t.end || t.start}`}
              onClick={() => onOpen(t)}
            >
              {g.width >= 90 && <span className="td-bartxt">{t.content}</span>}
            </button>
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
