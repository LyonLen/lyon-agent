// My own UI formatters — the same shapes the TUI uses, so a duration or a
// token count reads identically on both surfaces.

// fmtDuration renders a tool/prompt duration: milliseconds below one second,
// seconds with two decimals up to a minute (1.22s), then minutes+seconds —
// "439085ms" was both unreadable and misleading about how long it felt.
export function fmtDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return "";
  if (ms < 1000) return `${Math.round(ms)}ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(2)}s`;
  const s = Math.round(ms / 1000);
  return `${Math.floor(s / 60)}m${String(s % 60).padStart(2, "0")}s`;
}

// fmtCount is thousands-separated for the big numbers (1,000,000 reads;
// 1000000 does not) — identity for anything small.
export function fmtCount(n: number): string {
  return Number.isFinite(n) ? n.toLocaleString("en-US") : String(n);
}