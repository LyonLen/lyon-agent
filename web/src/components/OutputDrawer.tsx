// OutputDrawer — the chat pane's bottom drawer for multi-line command
// results (TUI cmdOutView parity, cmd/lyon/tui.go:1718): head-clamped
// to 10 rows so a long table can never swallow the transcript, a
// "+N more" hint keeps the tail discoverable, esc closes it app-wide.

const CMD_OUT_MAX_ROWS = 10;

export default function OutputDrawer({
  lines,
  onClose,
}: {
  lines: string[];
  onClose: () => void;
}) {
  const head = lines.slice(0, CMD_OUT_MAX_ROWS);
  const more = lines.length - head.length;
  return (
    <div className="out-drawer">
      <div className="out-head">
        <span className="out-title">── output ──</span>
        <span className="out-count">{lines.length} lines</span>
        <button className="ghost" onClick={onClose}>
          esc ✕
        </button>
      </div>
      <div className="out-body">
        {head.map((l, i) => (
          <div key={i} className="out-line">
            {l}
          </div>
        ))}
        {more > 0 && <div className="out-more">…(+{more} more)</div>}
      </div>
    </div>
  );
}
