// TodoModal — one entry's editor (the weekcal package's modal anatomy,
// lyon dark theme): status segmented control, title, goal select, day
// range, session chips (jump / unlink / link current), note, two-click
// delete. Edits live in the parent's draft; only 保存 commits (full-doc
// save). ESC / backdrop / × close without saving.

import { useEffect, useRef, useState } from "react";
import type { Session } from "../gen/lyon/v1/session_pb";
import { STATUS_GLYPH, type TodoEntry, type TodoGoal, type TodoStatus } from "../todos";

type Props = {
  draft: TodoEntry;
  isNew: boolean;
  goals: TodoGoal[];
  sessions: Session[];
  selectedId: string;
  onChange: (d: TodoEntry) => void;
  onSave: () => void;
  onDelete: () => void;
  onClose: () => void;
  onSelect: (id: string) => void; // jump to a linked session
};

const STATUS_OPTIONS: Array<{ v: TodoStatus; label: string }> = [
  { v: "pending", label: "待办" },
  { v: "in_progress", label: "进行中" },
  { v: "completed", label: "完成" },
];

export default function TodoModal({
  draft,
  isNew,
  goals,
  sessions,
  selectedId,
  onChange,
  onSave,
  onDelete,
  onClose,
  onSelect,
}: Props) {
  const [armDel, setArmDel] = useState(false);
  const titleRef = useRef<HTMLInputElement>(null);
  const armTimer = useRef(0);

  useEffect(() => {
    if (isNew) titleRef.current?.focus();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("keydown", onKey);
      window.clearTimeout(armTimer.current);
    };
  }, [isNew, onClose]);

  const titleOf = (sid: string): string => {
    const s = sessions.find((x) => x.id === sid);
    if (s?.title) return s.title;
    return sid.length > 10 ? sid.slice(0, 8) + "…" : sid;
  };

  const set = (patch: Partial<TodoEntry>) => onChange({ ...draft, ...patch });

  const unlink = (sid: string) =>
    set({ sessionIds: draft.sessionIds.filter((x) => x !== sid) });

  const linkCurrent = () => {
    if (!selectedId || draft.sessionIds.includes(selectedId)) return;
    set({ sessionIds: [...draft.sessionIds, selectedId] });
  };

  const clickDelete = () => {
    if (armDel) {
      onDelete();
      return;
    }
    setArmDel(true);
    armTimer.current = window.setTimeout(() => setArmDel(false), 3000);
  };

  return (
    <div className="dlg-backdrop td-mback" onClick={onClose}>
      <div
        className="td-modal"
        role="dialog"
        aria-modal="true"
        aria-label={isNew ? "新建待办" : "编辑待办"}
        onClick={(e) => e.stopPropagation()}
      >
        <div className="td-mhead">
          <div className="td-seg" role="group" aria-label="状态">
            {STATUS_OPTIONS.map((o) => (
              <button
                key={o.v}
                type="button"
                className={draft.status === o.v ? "on" : ""}
                onClick={() => set({ status: o.v })}
              >
                {STATUS_GLYPH[o.v]} {o.label}
              </button>
            ))}
          </div>
          <button type="button" className="td-x" aria-label="关闭" onClick={onClose}>
            ×
          </button>
        </div>

        <input
          ref={titleRef}
          className="td-title"
          placeholder="要做什么？"
          value={draft.content}
          maxLength={300}
          onChange={(e) => set({ content: e.target.value })}
          onKeyDown={(e) => {
            if (e.key === "Enter" && !e.shiftKey) {
              e.preventDefault();
              onSave();
            }
          }}
        />

        <div className="td-mmeta">
          <label>
            <span>目标</span>
            <select
              value={draft.goalId}
              onChange={(e) => set({ goalId: e.target.value })}
            >
              <option value="">未分组</option>
              {goals.map((g) => (
                <option key={g.id} value={g.id}>
                  {g.title}
                </option>
              ))}
            </select>
          </label>
          <label>
            <span>开始</span>
            <input
              type="date"
              value={draft.start}
              onChange={(e) => {
                const start = e.target.value;
                // end follows start when it would fall out of range
                const end = !start ? "" : draft.end && draft.end >= start ? draft.end : start;
                set({ start, end });
              }}
            />
          </label>
          <label>
            <span>结束</span>
            <input
              type="date"
              value={draft.end}
              min={draft.start || undefined}
              disabled={!draft.start}
              onChange={(e) => set({ end: e.target.value })}
            />
          </label>
        </div>

        <div className="td-msessions">
          <div className="td-mkicker">
            关联会话
            <button type="button" className="td-btn sm" onClick={linkCurrent}>
              🔗 关联当前会话
            </button>
          </div>
          {draft.sessionIds.length === 0 ? (
            <div className="td-mhint">没有关联的会话。</div>
          ) : (
            <div className="td-chips-row">
              {draft.sessionIds.map((sid) => (
                <span key={sid} className="td-schip">
                  <button
                    type="button"
                    className="td-sjump"
                    title={sid}
                    onClick={() => onSelect(sid)}
                  >
                    ↳ {titleOf(sid)}
                  </button>
                  <button
                    type="button"
                    className="td-sx"
                    aria-label="取消关联"
                    onClick={() => unlink(sid)}
                  >
                    ×
                  </button>
                </span>
              ))}
            </div>
          )}
        </div>

        <textarea
          className="td-note"
          placeholder="备注（可选）"
          rows={4}
          maxLength={4000}
          value={draft.note}
          onChange={(e) => set({ note: e.target.value })}
        />

        <div className="td-mfoot">
          {!isNew && (
            <button
              type="button"
              className={`td-btn danger${armDel ? " armed" : ""}`}
              onClick={clickDelete}
            >
              {armDel ? "再点一次确认删除" : "删除"}
            </button>
          )}
          <span className="td-mflex" />
          <button type="button" className="td-btn" onClick={onClose}>
            取消
          </button>
          <button type="button" className="td-btn primary" onClick={onSave}>
            保存
          </button>
        </div>
      </div>
    </div>
  );
}
