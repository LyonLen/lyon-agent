import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useState } from "react";
import type { Session } from "../gen/lyon/v1/session_pb";
import type { SnapshotRef } from "../gen/lyon/v1/dashboard_pb";

// Short local timestamp — the sidebar only needs "when", to the minute.
function shortWhen(d: Date): string {
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

// wsName is the workspace root's basename — the group header label (the
// full path rides the tooltip).
function wsName(ws: string): string {
  return ws.split(/[\\/]/).filter(Boolean).pop() || "workspace";
}

export default function SessionList(props: {
  sessions: Session[];
  saved: SnapshotRef[];
  selectedId: string;
  restoring: string;
  onSelect: (id: string) => void;
  onCreate: () => void;
  onRefresh: () => void;
  onRestore: (path: string) => void;
  onDelete: (id: string) => void;
}) {
  // Two-click delete (the memory-panel forget pattern): first click arms
  // the prompt, a second confirms; the prompt self-expires.
  const [confirmDel, setConfirmDel] = useState("");

  // Collapsed workspace groups, keyed by the group header's label — a
  // click on the header folds every session under that path (the sidebar
  // can hold dozens of rows per workspace; the fold is pure view state,
  // sessions and the selection are untouched).
  const [collapsed, setCollapsed] = useState<ReadonlySet<string>>(new Set());
  const toggleGroup = (key: string) =>
    setCollapsed((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });

  // Sessions grouped by workspace (会话按工作区分开): insertion order
  // follows the server's recency sort, so the most recently touched
  // workspace leads and recency holds within each group.
  const groups: Array<[string, Session[]]> = [];
  const byWs = new Map<string, Session[]>();
  for (const s of props.sessions) {
    const arr = byWs.get(s.workspace);
    if (arr) {
      arr.push(s);
    } else {
      const fresh: Session[] = [s];
      byWs.set(s.workspace, fresh);
      groups.push([s.workspace, fresh]);
    }
  }

  const rowOf = (s: Session) => (
    <div key={s.id} className="session-row">
      <button
        className={
          "session-item" + (s.id === props.selectedId ? " selected" : "")
        }
        onClick={() => props.onSelect(s.id)}
      >
        <span className="title">{s.title || "(untitled)"}</span>
        <span className="meta">
          {s.mode}
          {s.busy ? (
            <>
              {" · "}
              <span className="run-badge">running</span>
            </>
          ) : null}
          {s.queueLen > 0 ? ` · queued ${s.queueLen}` : ""}
          {/* turn count read as noise ("318 turns" says nothing the
              reader can act on) — when the seat was last touched does. */}
          {s.lastActive
            ? ` · ${shortWhen(timestampDate(s.lastActive))}`
            : ""}
        </span>
      </button>
      <button
        className="ghost danger session-del"
        title="delete this session"
        onClick={() => {
          if (confirmDel !== s.id) {
            setConfirmDel(s.id);
            window.setTimeout(
              () => setConfirmDel((c) => (c === s.id ? "" : c)),
              3000,
            );
            return;
          }
          setConfirmDel("");
          props.onDelete(s.id);
        }}
      >
        {confirmDel === s.id ? "确认?" : "×"}
      </button>
    </div>
  );

  return (
    <div className="session-list">
      <div className="row">
        <button onClick={props.onCreate}>+ new session</button>
        <button className="ghost" onClick={props.onRefresh}>
          ↻
        </button>
      </div>
      {groups.map(([ws, items]) => {
        const key = ws || "(default)";
        const open = !collapsed.has(key);
        // One workspace per group — any session's flag speaks for all.
        const repo = items.some((s) => s.gitRepo);
        return (
          <div key={key}>
            <button
              type="button"
              className="list-section ws-head"
              title={
                (ws || "(default)") +
                (repo ? " · git repository" : "") +
                " — click to fold/unfold"
              }
              onClick={() => toggleGroup(key)}
            >
              <span className={"chev" + (open ? " open" : "")}>▸</span>
              {wsName(ws)}
              {repo && <span className="ws-git">git</span>}
            </button>
            {open && items.map(rowOf)}
          </div>
        );
      })}
      {props.sessions.length === 0 && (
        <div className="placeholder">no sessions yet</div>
      )}
      {props.saved.length > 0 && (
        <>
          <div className="list-section">saved</div>
          {props.saved.map((r) => {
            const busyRestoring = props.restoring === r.path;
            return (
              <button
                key={r.path}
                className={"session-item saved" + (busyRestoring ? " selected" : "")}
                disabled={props.restoring !== "" && !busyRestoring}
                onClick={() => props.onRestore(r.path)}
              >
                <span className="title">{r.title || "(untitled)"}</span>
                <span className="meta">
                  {busyRestoring
                    ? "restoring…"
                    : `${r.messages} messages${r.savedAt ? ` · ${shortWhen(timestampDate(r.savedAt))}` : ""}`}
                </span>
              </button>
            );
          })}
        </>
      )}
    </div>
  );
}
