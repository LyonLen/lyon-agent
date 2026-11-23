import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useEffect, useState } from "react";
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

// Workspace-fold persistence. The sessions pane REMOUNTS on every session
// switch — the tiling layout hydrates per-session with fresh leaf nodeIds
// (TilingWorkspace keys panes by nodeId) — so a component-local fold set
// reset to "all expanded" on every row click ("点击后仍然会导致别的
// project 展开"). The fold set is a sidebar view preference, not session
// state: it lives in localStorage (the lyon_layout_v1 pattern) so it
// survives both the remount and a reload.
const FOLD_KEY = "lyon_ws_fold_v1";

function loadFolded(): ReadonlySet<string> {
  try {
    const raw = localStorage.getItem(FOLD_KEY);
    if (raw) {
      const arr: unknown = JSON.parse(raw);
      if (Array.isArray(arr)) {
        return new Set(arr.filter((x): x is string => typeof x === "string"));
      }
    }
  } catch {
    // corrupt JSON or storage unavailable — treat as all expanded
  }
  return new Set();
}

export default function SessionList(props: {
  sessions: Session[];
  saved: SnapshotRef[];
  // Session ids parked on an approval card — their badge renders amber
  // so an ask in a background session is spot-able without switching.
  approving: ReadonlySet<string>;
  selectedId: string;
  restoring: string;
  onSelect: (id: string) => void;
  onCreate: () => void;
  onRefresh: () => void;
  onRestore: (path: string) => void;
  onDelete: (id: string) => void;
  // 拿会话填充待办: push this session into the global todolist (App
  // queues it; TodosPane — the single doc writer — consumes the queue).
  onFill: (s: Session) => void;
}) {
  // Two-click delete (the memory-panel forget pattern): first click arms
  // the prompt, a second confirms; the prompt self-expires.
  const [confirmDel, setConfirmDel] = useState("");

  // Collapsed workspace groups, keyed by the group header's label — a
  // click on the header folds every session under that path (the sidebar
  // can hold dozens of rows per workspace; the fold is pure view state,
  // sessions and the selection are untouched). Seeded from (and written
  // back to) localStorage — see FOLD_KEY above for why component state
  // alone cannot hold it across the session-switch remount.
  const [collapsed, setCollapsed] = useState<ReadonlySet<string>>(loadFolded);
  useEffect(() => {
    try {
      localStorage.setItem(FOLD_KEY, JSON.stringify([...collapsed]));
    } catch {
      // storage blocked — folds just don't survive the next remount
    }
  }, [collapsed]);
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
          {s.busy || props.approving.has(s.id) ? (
            <>
              {" · "}
              <span
                className={"run-badge" + (props.approving.has(s.id) ? " wait" : "")}
                title={
                  props.approving.has(s.id)
                    ? "waiting for your approval — click to open"
                    : undefined
                }
              >
                running
              </span>
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
      {/* Actions overlay the row's right edge (out of flow) — the title
          keeps the full row width; hidden state costs zero space and
          zero hit area. */}
      <div className="session-acts">
        <button
          className="ghost session-fill"
          title="加入全局待办（会话填充）"
          onClick={() => props.onFill(s)}
        >
          +
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
    </div>
  );

  return (
    <div className="session-list">
      <div className="row">
        <button className="new-session" onClick={props.onCreate}>
          + new session
        </button>
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
