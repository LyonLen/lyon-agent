// The web key table — one source for the resident legend footer and the
// /help output. Mirrors TUI's idleLegend (cmd/lyon/tui.go:1023-1034)
// minus the terminal-only chords: ctrl+t is reserved by browsers (new
// tab, not interceptable), pgup/pgdn scroll natively, alt+e evolve is a
// TUI-only panel.

export interface KeyRow {
  keys: string;
  what: string;
}

export const KEY_TABLE: KeyRow[] = [
  { keys: "enter", what: "send · queue while busy" },
  { keys: "shift+tab", what: "cycle mode default → plan → bypass" },
  { keys: "/", what: "command panel — ←/→ switch category" },
  { keys: "@", what: "file completion — Tab/Enter insert" },
  { keys: "↑↓", what: "prompt history (empty line)" },
  { keys: "alt+←→", what: "widen / narrow the focused pane" },
  { keys: "alt+↑↓", what: "grow / shrink it vertically" },
  { keys: "ctrl+o", what: "expand / collapse thinking blocks" },
  { keys: "esc", what: "close the output drawer" },
  { keys: "y n a", what: "answer an approval (not typing)" },
];

// One-line resident legend (footer). Keep it short — the full table is
// in /help.
export const LEGEND =
  "enter send · shift+tab mode · / commands · @ files · alt+←→ width · alt+↑↓ height · ctrl+o thinking · esc output";
