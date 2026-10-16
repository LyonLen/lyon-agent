import type { Session } from "../gen/lyon/v1/session_pb";

export default function SessionList(props: {
  sessions: Session[];
  selectedId: string;
  onSelect: (id: string) => void;
  onCreate: () => void;
  onRefresh: () => void;
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
    </div>
  );
}
