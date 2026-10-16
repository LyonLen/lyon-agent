import { timestampDate } from "@bufbuild/protobuf/wkt";
import type { Session } from "../gen/lyon/v1/session_pb";
import type { SnapshotRef } from "../gen/lyon/v1/dashboard_pb";

// Short local timestamp — the sidebar only needs "when", to the minute.
function shortWhen(d: Date): string {
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
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
}) {
  return (
    <div className="session-list">
      <div className="row">
        <button onClick={props.onCreate}>+ new session</button>
        <button className="ghost" onClick={props.onRefresh}>
          ↻
        </button>
      </div>
      {props.sessions.map((s) => (
        <button
          key={s.id}
          className={
            "session-item" + (s.id === props.selectedId ? " selected" : "")
          }
          onClick={() => props.onSelect(s.id)}
        >
          <span className="title">{s.title || "(untitled)"}</span>
          <span className="meta">
            {s.mode}
            {s.busy ? " · running" : ""}
            {s.queueLen > 0 ? ` · queued ${s.queueLen}` : ""} · {s.turns} turns
          </span>
        </button>
      ))}
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
