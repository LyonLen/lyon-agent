import { lazy, Suspense, useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ClipboardEvent as ReactClipboardEvent, ReactNode } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import type { SessionEvent } from "../gen/lyon/v1/event_pb";
import { ModeAction, type WatchPendingResponse } from "../gen/lyon/v1/approval_pb";
import type { SnapshotRef } from "../gen/lyon/v1/dashboard_pb";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import type { Timestamp } from "@bufbuild/protobuf/wkt";
import { errText, fileClient, mindmapClient, sessionClient, settingsClient } from "../api";
import { FileIcon, gitBadge } from "../fileIcons";
import { uploadPaste } from "../paste";
import { parseTodos } from "../todos";
import type { TodoItem } from "../todos";
import { parseNodes } from "../mindmap";
import type { MapNode, MindmapSnap } from "../mindmap";
import { buildSelAsk, SEL_MARKER_RE, stripWireForDisplay } from "../selask";
import { classifySteer } from "../steer";
// (MindMapCard moved to App — the map renders in its own pane, #51)
import type { StepRow } from "../types";
import { todoProgress } from "./PlanPane";
import OutputDrawer from "./OutputDrawer";
import { KEY_TABLE } from "../keys";
import { fmtDuration, fmtStamp } from "../fmt";

// The markdown parser rides its own chunk — Suspense fallback shows the
// raw text until it lands (Phase F split).
const MarkdownView = lazy(() => import("./Markdown"));

// The web slice of TUI's slashCommands table (cmd/lyon/tui.go): session/
// model/panel/misc categories, descriptions mirrored from the terminal
// build. The evolution family (/evolve /exam /contest /route /agents
// /flow) opens the read-only evolve panel (BACKLOG §2.6 web v1) — runs
// stay TUI; unknown tokens fall through to App's runCommand and get a
// toast.
type SlashKind = "session" | "model" | "panel" | "misc";
const SLASH: Array<{ cmd: string; desc: string; kind: SlashKind }> = [
  { cmd: "/new", desc: "start a fresh session", kind: "session" },
  { cmd: "/resume", desc: "pick a saved session (N = restore slot N)", kind: "session" },
  { cmd: "/rollback", desc: "list checkpoints · N restore · save [text] marks one", kind: "session" },
  { cmd: "/undo", desc: "revert last run's file changes (git side-ref)", kind: "session" },
  { cmd: "/clear", desc: "clear the transcript view (history kept)", kind: "session" },
  { cmd: "/mode", desc: "cycle default → plan → bypass", kind: "model" },
  { cmd: "/model", desc: "show the session's effective model", kind: "model" },
  { cmd: "/think", desc: "expand/collapse thinking blocks", kind: "model" },
  { cmd: "/steps", desc: "open|close the audit panel · width N resizes", kind: "panel" },
  { cmd: "/mindmap", desc: "session mindmap — live understanding map", kind: "panel" },
  { cmd: "/memory", desc: "browse project memory — hot toggle, forget", kind: "panel" },
  { cmd: "/evolve", desc: "evolution panel — lineage, seats, exam banks", kind: "panel" },
  { cmd: "/exam", desc: "exam bank & clock (panel view)", kind: "panel" },
  { cmd: "/contest", desc: "contest field (panel view) — run in TUI", kind: "panel" },
  { cmd: "/route", desc: "routing table & leaderboard (panel)", kind: "panel" },
  { cmd: "/agents", desc: "candidate lineage (panel view)", kind: "panel" },
  { cmd: "/flow", desc: "frozen procedures (panel view) — run in TUI", kind: "panel" },
  { cmd: "/market", desc: "task hour market — per-class prices & recent runs", kind: "panel" },
  { cmd: "/remember", desc: "distill this session's learnings into memory now", kind: "misc" },
  { cmd: "/stats", desc: "usage totals — turns, tokens, cache", kind: "misc" },
  { cmd: "/help", desc: "list commands and keys", kind: "misc" },
];

// /remember submits the distill guidance as a prompt — the TUI calls the
// server-side extractor directly (DistillWithReport, no RPC yet), so the
// web steers the model to the same save_memory seam. Scope and caps
// mirror pkg/agent/distill.go's extractor contract.
const REMEMBER_PROMPT =
  "Run the memory distillation pass now: review this conversation so far " +
  "and call save_memory for each durable, non-obvious fact worth keeping " +
  "for future sessions — decisions and their reasons, user preferences, " +
  "conventions, environment gotchas, durable commands. At most 6, " +
  "shorter is better; skip transient task state, files currently in " +
  "flight, and anything obvious from the repo. Reply with one line per " +
  "fact saved.";

// Pick popups (/resume, /rollback): the numbered list shares one row
// shape and one keyboard grammar (↑↓ walk, Enter restore, Esc cancel —
// TUI updatePick parity).
type PickRow = { main: string; sub: string };
type PickState = { title: string; rows: PickRow[]; onPick: (idx: number) => void } | null;

// fmtTs renders a protobuf Timestamp for the drawer/picker rows; the
// undefined branch covers older records without a stamp.
const fmtTs = (t?: Timestamp): string => {
  if (!t) return "—";
  const d = timestampDate(t);
  const p = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
};

// One rendered item on the transcript. The reducer below folds the event
// stream into these blocks — deltas append, boundaries open new ones,
// tool execs attach to their own card by id (parallel calls never
// cross-attach).
type Block =
  | { kind: "user"; text: string }
  // turnMs pins the run's wall time (from prompt_end — submit to final
  // resolution) to the reply. turn_end is one API call: pinning that
  // showed a last-leg "51.38s" on a run that took ten minutes.
  | { kind: "text"; text: string; turnMs?: number; endedAt?: number }
  // t0/t1 = first/last thinking_delta's server TS (ms epoch) — the row's
  // duration is where a run's wall time actually goes (39-minute runs
  // with millisecond tools are thinking time). lc = client receipt of the
  // last delta: a LIVE row ticks past t1 using durations only, so the
  // two clocks never mix. tokens = the call's actual output (turn_end
  // stamps it over the stream estimate).
  | {
      kind: "thinking";
      text: string;
      t0?: number;
      t1?: number;
      lc?: number;
      tokens?: number;
    }
  | {
      kind: "tool";
      name: string;
      toolId: string;
      execId: bigint | null;
      input: string;
      output: string;
      status: "pending" | "running" | "done" | "failed" | "server";
      durationMs: number;
      // Client receipt of tool_exec_start — the running badge's clock
      // (after a 5s grace, "running" alone reads as stuck).
      startedAt?: number;
      error: string;
      server: boolean;
      sub: boolean;
      // Fan-out attribution (#53): which dispatched task this inner step
      // belongs to — chips the row and lets the subtask card count it.
      subId: string;
    }
  // One dispatched subtask's lifecycle (subagent_start…subagent_end):
  // task preview at open, live step count while running, and the answer
  // head the run brought back — the visible half of the main↔sub
  // contract (the full answer still rides the parent tool's result).
  | {
      kind: "subtask";
      subId: string;
      task: string;
      model: string;
      status: "running" | "done" | "failed";
      durationMs: number;
      error: string;
      answer: string;
      calls: string; // end detail, e.g. "12 API calls"
      steps: number; // inner tool executions seen so far
    }
  | { kind: "notice"; text: string; detail?: string }
  | { kind: "error"; text: string }
  // The run's file footprint (server-side git delta at prompt_end,
  // files_changed): rows click straight into the preview pane — new +
  // touched files the run produced, badge-colored like the files panel.
  | { kind: "files"; files: FileCard[] };

// One row of the files_changed card — GitChange's JSON shape off the
// server's diff (pkg/server/filecards.go), so gitBadge applies as-is.
type FileCard = { path: string; xy: string; kind: string; staged: boolean };

// §8 LYON_MINDMAP_ASK (default ON, draft 按 on 写): the GLOBAL kill
// switch for auto-ask rounds — read once from the settings env catalog
// for the whole tab; any failure falls back to the spec default (on).
// The PER-SESSION arm rides on top: flushMmEdit also requires the
// session's mode == "mindmap" (Image #61) — env off kills every mode,
// mindmap mode is where the channel actually lives.
let mmAskFlag: Promise<boolean> | null = null;
const mindmapAskOn = (): Promise<boolean> =>
  (mmAskFlag ??= settingsClient
    .getSettings({})
    .then((res) => {
      const e = (res.env ?? []).find((v) => v.name === "LYON_MINDMAP_ASK");
      if (!e) return true;
      return !["off", "0", "false", "no"].includes((e.value || "").toLowerCase());
    })
    .catch(() => true));



// The live thinking block auto-opens so the reader can watch the run (#47),
// but a long chain-of-thought would bury the feed: once it passes this many
// lines it folds itself and only the "live · ctrl+o to expand" hint remains.
// A global ctrl+o fold still opens it for the ordinary reason. 40 (was 10):
// at 10 a research-heavy turn folded its 思路 after two sentences and the
// reader couldn't follow the plan at all.
const LIVE_THINK_MAX_LINES = 40;

// Line count for the live-thinking fold gate — hand-rolled so the per-delta
// render path doesn't allocate a split array for the whole (growing) text.
function lineCount(s: string): number {
  let n = 1;
  for (let i = 0; i < s.length; i++) if (s.charCodeAt(i) === 10) n++;
  return n;
}

// lastThinking is the newest thinking block's index (-1 when the turn has
// none): the one block the live view keeps open while the seat streams.
function lastThinking(blocks: Block[]): number {
  for (let i = blocks.length - 1; i >= 0; i--) {
    if (blocks[i].kind === "thinking") return i;
  }
  return -1;
}

// textAfter: the run has already produced conclusion text past this row —
// the thinking has said its piece and the answer below is what to read
// now, so the row may fold (#40). Tools after it don't count: with no
// conclusion yet the thinking stays open, at least there's something to
// read.
function textAfter(blocks: Block[], i: number): boolean {
  for (let j = i + 1; j < blocks.length; j++) {
    if (blocks[j].kind === "text") return true;
  }
  return false;
}

// Last index matching pred — hand-rolled: the lib target is ES2022 and
// Array.prototype.findLastIndex is ES2023.
function lastToolIndex(
  blocks: Block[],
  pred: (b: Extract<Block, { kind: "tool" }>) => boolean,
): number {
  for (let i = blocks.length - 1; i >= 0; i--) {
    const b = blocks[i];
    if (b.kind === "tool" && pred(b)) return i;
  }
  return -1;
}

function newToolBlock(ev: SessionEvent): Extract<Block, { kind: "tool" }> {
  return {
    kind: "tool",
    name: ev.toolName || "tool",
    toolId: ev.toolId,
    execId: null,
    // Live tool_use_start carries no input yet (it fills from exec);
    // restored-history events arrive with the snapshot's input already.
    input: ev.inputJson,
    output: "",
    status: "pending",
    durationMs: 0,
    error: "",
    server: ev.server,
    sub: ev.sub,
    subId: ev.subId || "",
  };
}

// Fold one hub event into the block list. type_name is authoritative
// (forward compatible with types this build doesn't know). Event
// semantics mirror cmd/lyon/tui.go's transcript cases.
//
// reduceMut folds IN PLACE (push / assign): the history seed folds
// thousands of events in one pass, and the old per-event array copy was
// O(n^2) — long transcripts made session switches crawl. The live path
// uses the copying reduce wrapper (React state immutability).
function reduce(blocks: Block[], ev: SessionEvent): Block[] {
  const next = blocks.slice();
  reduceMut(next, ev);
  return next;
}

function appendText(blocks: Block[], delta: string): void {
  const last = blocks[blocks.length - 1];
  if (last && last.kind === "text") {
    // COW, never mutate: the block object is shared with the previous
    // render's array, and React StrictMode double-invokes updaters — an
    // in-place append applied every delta twice.
    blocks[blocks.length - 1] = { ...last, text: last.text + delta };
    return;
  }
  blocks.push({ kind: "text", text: delta });
}

function reduceMut(blocks: Block[], ev: SessionEvent): void {
  switch (ev.typeName) {
    case "user_message":
      if (ev.text) blocks.push({ kind: "user", text: ev.text });
      return;
    case "text_delta":
      appendText(blocks, ev.text);
      return;
    case "thinking_delta": {
      // Stamps ride the row: t0/t1 from the event's server TS (replay
      // computes the same duration history showed live), lc from the
      // client receipt (the live tick's tail).
      const ts = ev.ts ? timestampDate(ev.ts).getTime() : Date.now();
      const last = blocks[blocks.length - 1];
      if (last && last.kind === "thinking") {
        blocks[blocks.length - 1] = {
          ...last,
          text: last.text + ev.thinking,
          t1: ts,
          lc: Date.now(),
        };
        return;
      }
      blocks.push({ kind: "thinking", text: ev.thinking, t0: ts, t1: ts, lc: Date.now() });
      return;
    }
    case "tool_use_start": {
      const card = newToolBlock(ev);
      if (ev.server) {
        // Server-side tool: the API already has the input and executes
        // it itself — one card carries query + result, no exec events.
        blocks.push({ ...card, status: "server", input: ev.inputJson });
        return;
      }
      // Harness tool: the API's tool_use carries no input yet — the
      // exec row below fills it in. Parallel calls each get their own
      // card, tagged by tool id.
      blocks.push(card);
      return;
    }
    case "tool_exec_start": {
      // Attach to the parent tool_use card via tool id; a subagent-inner
      // exec has no parent on this stream (events.go Sub) — open its own.
      const i = lastToolIndex(
        blocks,
        (b) => b.toolId !== "" && b.toolId === ev.toolId && b.execId === null,
      );
      const filled = (
        base: Extract<Block, { kind: "tool" }>,
      ): Extract<Block, { kind: "tool" }> => ({
        ...base,
        execId: ev.execId,
        input: ev.inputJson || base.input,
        status: ev.sub && base.status === "pending" ? "pending" : "running",
        // First start wins — a replayed duplicate must not reset the clock.
        startedAt: base.startedAt ?? Date.now(),
        sub: base.sub || ev.sub,
      });
      if (i >= 0) {
        blocks[i] = filled(blocks[i] as Extract<Block, { kind: "tool" }>);
      } else {
        blocks.push(filled({ ...newToolBlock(ev), status: "pending" }));
      }
      // A fan-out inner step also ticks its task's live counter.
      if (ev.sub && ev.subId) {
        for (let j = blocks.length - 1; j >= 0; j--) {
          const b = blocks[j];
          if (b.kind === "subtask" && b.subId === ev.subId) {
            blocks[j] = { ...b, steps: b.steps + 1 };
            break;
          }
        }
      }
      return;
    }
    case "tool_exec_output": {
      if (ev.execId === 0n) return;
      const i = lastToolIndex(blocks, (b) => b.execId === ev.execId);
      if (i < 0) return;
      const t = blocks[i] as Extract<Block, { kind: "tool" }>;
      blocks[i] = { ...t, output: t.output + ev.text + ev.detail };
      return;
    }
    case "tool_exec_end": {
      const end = (base: Extract<Block, { kind: "tool" }>) => ({
        ...base,
        status: ev.isError ? ("failed" as const) : ("done" as const),
        durationMs: Number(ev.durationMs),
        error: ev.isError ? ev.error || base.error : base.error,
      });
      if (ev.execId !== 0n) {
        const i = lastToolIndex(blocks, (b) => b.execId === ev.execId);
        if (i >= 0) {
          blocks[i] = end(blocks[i] as Extract<Block, { kind: "tool" }>);
          return;
        }
      }
      // exec_end without a seen exec_start (replay anchor skipped it):
      // open a finished card rather than drop the completion.
      blocks.push(end({ ...newToolBlock(ev), status: "pending" }));
      return;
    }
    case "server_tool_result":
      // results: the payload rides input; one card per server tool.
      blocks.push({
        ...newToolBlock(ev),
        status: ev.isError ? "failed" : "done",
        input: ev.inputJson,
      });
      return;
    case "queue_injected":
      // The parked line joined the run at a turn gap — render it as the
      // prompt it now is (TUI queueInjected), not a generic notice.
      if (ev.text) {
        blocks.push({ kind: "user", text: ev.text });
      } else {
        blocks.push({ kind: "notice", text: "queued prompt joined the run" });
      }
      return;
    case "background_end": {
      // A detached run_shell finished: status rides detail, the log tail
      // and path ride attrs so the outcome is readable without opening
      // the file (expandable in the notice's details).
      const task = ev.attrs["task"] || "?";
      const detail = [ev.attrs["log"], ev.attrs["tail"]]
        .filter(Boolean)
        .join("\n");
      const mark = ev.isError ? "\u2717" : "\u23f3";
      blocks.push({
        kind: "notice",
        text: `${mark} background task #${task} \u2014 ${ev.detail}`,
        ...(detail ? { detail } : {}),
      });
      return;
    }
    case "memory_injected":
      blocks.push({ kind: "notice", text: ev.detail, detail: ev.attrs["memory"] });
      return;
    case "compaction":
      if (ev.detail) blocks.push({ kind: "notice", text: `compaction \u2014 ${ev.detail}` });
      return;
    case "subagent_start": {
      // Fan-out task opening (#53): its own card — the header of the
      // main/sub exchange. A legacy single call has no SubID and draws
      // nothing here (its result rides the parent tool card).
      if (!ev.subId) return;
      blocks.push({
        kind: "subtask",
        subId: ev.subId,
        task: ev.detail,
        model: ev.toolName,
        status: "running",
        durationMs: 0,
        error: "",
        answer: "",
        calls: "",
        steps: 0,
      });
      return;
    }
    case "subagent_end": {
      if (ev.subId) {
        for (let i = blocks.length - 1; i >= 0; i--) {
          const b = blocks[i];
          if (b.kind === "subtask" && b.subId === ev.subId) {
            blocks[i] = {
              ...b,
              status: ev.isError ? ("failed" as const) : ("done" as const),
              durationMs: Number(ev.durationMs),
              error: ev.isError ? ev.error : "",
              answer: ev.isError ? "" : ev.text || "",
              calls: ev.isError ? "" : ev.detail || "",
            };
            return;
          }
        }
      }
      // No matching card (legacy single run_subagent): the old one-liner.
      const u = ev.usage;
      const usage = u
        ? `in=${u.input} out=${u.output} cache_read=${u.cacheRead}`
        : "";
      const tail = ev.isError
        ? ev.error
        : `${ev.detail} ${Number(ev.durationMs)}ms`.trim();
      blocks.push({
        kind: "notice",
        text: `\ud83e\udde0 subagent ${ev.toolName} ${usage} ${tail}`.trim(),
      });
      return;
    }
    case "files_changed": {
      // Server-side git delta (filecards.go), JSON rows in attrs.files —
      // malformed attrs skip the card, never the stream.
      let files: FileCard[] = [];
      try {
        const raw: unknown = JSON.parse(ev.attrs["files"] || "[]");
        if (Array.isArray(raw)) files = raw as FileCard[];
      } catch {
        return;
      }
      if (files.length) blocks.push({ kind: "files", files });
      return;
    }
    case "error":
      blocks.push({ kind: "error", text: ev.error || ev.detail });
      return;
    case "prompt_end": {
      // The badge is the WHOLE run's wall time (time.Since(submit) on the
      // server), pinned when the run reaches its final state — submit to
      // final resolution, tool loop included. One stamp per reply: a text
      // block already carrying a badge belongs to an earlier run.
      const ms = Number(ev.durationMs || 0);
      // The completion time rides the same badge row as the duration — a
      // UI element beside the reply, never inside the reply text.
      const at = ev.ts ? timestampDate(ev.ts).getTime() : Date.now();
      for (let i = blocks.length - 1; i >= 0; i--) {
        const b = blocks[i];
        if (b.kind === "text") {
          if (!b.turnMs && !b.endedAt) {
            blocks[i] = { ...b, turnMs: ms, endedAt: at };
          }
          break;
        }
      }
      if (ev.error) {
        blocks.push({ kind: "error", text: ev.error });
        return;
      }
      // The output cap ended the reply mid-sentence: nothing failed, but
      // "done" over a truncated answer is a lie — say why it stops there.
      if (ev.stopReason === "max_tokens") {
        blocks.push({
          kind: "notice",
          text:
            ev.detail ||
            "reply cut off at the output token cap — raise max_tokens to finish",
        });
      }
      return;
    }
    case "turn_end": {
      // The call's output tokens land on its reasoning row: walk back to
      // the trailing thinking block and stamp the API's actual count over
      // the stream estimate (reasoning dominates output; a block already
      // stamped belongs to an earlier call — stop there).
      const out = ev.usage?.output;
      if (!out) return;
      for (let i = blocks.length - 1; i >= 0; i--) {
        const b = blocks[i];
        if (b.kind !== "thinking") continue;
        if (b.tokens !== undefined) return;
        blocks[i] = { ...b, tokens: Number(out) };
        return;
      }
      return;
    }
    default:
      // message_start / subagent_start / unknown — nothing to draw (TUI
      // draws nothing for turn boundaries either).
      return;
  }
}

// One display line for a parked prompt (TUI oneLine): the queue list is a
// list of rows, not a transcript, so newlines collapse.
function oneLine(s: string): string {
  return s.replace(/\s+/g, " ").trim();
}

// Pretty-print tool input JSON when it is JSON; pass anything else through.
function prettyInput(raw: string): string {
  try {
    return JSON.stringify(JSON.parse(raw), null, 2);
  } catch {
    return raw;
  }
}

// ---- tool output coloring (lightweight heuristics, never a parser) ----
// Captured command output is one flat <pre>; content-aware color keeps
// diff hunks, file:line hits and error words scannable at a glance.

const ANSI_RE = /\u001b\[[0-9;]*m/g;

function toneWords(line: string): ReactNode[] {
  const re =
    /\b(error|errors|failed|failure|fatal|panic|denied|refused|timeout|not found|ok|passed|success)\b/gi;
  const out: ReactNode[] = [];
  let last = 0;
  for (let m = re.exec(line); m; m = re.exec(line)) {
    if (m.index > last) out.push(line.slice(last, m.index));
    const w = m[0].toLowerCase();
    const bad = /error|fail|fatal|panic|denied|refused|timeout|not found/.test(w);
    out.push(
      <span key={m.index} className={bad ? "to-err" : "to-ok"}>
        {m[0]}
      </span>,
    );
    last = m.index + m[0].length;
  }
  if (last < line.length) out.push(line.slice(last));
  return out;
}

function outputLine(line: string): ReactNode {
  if (/^diff --git /.test(line)) return <span className="to-diff-head">{line}</span>;
  if (/^@@/.test(line)) return <span className="to-diff-hunk">{line}</span>;
  if (/^(---|\+\+\+) /.test(line)) return <span className="to-diff-file">{line}</span>;
  if (/^\+/.test(line)) return <span className="to-diff-add">{line}</span>;
  if (/^-/.test(line)) return <span className="to-diff-del">{line}</span>;
  // path:line[:col]: rest — the locator recedes so the payload reads
  // first (grep/rg/compiler output shape).
  const m = /^(\S+:\d+(?::\d+)?:)(.*)$/.exec(line);
  if (m) {
    return (
      <span>
        <span className="to-loc">{m[1]}</span>
        {toneWords(m[2])}
      </span>
    );
  }
  return <span>{toneWords(line)}</span>;
}

// colorizeOutput wraps each line in its tone; huge dumps colorize the
// head and render the tail plain, so a 10MB log never pays for paint.
function colorizeOutput(text: string): ReactNode[] {
  const clean = text.replace(ANSI_RE, "");
  const lines = clean.split("\n");
  const cap = 2000;
  const out: ReactNode[] = [];
  for (let i = 0; i < lines.length; i++) {
    const sep = i < lines.length - 1 ? "\n" : "";
    out.push(
      <span key={i}>
        {i < cap ? outputLine(lines[i]) : lines[i]}
        {sep}
      </span>,
    );
  }
  return out;
}

// Clip to one header-width line (the folded tool row is a glance, not
// a document — the <details> body still holds the full input/output).
function clipLine(s: string, n = 90): string {
  const t = s.trim();
  return t.length > n ? t.slice(0, n - 1) + "…" : t;
}

// First non-empty line, clipped — the folded-row peek for thinking and
// tool output. Hand-rolled scan: these strings stream in and can be
// tens of KB; no full split per render.
// Rough live token count for a thinking row until turn_end's actual
// lands: ASCII ~4 chars/token, CJK ~1 (deepseek-family tokenizer shape).
// The "~" prefix in the render says estimate — the stamped usage replaces
// it the moment the call ends.
function estimateTokens(s: string): number {
  let cjk = 0;
  for (let i = 0; i < s.length; i++) {
    if (s.charCodeAt(i) >= 0x2e80) cjk++;
  }
  return Math.max(1, Math.round((s.length - cjk) / 4 + cjk));
}

// fmtTokens is the thinking row's compact count — "↓ 5.9k tokens" reads
// at a glance where "5,900" makes the eye stop (k/M units, one decimal
// below 10k so 5900 → 5.9k).
function fmtTokens(n: number): string {
  if (!Number.isFinite(n) || n < 0) return "0";
  if (n < 1000) return String(Math.round(n));
  if (n < 1_000_000) {
    const k = n / 1000;
    const s = k < 10 ? k.toFixed(1) : String(Math.round(k));
    return s.endsWith(".0") ? s.slice(0, -2) + "k" : s + "k";
  }
  const m = (n / 1_000_000).toFixed(1);
  return (m.endsWith(".0") ? m.slice(0, -2) : m) + "M";
}

function firstLine(s: string, n = 110): string {
  let i = 0;
  while (i < s.length) {
    let j = s.indexOf("\n", i);
    if (j < 0) j = s.length;
    const t = s.slice(i, j).trim();
    if (t) return clipLine(t, n);
    i = j + 1;
  }
  return "";
}

// One-line digest of a tool call's input for the folded header — the
// difference between a wall of "run_shell ✓" and a transcript you can
// audit while the seat runs. Per-tool keys for the common cases (field
// names mirror pkg/tools' json tags); unknown tools fall back to their
// first string field, then compact JSON. Recomputed per render: rows
// are cheap and the input string is the single source.
function toolSummary(name: string, raw: string): string {
  if (!raw) return "";
  let inp: Record<string, unknown>;
  try {
    const parsed: unknown = JSON.parse(raw);
    if (!parsed || typeof parsed !== "object") return clipLine(String(parsed));
    inp = parsed as Record<string, unknown>;
  } catch {
    return clipLine(oneLine(raw));
  }
  const str = (k: string) =>
    typeof inp[k] === "string" ? (inp[k] as string) : "";
  switch (name) {
    case "run_shell":
      return clipLine(oneLine(str("command")));
    case "read_file": {
      const off =
        typeof inp.offset === "number" && inp.offset > 0
          ? ` :${inp.offset}`
          : "";
      return clipLine(`${str("path")}${off}`);
    }
    case "write_file":
    case "edit_file":
      return clipLine(str("path"));
    case "search_files":
      return clipLine(`${str("pattern")}${str("path") ? ` in ${str("path")}` : ""}`);
    case "save_memory":
      return clipLine(`${str("title")}${str("scope") ? ` · ${str("scope")}` : ""}`);
    case "read_memory":
    case "forget_memory":
      return clipLine(str("query") || str("name"));
    case "run_subagent":
    case "run_subagents":
      return clipLine(oneLine(str("task")));
    case "web_search":
      return clipLine(oneLine(str("query")));
    case "todo_write": {
      const todos = Array.isArray(inp.todos)
        ? (inp.todos as Array<{ content?: string; status?: string }>)
        : [];
      const active = todos.find((t) => t?.status === "in_progress");
      return clipLine(
        `${todos.length} items${active?.content ? ` · ${oneLine(active.content)}` : ""}`,
      );
    }
    default:
      break;
  }
  for (const v of Object.values(inp)) {
    if (typeof v === "string" && v.trim()) return clipLine(oneLine(v));
  }
  return clipLine(oneLine(JSON.stringify(inp)));
}

// modeHasPath: the chooser's "仅该文件" option only exists when the
// parked input names a file — run_shell's command has nothing to vouch,
// and answering ALLOW_PATH there would fail closed server-side anyway.
function modeHasPath(p: WatchPendingResponse | undefined): boolean {
  if (!p?.modeBlock) return false;
  try {
    const j = JSON.parse(p.inputJson || "{}");
    return typeof j.path === "string" && j.path !== "";
  } catch {
    return false;
  }
}

export default function ChatView(props: {
  sessionId: string;
  onChanged: () => void;
  pending: WatchPendingResponse[];
  onDecide: (
    sessionId: string,
    execId: bigint,
    approve: boolean,
    remember: boolean,
    action?: ModeAction,
  ) => void;
  onTodos: (t: TodoItem[]) => void;
  /** Open the plan pane, or focus + shake it when it is already open (the
   *  footer plan chip's click). */
  onOpenPlan?: () => void;
  onSteps: (s: StepRow[]) => void;
  onCommand: (cmd: string) => void;
  /** Open a produced file in the preview pane (files_changed card rows) —
   *  the same routing FilePanel clicks use. Untracked rows land here. */
  onOpenFile?: (path: string) => void;
  /** Open the 变更模式 (unified diff, #42) — reached ONLY from the
   *  files_changed card's change rows; plain preview never routes here. */
  onOpenDiff?: (path: string) => void;
  /** Live prompt_end only (not history replay): the server regenerates
   *  session titles asynchronously on its interval beats, so App re-reads
   *  the list a beat later. */
  onPromptEnd?: () => void;
  /** Live context load — App draws the footer's bottom-right ctx gauge
   *  (TUI corner-gauge parity). Server-computed (Session.ctx_used on
   *  open, turn_end's ctx_used live); 0 on reset. */
  onCtx?: (tok: number) => void;
  /** Session-cumulative USD, undefined = no price on file (hide $).
   *  Same two sources as onCtx; reset to undefined on session switch. */
  onCost?: (usd: number | undefined) => void;
  reloadKey: number; // bump re-anchors the stream (rollback rewrote the ring)
  // Saved-session store feed for the /resume /rollback pickers, plus the
  // two restore paths that need App state (restore swaps the selection,
  // rollback rewrites the ring and re-anchors the stream).
  snapshots: SnapshotRef[];
  onRestore: (path: string) => void;
  onRollbackTo: (checkpointId: number) => void;
  // Mode control lives in App's bottom-left dock (shift+tab / click) —
  // ChatView no longer owns a badge or set-mode buttons.
  /** Lifts the revision-guarded mindmap snapshot to App — the map renders
   *  in the mindmap pane, a peer of chat (#51). Fires null on mount so a
   *  fresh session key clears the previous seat's map before GetMindmap. */
  /** Snapshot report carries the session it belongs to — App guards
   *  against acting on the outgoing session's map during a switch. */
  onMindmap?: (s: MindmapSnap | null, sid: string) => void;
  /** M2 edit-channel commands from the pane: a seq'd request carrying
   *  the RPC's fresh nodes (rev set) or a bare refetch (rev undefined —
   *  the optimistic apply lost a §5 conflict). seq bumps per command.
   *  sid tags the session the command belongs to (Image #69: a stale
   *  cmd from the previous seat must never paint this pane). */
  mmCmd?: { seq: number; sid: string; rev?: number; nodesJson?: string } | null;
  /** Jump request from the mindmap pane: seq changes on every click so
   *  re-jumping the same turn re-fires. */
  jumpTo?: { turn: number; seq: number } | null;
  /** §7.3 selection: node ids currently圈选 in the pane (same session).
   *  Drives the composer's 对 N 个节点提问 hint; null/empty = none. */
  mmSelIds?: string[] | null;
  /** Chip clicks in past selection messages re-highlight that selection. */
  onMmSelect?: (ids: string[] | null) => void;
  /** A semantic selection ask composed by the pane's toolbar — seq bumps
   *  per ask so the same text can re-fire; sid tags the seat that asked,
   *  which is what keeps an incoming ChatView from re-dispatching the
   *  outgoing seat's ask (Image #74). ChatView dispatches it through the
   *  normal prompt path (queue-aware). */
  selAsk?: { seq: number; sid: string; text: string } | null;
  /** The session's execution mode (App's dock). Gates the AUTO-ask only:
   *  map edits dispatch a round in mindmap mode, land as a notice
   *  otherwise (Image #61). Manual selAsk is untouched — it is the user
   *  asking, not the map talking back. */
  mode?: string;
  /** A batch of agent-origin map_ops just landed — App counts unconfirmed
   *  batches for the toolbar's A5 warning (先处理未确认改动). */
  onAgentBatch?: () => void;
}) {
  const [blocks, setBlocks] = useState<Block[]>([]);
  const [busy, setBusy] = useState(false);
  const [input, setInput] = useState("");
  const [err, setErr] = useState("");
  const [apprSel, setApprSel] = useState(0);
  const feedRef = useRef<HTMLDivElement>(null);
  // Composer grows with its content: one line at rest, six at most, then
  // it scrolls. Done here rather than in CSS because it needs the
  // content's scrollHeight; the stylesheet only sets a floor.
  const composerRef = useRef<HTMLTextAreaElement>(null);
  useEffect(() => {
    const el = composerRef.current;
    if (!el) return;
    el.style.height = "auto";
    const line = parseFloat(getComputedStyle(el).lineHeight) || 21;
    const cap = Math.round(line * 6 + 10); // 6 lines + the 5px pad top/bottom
    el.style.height = `${Math.min(el.scrollHeight, cap)}px`;
    el.style.overflowY = el.scrollHeight > cap ? "auto" : "hidden";
  }, [input]);
  // Phase D: plan corner / status line / slash panel local state.
  const [todos, setTodos] = useState<TodoItem[]>([]);
  // The session mindmap (§7.1): ONE revision-guarded snapshot, not a
  // Block — every distill update would otherwise append another card.
  // Null until GetMindmap or the first event lands; reset on switch.
  // ChatView keeps the subscription (it owns the event stream) but the
  // card renders in App's mindmap pane, a peer of chat (#51) — the
  // snapshot is reported up on every change (null first, on mount).
  const [mindmap, setMindmap] = useState<MindmapSnap | null>(null);
  const mmRevRef = useRef(0);
  useEffect(() => {
    props.onMindmap?.(mindmap, props.sessionId);
  }, [mindmap]); // eslint-disable-line react-hooks/exhaustive-deps
  // The revision-guarded snapshot apply (§7.1): one state, never a
  // Block. Stale revisions lose; equal rev is an idempotent re-delivery
  // (same content — skip, no re-render churn). mindmap_update carries
  // title/subtitle, mindmap_edit doesn't — empty keeps what's on hand.
  // Component scope so every seam shares it: live stream, history
  // replay, GetMindmap, and the pane's UpdateMindmap response (mmCmd).
  const applyMindmap = (
    rev: number,
    title: string | undefined,
    subtitle: string | undefined,
    nodesJson: string | undefined,
  ) => {
    // Latest accepted revision as a ref: the live stream closure must
    // stamp agent map_ops with the CURRENT base (§5 optimistic lock)
    // without re-subscribing on every snapshot change. Max-merge keeps
    // it monotonic even under StrictMode's double-invoked updaters.
    mmRevRef.current = Math.max(mmRevRef.current, rev);
    setMindmap((cur) => {
      if (cur && rev <= cur.revision) return cur;
      return {
        revision: rev,
        title: title || cur?.title || "",
        subtitle: subtitle || cur?.subtitle || "",
        nodes: parseNodes(nodesJson),
      };
    });
  };
  // M2 merge window (§2/§8): consecutive human edits coalesce for 800ms
  // into ONE chat line (A1 — 改 A、改 B、删 C in a single message). Only
  // LIVE stream events feed the buffer: replaying an old mindmap_edit
  // from history must never re-fire a turn. LYON_MINDMAP_ASK=off lands
  // the merged line as a feed notice instead (只入 chat 不发轮).
  const mmEditBuf = useRef<string[]>([]);
  // Live mode for mount-time closures (flushMmEdit — see the gate below).
  // Same render-assignment pattern as dispatchRef at the bottom.
  const modeRef = useRef(props.mode);
  modeRef.current = props.mode;
  const mmEditTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const resetMmEdit = () => {
    if (mmEditTimer.current) clearTimeout(mmEditTimer.current);
    mmEditTimer.current = null;
    mmEditBuf.current = [];
  };
  // dispatchPrompt is declared below the stream effect; the merge fires
  // asynchronously (800ms later), so it goes through a ref that always
  // holds the latest closure (busy/queued state included).
  const dispatchRef = useRef<(p: string) => Promise<void>>(async () => {});
  const summarizeMmEdit = (opsJson: string | undefined, nodes: MapNode[]): string[] => {
    type Op = {
      op: string;
      id: string;
      label?: string;
      parent_id?: string;
      status?: string;
      state?: string;
    };
    let ops: Op[] = [];
    try {
      ops = JSON.parse(opsJson || "[]") as Op[];
    } catch {
      return [];
    }
    const by = new Map(nodes.map((n) => [n.id, n] as const));
    const name = (id: string) => by.get(id)?.label ?? id;
    return ops.map((o) => {
      switch (o.op) {
        case "rewrite":
          return `重命名节点为「${name(o.id)}」`;
        case "delete":
          return `删除「${name(o.id)}」`;
        case "park":
          return o.status === "active" ? `恢复「${name(o.id)}」` : `停靠「${name(o.id)}」`;
        case "confirm":
          return o.state === "proposed" ? `取消确认「${name(o.id)}」` : `确认「${name(o.id)}」`;
        case "set_parent":
          return `把「${name(o.id)}」移到「${name(o.parent_id || "")}」下`;
        case "merge":
          return `合并「${name(o.id)}」的选定节点`;
        case "add":
          return `新增「${o.label ?? name(o.id)}」`;
        default:
          return `${o.op}「${name(o.id)}」`;
      }
    });
  };
  const flushMmEdit = () => {
    const edits = mmEditBuf.current;
    mmEditBuf.current = [];
    mmEditTimer.current = null;
    if (edits.length === 0) return;
    const summary = edits.join("；");
    void mindmapAskOn().then((ask) => {
      // Image #61: two gates. The env flag is the server-wide kill
      // switch; the session mode is the per-session arm — outside
      // mindmap mode the map observes, it does not prompt the agent.
      // modeRef, NOT props.mode: flushMmEdit is invoked from the watch
      // effect's mount-time closure (deps don't include props), so the
      // direct read froze at the seat's opening mode and the gate never
      // opened on a default-opened seat.
      const armed = ask && modeRef.current === "mindmap";
      if (!armed) {
        setBlocks((b) => [
          ...b,
          {
            kind: "notice",
            text: `思维导图编辑：${summary}`,
            detail:
              modeRef.current === "mindmap"
                ? "LYON_MINDMAP_ASK=off — 只入 chat，未发轮"
                : "mindmap 模式外 — 只入 chat，未发轮（切到 mindmap 模式才追问）",
          },
        ]);
        return;
      }
      // §4.4 prompt discipline rides the message itself: one
      // restatement, ONE question, zero tool use — the turn exists so
      // the agent can voice its understanding back for correction.
      void dispatchRef.current(
        `我在思维导图上做了改动：${summary}。\n` +
          `【系统 · 图上自动提问纪律】先用一句话复述你对这次改动的理解，只问一个最关键的问题；` +
          `不要执行任何工具，不写文件、不改代码、不开始任何工作。`,
      );
    });
  };
  const applyMindmapEv = (ev: SessionEvent, live: boolean) => {
    const upd = ev.typeName === "mindmap_update";
    if (!upd && ev.typeName !== "mindmap_edit") return;
    applyMindmap(
      Number(ev.attrs["revision"] || 0),
      upd ? ev.attrs["title"] : undefined,
      upd ? ev.attrs["subtitle"] : undefined,
      ev.attrs["nodes"],
    );
    if (!live || upd) return;
    // §2: only HUMAN edits ride the map→chat channel. An agent-origin
    // batch landing is the answer to a selection ask — the agent must
    // never auto-prompt itself about its own write — so it counts
    // toward A5's pending batch tally instead (one event = one batch).
    const origin = ev.attrs["origin"] || "";
    if (origin === "agent") {
      props.onAgentBatch?.();
      return;
    }
    if (origin !== "user") return; // ""/unknown: observe the map only
    const parts = summarizeMmEdit(ev.attrs["ops"], parseNodes(ev.attrs["nodes"]));
    if (parts.length === 0) return;
    mmEditBuf.current.push(...parts);
    if (mmEditTimer.current) clearTimeout(mmEditTimer.current);
    mmEditTimer.current = setTimeout(flushMmEdit, 800);
  };
  // §7.3 selection-ask reply: the toolbar dispatches a marked prompt; the
  // live loop arms this ref on OUR user_message (a queued ask must not
  // collect the PREVIOUS turn's deltas), buffers text_delta, and parses
  // the ```map_ops fence once at prompt_end. History replay never arms it.
  const selAskRef = useRef<{
    text: string;
    collecting: boolean;
    buf: string;
    done: boolean;
  } | null>(null);
  const parseSelReply = (buf: string) => {
    const notice = (text: string, detail: string) =>
      setBlocks((b) => [...b, { kind: "notice", text, detail }]);
    const fence = /```map_ops\s*([\s\S]*?)```/.exec(buf);
    if (!fence) {
      // A4: prose-only reply — the map stays untouched, never guessed.
      // Only verbs that expected a change (【期望落图】 in the dispatched
      // text) earn the notice; 解释/矛盾吗 answering in prose is correct.
      if ((selAskRef.current?.text ?? "").includes("【期望落图】")) {
        notice(
          "选区答复未带 map_ops — 图未改动",
          "需要结构化 map_ops 才能落图（散文不落图）",
        );
      }
      return;
    }
    let ops: unknown;
    try {
      ops = JSON.parse(fence[1].trim());
    } catch {
      notice("map_ops 不是合法 JSON — 图未改动", "答复里的代码块解析失败");
      return;
    }
    if (
      !Array.isArray(ops) ||
      ops.length === 0 ||
      ops.some((o) => !o || typeof (o as { op?: unknown }).op !== "string")
    ) {
      notice("map_ops 格式不对 — 图未改动", "需要 JSON 数组，每项带 op 字段");
      return;
    }
    // origin="agent": §4.3 — may never touch pinned/confirmed (server
    // rejects the batch), new nodes land proposed (dashed, A5's count).
    void mindmapClient
      .updateMindmap({
        sessionId: props.sessionId,
        revision: mmRevRef.current,
        origin: "agent",
        opsJson: JSON.stringify(ops),
      })
      .then((res) => {
        // A5 count rides the origin="agent" event in applyMindmapEv
        // (exactly one per applied batch, no-ops and rejects don't count).
        applyMindmap(Number(res.revision), undefined, undefined, res.nodesJson);
      })
      .catch((e) => setErr(errText(e)));
  };
  // Toolbar → wire: arm the collector, then hand the marked prompt to the
  // single submission path. seq gates the effect so re-renders and
  // StrictMode double-mounts don't double-send.
  useEffect(() => {
    const a = props.selAsk;
    // Image #74: this effect's FIRST run on a seat switch still sees the
    // outgoing seat's ask (child effects precede App's clear) — the sid
    // tag rejects it instead of re-dispatching into the new session.
    if (!a || a.sid !== props.sessionId) return;
    selAskRef.current = { text: a.text, collecting: false, buf: "", done: false };
    void dispatchRef.current(a.text);
    // eslint-disable-next-line react-hooks/exhaustive-deps -- seq is the trigger
  }, [props.selAsk?.seq]);
  const [thinkOpen, setThinkOpen] = useState(false);
  const [cmdNote, setCmdNote] = useState(""); // local command receipt, 3s
  const [slashSel, setSlashSel] = useState(0);
  // Bare "/" browses one category tab at a time (TUI sugCat parity);
  // a typed prefix still searches across every category.
  const [slashCat, setSlashCat] = useState(0);
  const [hist, setHist] = useState<string[]>([]);
  const [histIdx, setHistIdx] = useState(-1); // -1 = editing a fresh line
  const noteTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  // Output drawer: multi-line command results land here (TUI cmdOutView).
  const [cmdOut, setCmdOut] = useState<string[] | null>(null);
  // @ file completion (TUI atfiles.go): the trailing @token's entries.
  const [atEntries, setAtEntries] = useState<Array<{ cmd: string; desc: string; dir: boolean }>>([]);
  // queued mirrors this pane's own Enqueue'd prompts (TUI m.queued): the
  // server exposes only QueueLen, so the text is client-side. Entries leave
  // on the matching queue_injected (FIFO, first equal string — same rule as
  // queueInjected in tui.go); whatever is left at prompt_end was dropped by
  // the session (#2) and is re-submitted here, exactly like kickQueued.
  const [queued, setQueuedState] = useState<string[]>([]);
  const queuedRef = useRef<string[]>([]);
  const setQueued = useCallback(
    (upd: (q: string[]) => string[]) => {
      queuedRef.current = upd(queuedRef.current);
      setQueuedState(queuedRef.current);
    },
    [],
  );
  const [atSel, setAtSel] = useState(0);
  const [atOff, setAtOff] = useState(false); // Esc dismissed until the input changes
  // /resume and /rollback open a numbered pick list in the pane; the
  // textarea's keys drive it until Enter or Esc.
  const [pick, setPick] = useState<PickState>(null);
  const [pickSel, setPickSel] = useState(0);

  // FIFO by ask time; the stream may replay snapshots in any order.
  const pendings = useMemo(
    () =>
      [...props.pending].sort((a, b) =>
        a.requestedAtMs === b.requestedAtMs
          ? 0
          : a.requestedAtMs < b.requestedAtMs
            ? -1
            : 1,
      ),
    [props.pending],
  );
  const sel = pendings.length === 0 ? 0 : Math.min(apprSel, pendings.length - 1);

  // Approval keys mirror TUI's card: y/n/a answer, arrows/tab walk the
  // queue. Never while the user is typing into an input — a prompt that
  // contains "y" must not approve anything. A mode_block card swaps the
  // vocabulary (f/d/b/n — the chooser below): a policy answer, not consent.
  useEffect(() => {
    if (pendings.length === 0) return;
    const answer = (approve: boolean, remember: boolean) => {
      const p = pendings[sel];
      if (p) props.onDecide(p.sessionId, p.execId, approve, remember);
    };
    const answerMode = (action: ModeAction) => {
      const p = pendings[sel];
      if (p)
        props.onDecide(
          p.sessionId,
          p.execId,
          action === ModeAction.MODE_DEFAULT, // approve flag: old-client compat only
          false,
          action,
        );
    };
    const isMode = !!pendings[sel]?.modeBlock;
    const onKey = (e: KeyboardEvent) => {
      const ae = document.activeElement;
      // isContentEditable: CodeMirror edits are typing too — a "y" in the
      // editor must not approve a pending prompt
      if (ae && (ae.tagName === "TEXTAREA" || ae.tagName === "INPUT" || (ae as HTMLElement).isContentEditable)) return;
      switch (e.key) {
        case "ArrowDown":
          e.preventDefault();
          setApprSel((s) => (s + 1) % pendings.length);
          break;
        case "Tab":
          // shift+tab belongs to App's global mode cycle, not the card.
          if (e.shiftKey) break;
          e.preventDefault();
          setApprSel((s) => (s + 1) % pendings.length);
          break;
        case "ArrowUp":
          e.preventDefault();
          setApprSel((s) => (s - 1 + pendings.length) % pendings.length);
          break;
        case "y":
          // Old-client compat: y on a mode card = "let it run" = default.
          if (isMode) answerMode(ModeAction.MODE_DEFAULT);
          else answer(true, false);
          break;
        case "n":
          if (isMode) answerMode(ModeAction.DENY);
          else answer(false, false);
          break;
        case "a":
          if (!isMode) answer(true, true); // no "always" on a policy card
          break;
        case "f":
          if (isMode && modeHasPath(pendings[sel])) {
            e.preventDefault();
            answerMode(ModeAction.ALLOW_PATH);
          }
          break;
        case "d":
          if (isMode) {
            e.preventDefault();
            answerMode(ModeAction.MODE_DEFAULT);
          }
          break;
        case "b":
          if (isMode) {
            e.preventDefault();
            answerMode(ModeAction.MODE_BYPASS);
          }
          break;
        case "Enter":
          e.preventDefault();
          if (isMode) answerMode(ModeAction.MODE_DEFAULT);
          else answer(true, false);
          break;
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [pendings, sel, props]);

  // Output drawer + thinking fold are app-wide gestures (TUI esc / ctrl+o):
  // they must fire while the prompt textarea is focused, so there is no
  // TEXTAREA guard here — only defaultPrevented, i.e. a popup that already
  // claimed the key (slash panel Esc, @ popup Esc) wins, and a second Esc
  // then closes the drawer. ctrl+o is preventDefault()ed so Chrome's
  // Ctrl+O file dialog never opens over the workspace.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.defaultPrevented) return;
      if (e.key === "Escape") {
        setCmdOut((v) => (v === null ? v : null));
        return;
      }
      if (e.ctrlKey && !e.altKey && !e.shiftKey && e.key.toLowerCase() === "o") {
        e.preventDefault();
        setThinkOpen((v) => !v);
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, []);

  // One WatchEvents stream per selected session — but the transcript
  // renders from the unary GetHistory batch first: history no longer
  // rides the stream, so a stalled streaming socket costs live deltas
  // only, never the initial render (the "点旧会话不渲染，提问才渲染"
  // bug). The tail opens at last_seq, exactly where the batch ended.
  // Busy/model seed from the session record so opening a mid-run session
  // doesn't render it idle. reloadKey re-anchors the same seat after a
  // rollback rewrote the ring — the batch below re-derives todos/steps
  // from the surviving events; streamKey is the local retry bump (dropped
  // stream → the err row's retry button). Inside, a first-byte watchdog
  // reopens a stalled connection (see below).
  const [streamKey, setStreamKey] = useState(0);
  useEffect(() => {
    let ac: AbortController | null = null;
    let live = true;
    setErr("");
    setTodos([]);
    setMindmap(null);
    resetMmEdit(); // a pending auto-ask belongs to the outgoing seat
    props.onCtx?.(0);
    props.onCost?.(undefined);
    void (async () => {
      // All three reads are unary (content-length) responses — they
      // arrive even while a streaming socket would sit unread. Parallel:
      // independent RPCs, and each failure degrades on its own (no
      // history → the stream's full replay below; no record → idle; no
      // map → the card waits for its first event).
      const [hist, sres, mm] = await Promise.all([
        sessionClient.getHistory({ sessionId: props.sessionId }).catch(() => null),
        sessionClient.getSession({ sessionId: props.sessionId }).catch(() => null),
        mindmapClient.getMindmap({ sessionId: props.sessionId }).catch(() => null),
      ]);
      if (!live) return;
      // The map is NOT in the session snapshot — /resume rebuilds it
      // here; the history fold below reconciles either way through the
      // same revision guard.
      if (mm) applyMindmap(Number(mm.revision), mm.title, mm.subtitle, mm.nodesJson);
      // anchor = where the live tail starts. -1 (no usable batch: reaped
      // seat, empty ring) keeps the old full-replay path — replaying at
      // subscribe time also covers events stamped between an empty batch
      // and the stream opening.
      let anchor = -1n;
      if (hist) {
        let seeded: Block[] = [];
        for (const ev of hist.events) {
          // Same fold as the stream below, minus busy: the session
          // record is authoritative there (history holds prompt_end
          // rows from earlier turns of a mid-run seat).
          if (ev.typeName === "tool_exec_start" && !ev.sub && ev.toolName === "todo_write") {
            const t = parseTodos(ev.inputJson);
            if (t) {
              setTodos(t);
              props.onTodos(t);
            }
          }
          if (ev.typeName === "turn_end") {
            // Server stamp wins (contextUsed includes output + compaction
            // special-case — the raw-usage sum below drifted from TUI).
            if (ev.ctxUsed) props.onCtx?.(Number(ev.ctxUsed));
            if (ev.costUsd !== undefined) props.onCost?.(Number(ev.costUsd));
          }
          applyMindmapEv(ev, false); // replay: snapshot only, no auto-ask re-fire
          reduceMut(seeded, ev); // in place: the O(n) history fold
        }
        setBlocks(seeded);
        if (hist.events.length > 0) anchor = hist.lastSeq;
      } else {
        // No batch at all: the stream's full replay rebuilds — clear
        // first so kept blocks can never double against the replay.
        setBlocks([]);
      }
      if (sres?.session) {
        setBusy(sres.session.busy);
        // Open-time gauge: ctx_used/cost_usd ride the Session snapshot so
        // an untouched (or restored) session shows its load before any
        // turn_end streams (#42 — "打开会话不请求就不展示").
        props.onCtx?.(Number(sres.session.ctxUsed ?? 0));
        props.onCost?.(
          sres.session.costUsd !== undefined
            ? Number(sres.session.costUsd)
            : undefined,
        );
      }
      // First-byte watchdog (the stream sitting on a connection that
      // never delivers): during the observed hang every other request
      // (curl probes, bare fetch, SubmitPrompt, GetHistory above)
      // returned instantly — only the stream POST sat un-read. Zero
      // events within the window ⇒ abort and reopen; fresh connections
      // are empirically always healthy. Retries fire only while the
      // stream has said nothing, so live seats are untouched, and the
      // final attempt is left open (not aborted) so an idle seat keeps
      // waiting for its first prompt in peace. History already rendered
      // above, so these attempts only ever cost the live tail.
      const WATCHDOG_MS = 1500;
      const MAX_ATTEMPTS = 4;
      const sleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));
      // kickQueued sends the next parked line — one per call, so the chain
      // paces itself at each prompt_end. Also called on rebuild: a stream
      // that died around prompt_end must not strand the queue — an idle
      // seat with parked lines picks the head back up.
      const kickQueued = (): boolean => {
        const [head, ...rest] = queuedRef.current;
        if (head === undefined) return false;
        setQueued(() => rest);
        sessionClient
          .submitPrompt({ sessionId: props.sessionId, prompt: head })
          .then(() => props.onChanged())
          .catch((e) => {
            if (!live) return;
            // The re-send lost the race (seat reaped, another pane
            // grabbed the run): park the head again instead of
            // dropping it — visible in the queue, it rides the next
            // prompt_end, while the error line explains the miss.
            setQueued((q) => [head, ...q]);
            setErr(errText(e));
            setBusy(false);
          });
        return true;
      };
      // A stream that died around prompt_end left parked lines stranded:
      // the seat is idle again, so the head runs now.
      if (sres?.session && !sres.session.busy && kickQueued()) {
        setBusy(true);
      }
      for (let attempt = 1; live && attempt <= MAX_ATTEMPTS; attempt++) {
        setErr("");
        const controller = new AbortController();
        ac = controller;
        let timedOut = false;
        let timer: ReturnType<typeof setTimeout> | null =
          attempt < MAX_ATTEMPTS
            ? setTimeout(() => {
                timedOut = true;
                controller.abort();
              }, WATCHDOG_MS)
            : null;
        try {
          const stream = sessionClient.watchEvents(
            { sessionId: props.sessionId, fromSeq: anchor },
            {
              signal: controller.signal,
              // Chrome won't complete the first read on a held-open
              // connect+json chunked stream until ~1s of continued flow;
              // the server beats every 100ms when this header is set.
              headers: { "x-lyon-keepalive": "1" },
            },
          );
          for await (const msg of stream) {
            if (!live) return;
            if (timer) {
              clearTimeout(timer);
              timer = null;
            }
            const ev = msg.event;
            if (!ev) continue;
            // §7.3 selection-ask collection: arm on OUR prompt echo (a
            // queued ask must not collect the previous turn's deltas),
            // buffer the reply's text deltas, parse once at prompt_end.
            if (
              ev.typeName === "user_message" &&
              selAskRef.current &&
              !selAskRef.current.done &&
              ev.text === selAskRef.current.text
            ) {
              selAskRef.current.collecting = true;
            }
            if (ev.typeName === "text_delta" && selAskRef.current?.collecting) {
              selAskRef.current.buf += ev.text ?? "";
            }
            if (ev.typeName === "prompt_end") {
              if (selAskRef.current?.collecting && !selAskRef.current.done) {
                selAskRef.current.done = true;
                parseSelReply(selAskRef.current.buf);
              }
              // Undelivered parked lines were dropped by the session in the
              // same critical section that cleared busy (#2). Mirror the
              // TUI's kickQueued: re-send the head as the next prompt (one
              // per prompt_end, so the chain paces itself); the rest stay
              // parked. No head → the seat is genuinely idle again.
              if (!kickQueued()) {
                setBusy(false);
              }
              props.onPromptEnd?.();
            }
            // A parked line joined the run: take its display row down.
            // FIFO on both sides means the first equal string is the one
            // that arrived (tui.go queueInjected).
            if (ev.typeName === "queue_injected" && ev.text) {
              setQueued((q) => {
                const i = q.indexOf(ev.text);
                return i < 0 ? q : [...q.slice(0, i), ...q.slice(i + 1)];
              });
            }
            // todo_write: exec_start (not sub) carries the COMPLETE list —
            // replace the whole plan (cmd/lyon/todos.go semantics). On the
            // replay path the same exec_start re-delivers it, so the plan
            // survives rollbacks and reconnects without extra plumbing.
            if (ev.typeName === "tool_exec_start" && !ev.sub && ev.toolName === "todo_write") {
              const t = parseTodos(ev.inputJson);
              if (t) {
                setTodos(t);
                props.onTodos(t);
              }
            }
            // turn_end's server-stamped gauge: context load + cumulative
            // cost (cmd/lyon/usage.go twins — one definition, both surfaces).
            if (ev.typeName === "turn_end") {
              if (ev.ctxUsed) props.onCtx?.(Number(ev.ctxUsed));
              if (ev.costUsd !== undefined) props.onCost?.(Number(ev.costUsd));
            }
            applyMindmapEv(ev, true); // live: feeds the 800ms merge → one chat line
            setBlocks((b) => reduce(b, ev));
            // Advance the retry anchor with every processed event: a
            // reopen replays seq > anchor, so blocks fold once per event
            // across attempts — no per-attempt reset, and a stray event
            // flushed mid-abort can't double.
            if (ev.seq > anchor) anchor = ev.seq;
          }
          if (timer) clearTimeout(timer);
          return; // server closed the stream — nothing to retry here
        } catch (e) {
          if (timer) clearTimeout(timer);
          if (!live) return;
          if (timedOut) {
            await sleep(300); // let the aborted socket actually die
            continue; // reopen on a fresh connection
          }
          const ce = ConnectError.from(e);
          if (ce.code !== Code.Canceled) {
            setErr(errText(e));
            // NotFound = the seat left the store (reaped while the list
            // was stale); nudge App to refresh so the ghost row goes and
            // the selection clears.
            if (ce.code === Code.NotFound) props.onChanged();
          }
          return;
        }
      }
    })();
    return () => {
      live = false;
      ac?.abort();
    };
  }, [props.sessionId, props.reloadKey, streamKey]); // eslint-disable-line react-hooks/exhaustive-deps

  // Steps are a pure view of the tool blocks (single source of truth):
  // fold once here, publish the rows for the right column's audit list.
  useEffect(() => {
    const rows: StepRow[] = [];
    for (const b of blocks) {
      if (b.kind !== "tool") continue;
      rows.push({
        execId: b.execId ?? 0n,
        toolName: b.name,
        input: b.input ? prettyInput(b.input) : "",
        output: b.output,
        status: b.status,
        durationMs: b.durationMs,
        error: b.error,
        sub: b.sub,
      });
    }
    props.onSteps(rows);
  }, [blocks]); // eslint-disable-line react-hooks/exhaustive-deps

  // The slash panel only matches a single bare token ("/sta"), so a
  // command with args falls through to send()'s dispatcher. Bare "/"
  // browses the current category tab (TUI sugCat parity); a prefix
  // searches across every category.
  const SLASH_CATS: SlashKind[] = ["session", "model", "panel", "misc"];
  const slashOpen = input.startsWith("/") && !/\s/.test(input);
  const slashBare = input === "/";
  const slashMatches = useMemo(() => {
    if (!slashOpen) return [];
    const q = input.trim().toLowerCase();
    if (q === "/") {
      return SLASH.filter((c) => c.kind === SLASH_CATS[slashCat]);
    }
    return SLASH.filter((c) => c.cmd.startsWith(q));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [slashOpen, input, slashCat]);
  const ssel = Math.min(slashSel, Math.max(0, slashMatches.length - 1));
  useEffect(() => setSlashSel(0), [input, slashCat]);

  // @ file completion (TUI atfiles.go atToken): the trailing whitespace-
  // separated field iff it starts with "@" and holds no space/quote after
  // the sigil. Directory part stays inside the workspace — the popup
  // only claims bare tokens.
  const atTok = useMemo(() => {
    if (input === "") return null;
    const start = input.lastIndexOf(" ") + 1;
    const tok = input.slice(start);
    if (!tok.startsWith("@") || /[\s"]/.test(tok.slice(1))) return null;
    return { rel: tok.slice(1), start };
  }, [input]);
  const atOpen = atTok !== null && !atOff;
  useEffect(() => {
    setAtOff(false);
    setAtSel(0);
  }, [input]);
  useEffect(() => {
    if (!atOpen || !atTok) {
      setAtEntries([]);
      return;
    }
    // Split the token into the directory to list and the name prefix to
    // filter ("pkg/rea" lists "pkg" filtering "rea").
    const slash = atTok.rel.lastIndexOf("/");
    const dirPart = slash >= 0 ? atTok.rel.slice(0, slash) : "";
    const prefix = (slash >= 0 ? atTok.rel.slice(slash + 1) : atTok.rel).toLowerCase();
    let live = true;
    void (async () => {
      try {
        const res = await fileClient.listDirectory({ path: dirPart });
        if (!live) return;
        const dirs: Array<{ cmd: string; desc: string; dir: boolean }> = [];
        const files: typeof dirs = [];
        if (dirPart !== "") {
          dirs.push({ cmd: `@${dirPart}/../`, desc: "up", dir: true });
        }
        for (const e of res.entries) {
          if (e.name.startsWith(".") && !prefix.startsWith(".")) continue;
          if (prefix !== "" && !e.name.toLowerCase().startsWith(prefix)) continue;
          if (e.isDir) dirs.push({ cmd: `@${dirPart}${dirPart ? "/" : ""}${e.name}/`, desc: "dir", dir: true });
          else files.push({ cmd: `@${dirPart}${dirPart ? "/" : ""}${e.name}`, desc: `${Number(e.sizeBytes)}B`, dir: false });
        }
        // dirs first, alphabetical within each group; cap so a huge
        // directory can't drown the input line
        dirs.sort((a, b) => a.cmd.localeCompare(b.cmd));
        files.sort((a, b) => a.cmd.localeCompare(b.cmd));
        setAtEntries([...dirs, ...files].slice(0, 40));
      } catch {
        if (live) setAtEntries([]);
      }
    })();
    return () => {
      live = false;
    };
  }, [atOpen, atTok?.rel]); // eslint-disable-line react-hooks/exhaustive-deps
  const asel = Math.min(atSel, Math.max(0, atEntries.length - 1));
  const atInsert = (item: { cmd: string; dir: boolean }) => {
    if (!atTok) return;
    // Replace the @token in place; dirs keep the popup open for further
    // typing, files get a trailing space (the TUI insertion form).
    const head = input.slice(0, atTok.start);
    setInput(item.dir ? `${head}${item.cmd}` : `${head}${item.cmd} `);
  };

  // Follow-the-tail with an opt-out: while the reader is parked at the
  // bottom, new blocks snap the feed down; scrolling up pauses tracking so
  // a run can be reviewed mid-flight, and coming back to the bottom
  // re-arms it. (Unconditional scrollTo here was the "running 中不能往上
  // 滚" bug — every delta yanked the view back down.)
  const stickRef = useRef(true);
  const onFeedScroll = () => {
    const el = feedRef.current;
    if (!el) return;
    stickRef.current = el.scrollHeight - el.scrollTop - el.clientHeight < 48;
  };
  useEffect(() => {
    const el = feedRef.current;
    if (el && stickRef.current && blocks.length > 0) el.scrollTo({ top: el.scrollHeight });
  }, [blocks]);

  // note prints a local command receipt into the status line (3s, the
  // TUI status toast slot — App's flash() covers cross-component ones).
  const note = (m: string) => {
    setCmdNote(m);
    if (noteTimer.current) clearTimeout(noteTimer.current);
    noteTimer.current = setTimeout(() => setCmdNote(""), 3000);
  };

  // pasteImages is the web twin of the TUI's alt+v (cmd/lyon/paste.go):
  // clipboard bitmaps go into <workspace>/.lyon/paste through UploadFile,
  // and the composer gets the "@rel " token that resolves to them at submit
  // (tools.PromptBlocks). Same directory, same naming, same syntax as a
  // picture the model received from the terminal — one attachment contract
  // for both surfaces, so history replay reads identically.
  const [pasting, setPasting] = useState(false);
  const pasteImages = async (files: File[]) => {
    if (files.length === 0 || pasting) return;
    setPasting(true);
    note("uploading image…");
    const added: string[] = [];
    const failed: string[] = [];
    for (const f of files) {
      try {
        added.push(`@${await uploadPaste(f)} `);
      } catch (e) {
        failed.push(errText(e));
      }
    }
    if (added.length > 0) {
      const ins = added.join("");
      // Insert at the caret when the composer has it, else append — the
      // token is a normal part of the prompt, so it must not eat what the
      // user already typed.
      const el = composerRef.current;
      const at = el && el.selectionStart != null ? el.selectionStart : input.length;
      setInput((prev) => prev.slice(0, at) + ins + prev.slice(at));
      requestAnimationFrame(() => {
        const node = composerRef.current;
        if (!node) return;
        node.focus();
        const pos = at + ins.length;
        node.setSelectionRange(pos, pos);
      });
      note(`attached ${added.length} image${added.length === 1 ? "" : "s"}`);
    }
    if (failed.length > 0) setErr(failed.join("; "));
    setPasting(false);
  };

  // Clipboard images own the paste when any are present; a text-only
  // clipboard falls through to the browser's normal paste. A copied HTML
  // fragment carries no image item, a screenshot carries nothing else, so
  // the two never fight over one keystroke.
  const onPaste = (e: ReactClipboardEvent) => {
    const files: File[] = [];
    for (const item of Array.from(e.clipboardData.items)) {
      if (item.kind !== "file") continue;
      const f = item.getAsFile();
      if (f && (f.type.startsWith("image/") || f.type === "")) files.push(f);
    }
    if (files.length === 0) return;
    e.preventDefault(); // don't also paste the filename as text
    void pasteImages(files);
  };

  // execCommand runs one slash token: the local rows ChatView owns, the
  // cross-component ones routed up to App (/new /mode /steps /memory …).
  const execCommand = async (raw: string) => {
    const [cmd, ...rest] = raw.trim().split(/\s+/);
    const arg = rest.join(" ");
    switch (cmd) {
      case "/clear":
        setBlocks([]);
        note("transcript cleared (history kept)");
        break;
      case "/think":
        setThinkOpen((v) => {
          note(v ? "thinking collapsed" : "thinking expanded");
          return !v;
        });
        break;
      case "/model": {
        // The knowledge card is server-side (pkg/models); the web card is
        // the live session's effective config straight from GetSession.
        try {
          const res = await sessionClient.getSession({ sessionId: props.sessionId });
          const s = res.session;
          if (!s) throw new Error("no session record");
          setCmdOut([
            `model card — ${s.title || props.sessionId.slice(0, 8)}`,
            `  session    ${s.id}`,
            `  model      ${s.model || "engine default"}`,
            `  mode       ${s.mode || "default"}`,
            `  turns      ${s.turns}`,
            `  created    ${fmtTs(s.createdAt)}`,
            `  last seen  ${fmtTs(s.lastActive)}`,
            `  workspace  ${s.workspace}`,
          ]);
        } catch (e) {
          note(errText(e));
        }
        break;
      }
      case "/stats": {
        try {
          const res = await sessionClient.getStats({ sessionId: props.sessionId });
          let inT = 0;
          let outT = 0;
          let cr = 0;
          let cw = 0;
          for (const t of res.turns) {
            const u = t.usage;
            if (!u) continue;
            inT += Number(u.input);
            outT += Number(u.output);
            cr += Number(u.cacheRead);
            cw += Number(u.cacheCreation);
          }
          // A table lands in the drawer so it stays re-readable (TUI
          // cmdOutView); single-line receipts still use note().
          setCmdOut([
            `turns       ${res.turns.length}`,
            `input       ${inT} tok`,
            `output      ${outT} tok`,
            `cache read  ${cr} tok`,
            `cache write ${cw} tok`,
          ]);
        } catch (e) {
          note(errText(e));
        }
        break;
      }
      case "/help":
        setCmdOut([
          "commands:",
          ...SLASH.map((c) => `  ${c.cmd.padEnd(14)}${c.desc}`),
          "",
          "keys:",
          ...KEY_TABLE.map((k) => `  ${k.keys.padEnd(10)}${k.what}`),
        ]);
        break;
      case "/resume": {
        const refs = props.snapshots;
        if (refs.length === 0) {
          note("no saved session for this workspace");
          break;
        }
        if (arg !== "") {
          const n = Number(arg);
          if (!Number.isInteger(n) || n < 1 || n > refs.length) {
            note(`usage: /resume [1..${refs.length}] — bare /resume opens the picker`);
            break;
          }
          props.onRestore(refs[n - 1].path);
          break;
        }
        if (refs.length === 1) {
          props.onRestore(refs[0].path);
          break;
        }
        // Several saved: the picker owns the bottom until Enter/Esc
        // (TUI resumeAction → updatePick parity).
        setPickSel(0);
        setPick({
          title: "saved sessions — ↑↓ choose · enter restore · esc cancel",
          rows: refs.map((s) => ({
            main: s.title || "(untitled)",
            sub: `${fmtTs(s.savedAt)} · ${s.messages} msgs`,
          })),
          onPick: (i) => {
            setPick(null);
            const r = refs[i];
            if (r) props.onRestore(r.path);
          },
        });
        break;
      }
      case "/rollback": {
        if (arg === "save" || arg.startsWith("save ")) {
          const label = arg === "save" ? "" : arg.slice(5).trim();
          try {
            const res = await sessionClient.saveCheckpoint({
              sessionId: props.sessionId,
              label,
            });
            const cp = res.checkpoint;
            note(
              cp
                ? `· checkpoint #${cp.id} saved — “${cp.label}” · ${cp.messages} msgs`
                : "checkpoint saved",
            );
          } catch (e) {
            note(errText(e));
          }
          break;
        }
        try {
          const res = await sessionClient.listCheckpoints({
            sessionId: props.sessionId,
          });
          const cps = res.checkpoints;
          if (cps.length === 0) {
            note(
              "no checkpoint yet — one is captured before each prompt that had history",
            );
            break;
          }
          if (arg !== "") {
            const n = Number(arg);
            if (!Number.isInteger(n) || n < 1 || n > cps.length) {
              note(`no slot ${arg} — /rollback lists ${cps.length} checkpoints`);
              break;
            }
            props.onRollbackTo(cps[n - 1].id);
            break;
          }
          setPickSel(0);
          setPick({
            title: "checkpoints — ↑↓ choose · enter rollback · esc cancel",
            rows: cps.map((c) => ({
              main: c.label,
              sub: `${c.messages} msgs · ${fmtTs(c.at)}`,
            })),
            onPick: (i) => {
              setPick(null);
              const c = cps[i];
              if (c) props.onRollbackTo(c.id);
            },
          });
        } catch (e) {
          note(errText(e));
        }
        break;
      }
      case "/undo": {
        // Side-ref walk (blueprint §四): restore the last run's file
        // changes and step refs/lyon/auto/<sid> down its chain; the
        // flash list is the disclosure (post-commit user edits on the
        // same file are overwritten — no 3-way merge at this scale).
        try {
          const res = await sessionClient.undoLastRun({
            sessionId: props.sessionId,
          });
          const paths = res.restoredPaths;
          note(
            paths.length > 0
              ? `reverted: ${paths.join(" · ")}${res.ref ? "" : " · chain exhausted"}`
              : "nothing to revert",
          );
          props.onChanged(); // the files panel's diff rows are now stale
        } catch (e) {
          note(errText(e));
        }
        break;
      }
      case "/remember":
        // The TUI extracts server-side (DistillWithReport); the web has no
        // distill RPC yet, so steer the model to the same save_memory seam.
        note("distilling… — watch for save_memory calls");
        await dispatchPrompt(REMEMBER_PROMPT);
        break;
      default:
        // Keep the argument tail — App parses "/steps width N" itself.
        void props.onCommand(raw.trim());
    }
  };

  // dispatchPrompt is the single submission path (input box and
  // /remember): a busy seat queues, an idle one submits.
  const dispatchPrompt = async (prompt: string) => {
    setErr("");
    // §7.3 arm: the reply collector matches the user_message echo against
    // this exact text — arming only in the float-bar effect left a
    // composer-typed selection ask uncollected, so its map_ops reply
    // fell on the floor (Image #64 选中提问无效).
    if (SEL_MARKER_RE.test(prompt))
      selAskRef.current = { text: prompt, collecting: false, buf: "", done: false };
    try {
      if (busy) {
        // Mid-run steering (steer.ts — mirror of pkg/agent/steer.go): the
        // intent decides the mechanism. Stop fires immediately; redirect
        // stops the run and jumps the parked queue (the prompt_end re-send
        // below runs it next); supplement parks WITHOUT gap-injection —
        // the working task must not see it mid-flight; everything else
        // enqueues at the next turn gap like before.
        const intent = classifySteer(prompt);
        if (intent === "stop") {
          await sessionClient.interrupt({ sessionId: props.sessionId });
          note("已停止当前任务");
          props.onChanged();
          return;
        }
        if (intent === "redirect" || intent === "supplement") {
          if (intent === "redirect") {
            // Park BEFORE the interrupt (urgent: jumps older parked
            // lines) — an interrupt failure must never swallow the line;
            // parked, it still rides the next prompt_end.
            setQueued((q) => [prompt, ...q]);
            await sessionClient.interrupt({ sessionId: props.sessionId });
            note("换方向 — 当前任务停止后处理新问题");
          } else {
            setQueued((q) => [...q, prompt]);
            note("补充 — 已收录，当前任务完成后处理");
          }
          props.onChanged();
          return;
        }
        try {
          await sessionClient.enqueuePrompt({
            sessionId: props.sessionId,
            prompt,
          });
        } catch (e) {
          // FailedPrecondition = the run ended between our busy check and
          // the RPC: the server maps ErrNoPromptRunning there and documents
          // "the caller should fall back to SubmitPrompt"
          // (connect_session.go). Submit instead of dropping the line —
          // without this fallback the input box already consumed the prompt
          // and it evaporates with only a transient error to show for it.
          if (ConnectError.from(e).code !== Code.FailedPrecondition) throw e;
          setBusy(true);
          await sessionClient.submitPrompt({
            sessionId: props.sessionId,
            prompt,
          });
          props.onChanged();
          return;
        }
        setBusy(true);
        // Show the line itself, not just its depth (TUI queueRows): the
        // point of parking a prompt is seeing what will join the run. The
        // RPC's queueLen is admission only — it counts lines other clients
        // also parked, which this pane cannot render, so the list is our
        // own mirror.
        setQueued((q) => [...q, prompt]);
      } else {
        setBusy(true);
        // No local echo: SubmitPrompt injects the prompt as a
        // user_message hub event, so live and replayed transcripts stay
        // identical (server path, connect_session.go).
        await sessionClient.submitPrompt({
          sessionId: props.sessionId,
          prompt,
        });
      }
      props.onChanged();
    } catch (e) {
      setBusy(false);
      setErr(errText(e));
    }
  };
  // The merge window fires long after this render — hand dispatchPrompt
  // (with its live busy/queued closure) to the flush through the ref.
  dispatchRef.current = dispatchPrompt;

  const send = async () => {
    const typed = input.trim();
    if (!typed) return;
    setInput("");
    setErr("");
    // A slash token (with or without args) dispatches instead of sending:
    // "/rollback" alone or "/rollback anything" both reach execCommand.
    if (typed.startsWith("/")) {
      await execCommand(typed);
      return;
    }
    // §7.3: a live selection marks the composer's line exactly like the
    // float bar does (Image #64 — typed bare, the model saw no selection
    // at all). Already-marked text (↑ recall of a past ask) is not
    // wrapped twice.
    const prompt =
      props.mmSelIds && props.mmSelIds.length > 0 && !SEL_MARKER_RE.test(typed)
        ? buildSelAsk(props.mmSelIds, typed)
        : typed;
    setHist((h) => [...h, typed]); // ↑ browses what the human typed
    setHistIdx(-1);
    await dispatchPrompt(prompt);
  };

  const interrupt = async () => {
    try {
      await sessionClient.interrupt({ sessionId: props.sessionId });
    } catch (e) {
      setErr(errText(e));
    }
  };

  // Prop handlers change identity on every App render (divider drags
  // fire per mousemove) — route them through a ref so the feed memo
  // below keeps stable callback identities.
  const chatProps = useRef(props);
  chatProps.current = props;
  const stOpenFile = useCallback((p: string) => chatProps.current.onOpenFile?.(p), []);
  const stOpenDiff = useCallback((p: string) => chatProps.current.onOpenDiff?.(p), []);
  const stMmSelect = useCallback(
    (ids: string[] | null) => chatProps.current.onMmSelect?.(ids),
    [],
  );

  // The transcript is the expensive part of this component: typing and
  // divider drags re-render ChatView constantly, so the feed JSX is
  // cached until its DATA changes (identical elements make React bail
  // out of the subtree). Fan-out ordinals ride inside: first-appearance
  // order of each SubID — the chip that ties a sub row to its task.
  const feedRows = useMemo(() => {
    const subOrd = new Map<string, number>();
    for (const b of blocks) {
      const sid = b.kind === "subtask" ? b.subId : b.kind === "tool" ? b.subId : "";
      if (sid && !subOrd.has(sid)) subOrd.set(sid, subOrd.size + 1);
    }
    return blocks.map((b, i) => (
      <BlockView
        key={`${i}:${b.kind}`}
        block={b}
        thinkOpen={thinkOpen}
        ord={
          b.kind === "subtask" || (b.kind === "tool" && b.subId)
            ? subOrd.get(b.subId ?? "")
            : undefined
        }
        live={busy && i === lastThinking(blocks)}
        autoOpen={busy && i === lastThinking(blocks) && !textAfter(blocks, i)}
        ticking={busy && i === blocks.length - 1}
        hinted={i === lastThinking(blocks)}
        onOpenFile={stOpenFile}
        onOpenDiff={stOpenDiff}
        mmNodes={mindmap?.nodes ?? null}
        onMmSelect={stMmSelect}
      />
    ));
  }, [blocks, busy, thinkOpen, mindmap, stOpenFile, stOpenDiff, stMmSelect]);

  // Click-jump (§7.1 core): the map's source_turn counts user turns, and
  // the feed's children mirror blocks 1:1 — so the Nth user block is the
  // target. A turn the ring no longer holds (compacted away) is a no-op.
  const jumpToTurn = (turn: number) => {
    const feed = feedRef.current;
    if (!feed || turn < 1) return;
    const kids = feed.children;
    let count = 0;
    let hit: Element | null = null;
    const n = Math.min(blocks.length, kids.length);
    for (let i = 0; i < n; i++) {
      if (blocks[i].kind === "user" && ++count === turn) {
        hit = kids[i];
        break;
      }
    }
    if (!hit) return;
    const target = hit;
    target.scrollIntoView({ block: "center", behavior: "smooth" });
    // re-trigger the flash even when a previous jump's class lingers
    target.classList.remove("mm-jump");
    void (target as HTMLElement).offsetWidth;
    target.classList.add("mm-jump");
    window.setTimeout(() => target.classList.remove("mm-jump"), 1600);
  };

  // The map's clicks land in App's pane (#51) and arrive as seq'd
  // requests — seq bumps per click so jumping the same turn twice
  // still re-fires the scroll + flash.
  useEffect(() => {
    if (!props.jumpTo) return;
    jumpToTurn(props.jumpTo.turn);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [props.jumpTo?.seq]);

  // M2 edit-channel responses from the pane: rev set = the RPC's fresh
  // nodes applied under the same guard (the stream event dedups by
  // revision); rev undefined = a §5 conflict — refetch the authoritative
  // snapshot instead of showing a half-stale map.
  useEffect(() => {
    const cmd = props.mmCmd;
    // Image #69: commands are session-tagged — a cmd left over from the
    // outgoing session must not paint this fresh pane (child effects run
    // BEFORE App's switch reset, so the tag is the only reliable guard).
    if (!cmd || cmd.sid !== props.sessionId) return;
    void (async () => {
      if (cmd.rev === undefined) {
        try {
          const mm = await mindmapClient.getMindmap({ sessionId: props.sessionId });
          if (mm) applyMindmap(Number(mm.revision), mm.title, mm.subtitle, mm.nodesJson);
        } catch {
          // transient — the next mindmap event re-syncs anyway
        }
        return;
      }
      applyMindmap(cmd.rev, undefined, undefined, cmd.nodesJson);
    })();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [props.mmCmd?.seq]);

  return (
    <div className={"chat" + (busy ? " streaming" : "")}>
      <div
        className="chat-feed"
        ref={feedRef}
        onScroll={onFeedScroll}
        onWheel={(e) => {
          // Scrolling up is intent, not position: wheel-up disarms the
          // follow before the next block can yank the view back down
          // (the scroll event alone races the render effect).
          if (e.deltaY < 0) stickRef.current = false;
        }}
      >
        {feedRows}
        {blocks.length === 0 && (
          <div className="placeholder">
            live events appear here — submit a prompt below
          </div>
        )}
        {/* The map lives in the mindmap pane, a peer of chat (#51) —
            jumps arrive via props.jumpTo, handled next to jumpToTurn. */}
      </div>
      {err && (
        <div className="chat-err-row">
          <span className="err chat-err">{err}</span>
          <button
            className="ghost"
            onClick={() => {
              setErr("");
              setStreamKey((k) => k + 1);
            }}
          >
            retry
          </button>
        </div>
      )}
      {pendings.length > 0 && (
        <div className="approval-stack">
          {pendings.map((p, i) => {
            const isMode = p.modeBlock;
            const hasPath = modeHasPath(p);
            // One card, two vocabularies: an approval asks consent
            // (y/n/a), a plan interception answers policy (f/d/b/n).
            const decide = (approve: boolean, remember: boolean, action?: ModeAction) =>
              props.onDecide(p.sessionId, p.execId, approve, remember, action);
            return (
              <div
                key={`${p.sessionId}:${p.execId}`}
                className={
                  "approval-card" +
                  (i === sel ? " active" : "") +
                  (isMode ? " mode" : "")
                }
              >
                <div className="appr-head">
                  <span className="appr-title">
                    <span className="appr-q">{isMode ? "plan 拦截" : "approve?"}</span>
                    <span className="tool-name">{p.toolName}</span>
                  </span>
                  {i === sel && (
                    <span className="appr-hint">
                      {isMode
                        ? hasPath
                          ? "f 仅该文件 · d 切 default · b 切 bypass · n 拒绝"
                          : "d 切 default · b 切 bypass · n 拒绝"
                        : "y approve · n deny · a always · ↑↓ switch"}
                    </span>
                  )}
                </div>
                {p.inputJson && (
                  <pre className="appr-input">{prettyInput(p.inputJson)}</pre>
                )}
                <div className="appr-actions">
                  {isMode ? (
                    <>
                      {hasPath && (
                        <button
                          onClick={() => decide(false, false, ModeAction.ALLOW_PATH)}
                        >
                          仅该文件 f
                        </button>
                      )}
                      <button
                        className="ghost"
                        onClick={() => decide(true, false, ModeAction.MODE_DEFAULT)}
                      >
                        切 default d
                      </button>
                      <button
                        className="ghost"
                        onClick={() => decide(true, false, ModeAction.MODE_BYPASS)}
                      >
                        切 bypass b
                      </button>
                      <button
                        className="ghost"
                        onClick={() => decide(false, false, ModeAction.DENY)}
                      >
                        拒绝 n
                      </button>
                    </>
                  ) : (
                    <>
                      <button onClick={() => decide(true, false)}>approve</button>
                      <button className="ghost" onClick={() => decide(false, false)}>
                        deny
                      </button>
                      <button className="ghost" onClick={() => decide(true, true)}>
                        always
                      </button>
                    </>
                  )}
                </div>
              </div>
            );
          })}
        </div>
      )}
      {slashOpen && slashMatches.length > 0 && (
        <div className="slash-panel">
          <div className="slash-tabs">
            {SLASH_CATS.map((c, i) => (
              <button
                key={c}
                className={"slash-tab" + (i === slashCat ? " active" : "")}
                onMouseDown={(e) => e.preventDefault()} // keep textarea focus
                onClick={() => setSlashCat(i)}
              >
                {c}
              </button>
            ))}
          </div>
          {slashMatches.map((c, i) => (
            <div
              key={c.cmd}
              className={"slash-row" + (i === ssel ? " sel" : "")}
              onMouseDown={(e) => {
                e.preventDefault(); // keep focus; run on click
                setInput("");
                void execCommand(c.cmd);
              }}
            >
              <span className="slash-cmd">{c.cmd}</span>
              <span className={`slash-kind ${c.kind}`}>{c.kind}</span>
              <span className="slash-desc">{c.desc}</span>
            </div>
          ))}
        </div>
      )}
      {atOpen && atEntries.length > 0 && (
        <div className="slash-panel at-panel">
          {atEntries.map((en, i) => (
            <div
              key={en.cmd}
              className={"slash-row" + (i === asel ? " sel" : "")}
              onMouseDown={(e) => {
                e.preventDefault(); // keep focus; insert in place
                atInsert(en);
              }}
            >
              <span className="slash-cmd">{en.cmd}</span>
              <span className="slash-desc">{en.desc}</span>
            </div>
          ))}
        </div>
      )}
      {pick && (
        <div className="slash-panel pick-panel">
          <div className="pick-title">{pick.title}</div>
          {pick.rows.map((r, i) => (
            <div
              key={`${r.main}:${i}`}
              className={"slash-row" + (i === pickSel ? " sel" : "")}
              onMouseDown={(e) => {
                e.preventDefault(); // keep focus; restore/rollback on click
                pick.onPick(i);
              }}
            >
              <span className="pick-num">{i + 1}</span>
              <span className="pick-main">{r.main}</span>
              <span className="slash-desc">{r.sub}</span>
            </div>
          ))}
        </div>
      )}
      {queued.length > 0 && (
        <div className="queue-list">
          <div className="queue-head">queued ({queued.length}):</div>
          {queued.map((q, i) => {
            // Parked prompts keep their full wire (the re-send needs it);
            // the row shows only the human line — marker → chips gone,
            // discipline suffix stripped (Image #65).
            const shown = oneLine(stripWireForDisplay(q).body);
            return (
              <div className="queue-row" key={`${i}:${q}`}>
                <span className="queue-num">{i + 1}›</span>
                <span className="queue-text" title={shown}>
                  {shown}
                </span>
              </div>
            );
          })}
        </div>
      )}
      {busy && input.trim() && (
        <div className="queue-preview">
          <span className="queue-caret">⏎</span>
          <span className="queue-text">{input.trim().slice(0, 120)}</span>
          <span className="queue-hint">Enter queues for the next turn gap</span>
        </div>
      )}
      {cmdOut && <OutputDrawer lines={cmdOut} onClose={() => setCmdOut(null)} />}
      <div className="status-line">
        {/* Run-state indicator only: the plan itself lives in the plan
            pane, so a finished prompt must not leave 6/6 parked above the
            input box. */}
        {busy && todos.length > 0 && (
          <button
            type="button"
            className="sl-plan"
            title="open the plan pane"
            onClick={() => props.onOpenPlan?.()}
          >
            {(() => {
              const p = todoProgress(todos);
              return `📋 ${p.done}/${p.total}${p.current ? ` ▸ ${p.current}` : ""}`;
            })()}
          </button>
        )}
        <span className="sl-fill" />
        {cmdNote ? (
          <span className="sl-note">{cmdNote}</span>
        ) : pendings.length > 0 ? (
          <span className="sl-appr">
            {pendings[sel]?.modeBlock
              ? modeHasPath(pendings[sel])
                ? "plan 拦截 — f 仅该文件 · d default · b bypass · n 拒绝"
                : "plan 拦截 — d default · b bypass · n 拒绝"
              : "approval pending — y approve · n deny · a always"}
          </span>
        ) : busy ? (
          <span className="sl-hint">
            <span className="spin" aria-hidden="true" />
            running ·{" "}
            <button
              type="button"
              className="sl-stop"
              onClick={() => void interrupt()}
            >
              interrupt
            </button>{" "}
            to stop · Enter queues
          </span>
        ) : null}
      </div>
      {props.mmSelIds && props.mmSelIds.length > 0 && (
        <div className="mm-selhint">
          <span className="mm-selhint-count">
            对 {props.mmSelIds.length} 个节点提问 — 输入框直接发送即带选区（浮条：改写
            / 矛盾吗 / 解释）
          </span>
          <button
            type="button"
            className="ghost mm-selhint-clear"
            onClick={() => props.onMmSelect?.(null)}
          >
            清除 (Esc)
          </button>
        </div>
      )}
      <div className="chat-input">
        <textarea
          ref={composerRef}
          rows={1}
          value={input}
          placeholder={busy ? "busy — Enter queues for the next turn gap" : "prompt (Enter to send) · / for commands"}
          onChange={(e) => setInput(e.target.value)}
          onPaste={onPaste}
          onKeyDown={(e) => {
            // Pick popups own the keyboard outright (TUI updatePick):
            // ↑↓ walk, Enter pick, Esc cancel, everything else swallowed —
            // the input line stays empty underneath.
            if (pick) {
              if (e.nativeEvent.isComposing) return;
              if (e.key === "ArrowDown") {
                e.preventDefault();
                setPickSel((s) => (s + 1) % pick.rows.length);
                return;
              }
              if (e.key === "ArrowUp") {
                e.preventDefault();
                setPickSel((s) => (s - 1 + pick.rows.length) % pick.rows.length);
                return;
              }
              if (e.key === "Enter" && !e.shiftKey) {
                e.preventDefault();
                pick.onPick(Math.min(pickSel, Math.max(0, pick.rows.length - 1)));
                return;
              }
              if (e.key === "Escape") {
                e.preventDefault();
                setPick(null);
                return;
              }
              e.preventDefault(); // the list owns the bottom until it closes
              return;
            }
            // @ popup first: it owns the keys while a trailing "@…" token
            // has entries. (It and the slash panel are mutually exclusive —
            // a bare "/…" token can never be an @token.)
            if (atOpen && atEntries.length > 0) {
              if (e.key === "ArrowDown") {
                e.preventDefault();
                setAtSel((s) => (s + 1) % atEntries.length);
                return;
              }
              if (e.key === "ArrowUp") {
                e.preventDefault();
                setAtSel((s) => (s - 1 + atEntries.length) % atEntries.length);
                return;
              }
              if ((e.key === "Tab" || e.key === "Enter") && !e.shiftKey) {
                e.preventDefault(); // insert the pick in place
                atInsert(atEntries[asel]);
                return;
              }
              if (e.key === "Escape") {
                e.preventDefault();
                setAtOff(true); // dismissed until the input changes
                return;
              }
            }
            // Slash panel: it is open only while the input is one bare
            // token, so these keys never fight prose editing.
            if (slashOpen && slashMatches.length > 0) {
              if (e.key === "ArrowDown") {
                e.preventDefault();
                setSlashSel((s) => (s + 1) % slashMatches.length);
                return;
              }
              if (e.key === "ArrowUp") {
                e.preventDefault();
                setSlashSel(
                  (s) => (s - 1 + slashMatches.length) % slashMatches.length,
                );
                return;
              }
              // ←/→ flip category tabs on a bare "/" (TUI sugCat); with a
              // prefix typed they stay plain cursor movement.
              if (e.key === "ArrowLeft" && slashBare) {
                e.preventDefault();
                setSlashCat(
                  (c) => (c - 1 + SLASH_CATS.length) % SLASH_CATS.length,
                );
                return;
              }
              if (e.key === "ArrowRight" && slashBare) {
                e.preventDefault();
                setSlashCat((c) => (c + 1) % SLASH_CATS.length);
                return;
              }
              if (e.key === "Tab" && !e.shiftKey) {
                e.preventDefault(); // complete the bare token in place
                setInput(slashMatches[ssel].cmd);
                return;
              }
              if (e.key === "Enter" && !e.shiftKey) {
                e.preventDefault();
                const pick = slashMatches[ssel] ?? slashMatches[0];
                setInput("");
                void execCommand(pick.cmd);
                return;
              }
              if (e.key === "Escape") {
                e.preventDefault();
                setInput("");
                return;
              }
            }
            // ↑↓ walks the sent-prompt history while the line is empty —
            // the TUI readline gesture; a fresh line starts at newest.
            if (e.key === "ArrowUp" && input === "" && hist.length > 0) {
              e.preventDefault();
              const next = histIdx < 0 ? hist.length - 1 : Math.max(0, histIdx - 1);
              setHistIdx(next);
              setInput(hist[next]);
              return;
            }
            if (e.key === "ArrowDown" && histIdx >= 0) {
              e.preventDefault();
              const next = histIdx + 1;
              if (next >= hist.length) {
                setHistIdx(-1);
                setInput("");
              } else {
                setHistIdx(next);
                setInput(hist[next]);
              }
              return;
            }
            if (e.key === "Enter" && !e.shiftKey) {
              e.preventDefault();
              void send();
            }
          }}
        />
        <button onClick={() => void send()} disabled={busy && false}>
          {busy ? "queue" : "send"}
        </button>
      </div>
    </div>
  );
}

// subHue picks a readable accent per fan-out task number — distinct
// enough to re-gather one task's interleaved steps, muted enough to sit
// in the existing palette.
function subHue(ord: number): string {
  return `hsl(${(ord * 67) % 360} 62% 62%)`;
}

// Turn durations use the same shape as tool cards — a whole prompt that
// answered in 3.4s and one that took 12m04s are both readable at a glance.

function BlockView({
  block,
  thinkOpen,
  live,
  autoOpen,
  ticking,
  ord,
  onOpenFile,
  onOpenDiff,
  hinted,
  mmNodes,
  onMmSelect,
}: {
  block: Block;
  thinkOpen: boolean;
  live?: boolean;
  /** Auto-expand the row: the run's current thinking with no conclusion
   *  text after it yet (#40). Long rows still self-fold below. */
  autoOpen?: boolean;
  /** The duration clock runs only on the feed's trailing block — a tool
   *  row after this thinking means the call is over (#36). */
  ticking?: boolean;
  /** Fan-out task number (1-based) for subtask cards and sub rows. */
  ord?: number;
  /** files_changed rows click into the preview pane (App.openPreviewFile). */
  onOpenFile?: (path: string) => void;
  /** …except change rows, which open the 变更模式 diff (#42, card-only). */
  onOpenDiff?: (path: string) => void;
  /** The feed's last thinking row carries the fold hint (one per feed). */
  hinted?: boolean;
  /** §7.3 selection-ask rows: node snapshot for chip labels + chip click
   *  re-selects the ids in the map pane. */
  mmNodes?: MapNode[] | null;
  onMmSelect?: (ids: string[] | null) => void;
}) {
  switch (block.kind) {
    case "user": {
      // §7.3: the marker rides the prompt text itself (ids in the wire
      // string, labels from the snapshot) — strip ALL wire-only text
      // before rendering: the marker becomes clickable label chips, and
      // the 【系统 ·…】 discipline suffix never shows (Image #65: raw
      // system prompt in the feed reads as noise).
      const { marked, ids, body } = stripWireForDisplay(block.text);
      if (marked) {
        const byId = new Map((mmNodes ?? []).map((n) => [n.id, n]));
        return (
          <div className="blk-user">
            <div className="mm-usrchips">
              {ids.map((id) => (
                <button
                  key={id}
                  type="button"
                  className="mm-chip-user"
                  title={byId.get(id)?.label ?? id}
                  onClick={() => onMmSelect?.(ids)}
                >
                  {byId.get(id)?.label ?? id.slice(0, 8)}
                </button>
              ))}
            </div>
            <div className="user-text">{body}</div>
          </div>
        );
      }
      return (
        <div className="blk-user">
          <div className="user-text">{body}</div>
        </div>
      );
    }
    case "text":
      return (
        <div className="blk-text md">
          <Suspense fallback={<pre>{block.text}</pre>}>
            <MarkdownView>{block.text}</MarkdownView>
          </Suspense>
          {block.turnMs || block.endedAt ? (
            <div className="turn-ms">
              {block.endedAt ? fmtStamp(block.endedAt) : null}
              {block.endedAt && block.turnMs ? " · " : null}
              {block.turnMs ? fmtDuration(block.turnMs) : null}
            </div>
          ) : null}
        </div>
      );
    case "thinking": {
      // autoOpen (newest thinking, no conclusion text after it yet) is
      // the run to watch: open it so the reader can follow (#47) — but
      // only while it stays short: once it passes LIVE_THINK_MAX_LINES
      // it folds itself so a long chain-of-thought doesn't bury the
      // feed. A global ctrl+o fold opens it for the ordinary reason (the
      // hint then reads the chord, not live).
      const isLive = !!live;
      const auto =
        !!autoOpen && !thinkOpen && lineCount(block.text) <= LIVE_THINK_MAX_LINES;
      return (
        <ThinkingBlock
          key={`${String(thinkOpen)}:${String(auto)}:${String(isLive)}`}
          block={block}
          thinkOpen={thinkOpen || auto}
          live={isLive}
          ticking={!!ticking}
          hinted={!!hinted}
        />
      );
    }
    case "tool": {
      // Folded-header digest: what this call is about, at a glance
      // (command, path, pattern…). Empty until the exec event fills
      // input; the <details> body keeps the full input/output.
      const summary = toolSummary(block.name, block.input);
      // Result peek: the output's first line rides the folded row so a
      // finished call reads as "what ran → what came back" without a
      // click; failures prefer the error line. Hidden while open — the
      // body already shows the full text.
      const peekSrc =
        block.status === "failed" && block.error ? block.error : block.output;
      const outPeek =
        block.status === "done" ||
        block.status === "failed" ||
        block.status === "server"
          ? firstLine(peekSrc)
          : "";
      return (
        <details
          className={
            "blk-tool" +
            (block.status === "failed" ? " failed" : "") +
            (block.sub ? " sub" : "")
          }
          // Fan-out tint: same accent as the owning task's chip, so the
          // eye can gather one task's interleaved steps back together.
          style={ord ? { borderLeftColor: subHue(ord) } : undefined}
        >
          <summary className="blk-tool-head">
            <div className="blk-tool-row">
              <span className="tool-title">
                <span className="tool-chevron" aria-hidden="true">
                  ▸
                </span>
                <span className="tool-name">{block.name}</span>
                {block.sub && (
                  <span
                    className="tool-badge sub"
                    style={ord ? { color: subHue(ord), borderColor: subHue(ord) } : undefined}
                  >
                    {ord ? `sub ${ord}` : "sub"}
                  </span>
                )}
                {block.server && (
                  <span className="tool-badge server">server</span>
                )}
              </span>
              {summary && <span className="tool-summary">{summary}</span>}
              <span className={`tool-status ${block.status}`}>
                {block.status === "running" && (
                  <span className="spin" aria-hidden="true" />
                )}
                {block.status === "pending" && "…"}
                {block.status === "running" && "running"}
                {block.status === "running" && block.startedAt !== undefined && (
                  <RunningClock since={block.startedAt} />
                )}
                {/* Replayed history has no timing — show the marker bare
                    instead of a misleading "0ms". */}
                {block.status === "done" &&
                  (block.durationMs ? `✓ ${fmtDuration(block.durationMs)}` : "✓")}
                {block.status === "failed" &&
                  (block.durationMs ? `✗ ${fmtDuration(block.durationMs)}` : "✗")}
                {block.status === "server" && "🌐 server"}
              </span>
            </div>
            {outPeek && <div className="tool-out-peek">{outPeek}</div>}
          </summary>
          {block.input && <pre className="tool-input">{prettyInput(block.input)}</pre>}
          {block.output && (
            <pre className="tool-output">{colorizeOutput(block.output)}</pre>
          )}
          {block.error && <div className="err">{block.error}</div>}
        </details>
      );
    }
    case "subtask": {
      const meta =
        block.status === "running"
          ? `running · ${block.steps} step${block.steps === 1 ? "" : "s"} · ${block.model}`
          : block.status === "done"
            ? `✓ ${fmtDuration(block.durationMs)}${block.calls ? ` · ${block.calls}` : ""} · ${block.steps} steps · ${block.model}`
            : `✗ ${fmtDuration(block.durationMs)} · ${block.steps} steps · ${block.model}`;
      return (
        // Key on status: remount flips open (auto-shows while running,
        // folds to its header when done — same trick as ThinkingBlock).
        <details
          key={block.status}
          className={`blk-subtask ${block.status}`}
          open={block.status === "running"}
        >
          <summary>
            <span
              className="sub-chip"
              style={ord ? { color: subHue(ord), borderColor: subHue(ord) } : undefined}
            >
              {ord ?? "•"}
            </span>
            <span className="sub-task">{block.task || "(subtask)"}</span>
            {block.status === "running" && (
              <span className="tool-status running">
                <span className="spin" aria-hidden="true" />
              </span>
            )}
            <span className={`tool-status ${block.status}`}>
              {block.status === "done" && "done"}
              {block.status === "failed" && "failed"}
            </span>
            <span className="sub-meta">{meta}</span>
          </summary>
          {block.answer && (
            <div className="sub-answer">
              <span className="sub-answer-label">answer</span>
              {block.answer}
            </div>
          )}
          {block.error && <div className="err">{block.error}</div>}
        </details>
      );
    }
    case "files":
      // 产出文件 card — one row per file the run created or touched.
      // Routing (#42): a CHANGE row (M/A/D/R/C — 无论是否已 add) opens the
      // 变更模式 diff, because "改了啥" is the question a badge answers;
      // an untracked new file has nothing to compare, so it keeps the
      // plain preview. The diff is reachable ONLY from this card.
      return (
        <div className="blk-files">
          <div className="blk-files-head">产出文件 · {block.files.length}</div>
          {block.files.map((f) => (
            <button
              key={f.path}
              type="button"
              className="blk-file-row"
              onClick={() =>
                f.kind === "untracked" || !onOpenDiff
                  ? onOpenFile?.(f.path)
                  : onOpenDiff(f.path)
              }
              title={f.path}
            >
              <FileIcon name={f.path} isDir={false} />
              <span className="blk-file-path">{f.path}</span>
              <span className={"file-badge " + gitBadge(f).cls}>
                {gitBadge(f).letter}
              </span>
            </button>
          ))}
        </div>
      );
    case "notice":
      if (block.detail) {
        return (
          <details className="blk-notice">
            <summary>{block.text}</summary>
            <pre className="notice-detail">{block.detail}</pre>
          </details>
        );
      }
      return <div className="blk-notice">{block.text}</div>;
    case "error":
      return <div className="err">{block.text}</div>;
  }
}

// RunningClock: a running tool stays just a spinner for the first 5s —
// then the elapsed time appears and ticks, because "running" with no
// number for minutes is what makes people glance at the clock (#37).
// Client-receipt based (same clock as the tick), so it never mixes with
// the server's event timestamps.
function RunningClock({ since }: { since: number }) {
  const [, setTick] = useState(0);
  useEffect(() => {
    const id = setInterval(() => setTick((t) => t + 1), 250);
    return () => clearInterval(id);
  }, []);
  const elapsed = Date.now() - since;
  if (elapsed < 5000) return null;
  return <span className="tool-elapsed">{fmtDuration(elapsed)}</span>;
}

// ThinkingBlock splits out so it can track its own open state: ctrl+o
// flips thinkOpen (the key remounts and re-seeds), while a manual summary
// click rides the native toggle event. The hint reads whichever way the
// block currently sits — an open block says collapse, not expand; a live
// block says live, and offers the way back in once it has folded itself —
// but only on the feed's LAST thinking row: a chord printed on every
// folded row is noise, the eye should land in one place.
function ThinkingBlock({
  block,
  thinkOpen,
  live,
  ticking,
  hinted,
}: {
  block: Extract<Block, { kind: "thinking" }>;
  thinkOpen: boolean;
  live: boolean;
  /** Duration-clock switch: only the trailing block of a live run ticks —
   *  a tool row landing after this thinking means the call is over, and
   *  the number freezes at the real span (#36: run_shell 阶段还在计时). */
  ticking: boolean;
  hinted: boolean;
}) {
  const [open, setOpen] = useState(thinkOpen);
  // Ticking rows count between deltas: server span-so-far plus the client
  // tail since the last chunk — durations only, the clocks never mix.
  const [tick, setTick] = useState(() => Date.now());
  useEffect(() => {
    if (!ticking) return;
    const id = setInterval(() => setTick(Date.now()), 250);
    return () => clearInterval(id);
  }, [ticking]);
  const hint = !hinted
    ? ""
    : live
      ? open
        ? "live"
        : "live · ctrl+o to expand"
      : open
        ? "ctrl+o to collapse"
        : "ctrl+o to expand";
  // Duration = where the run's wall time actually went (a 39-minute run
  // with millisecond tools is thinking time — now it shows on the row).
  const durMs =
    block.t0 === undefined
      ? 0
      : ticking
        ? (block.t1 ?? block.t0) - block.t0 + (tick - (block.lc ?? tick))
        : (block.t1 ?? block.t0) - block.t0;
  // Tokens: the call's actual output once turn_end stamped it, else a
  // live estimate while the row is still receiving.
  const tokens =
    block.tokens ?? (live ? estimateTokens(block.text) : undefined);
  // Folded peek: the reasoning's first line rides the header so a
  // closed block still says *why* the next step happens — the whole
  // complaint behind "thinking 不好阅读". Hidden while open.
  const peek = open ? "" : firstLine(block.text);
  return (
    <details
      className="blk-thinking"
      open={thinkOpen}
      onToggle={(e) => setOpen(e.currentTarget.open)}
    >
      <summary>
        thinking
        {durMs > 0 && (
          <span className="think-stats">{fmtDuration(durMs)}</span>
        )}
        {tokens !== undefined && (
          <span className="think-stats">
            ↓ {block.tokens === undefined ? "~" : ""}
            {fmtTokens(tokens)} tokens
          </span>
        )}
        {peek && <span className="think-peek">{peek}</span>}
        <span className={"think-key" + (live ? " live" : "")}>{hint}</span>
      </summary>
      <pre>{block.text}</pre>
    </details>
  );
}
