// Day-granular date helpers for the todos pane (month / gantt views).
// All dates are local-time YYYY-MM-DD strings; weeks start Monday (the
// weekcal design package's grid order).

export function ymd(d: Date): string {
  const m = String(d.getMonth() + 1).padStart(2, "0");
  const day = String(d.getDate()).padStart(2, "0");
  return `${d.getFullYear()}-${m}-${day}`;
}

// parseYmd rejects impossible dates (2026-02-30) — JS would silently
// roll them into March, which must never land in a gantt bar.
export function parseYmd(s: string): Date | null {
  if (!/^\d{4}-\d{2}-\d{2}$/.test(s)) return null;
  const [y, m, d] = s.split("-").map(Number);
  const dt = new Date(y, m - 1, d);
  if (dt.getFullYear() !== y || dt.getMonth() !== m - 1 || dt.getDate() !== d) return null;
  return dt;
}

export function addDays(d: Date, n: number): Date {
  const x = new Date(d);
  x.setDate(x.getDate() + n);
  return x;
}

// startOfWeek: Monday of d's week (local midnight).
export function startOfWeek(d: Date): Date {
  const x = new Date(d.getFullYear(), d.getMonth(), d.getDate());
  const dow = (x.getDay() + 6) % 7; // Mon=0 … Sun=6
  return addDays(x, -dow);
}

export function todayYmd(): string {
  return ymd(new Date());
}

// daysBetween(a, b) = whole days from a to b (b − a). Math.round, not
// floor: DST shifts the ms delta off a whole day by an hour.
export function daysBetween(a: string, b: string): number {
  const da = parseYmd(a);
  const db = parseYmd(b);
  if (!da || !db) return 0;
  return Math.round((db.getTime() - da.getTime()) / 86400000);
}

// isWeekendYmd: Sat/Sun for a valid YYYY-MM-DD (invalid → false).
export function isWeekendYmd(s: string): boolean {
  const d = parseYmd(s);
  if (!d) return false;
  const dow = d.getDay();
  return dow === 0 || dow === 6;
}
