import { lazy, Suspense, useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ClipboardEvent as ReactClipboardEvent } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import type { SessionEvent } from "../gen/lyon/v1/event_pb";
import { ModeAction, type WatchPendingResponse } from "../gen/lyon/v1/approval_pb";
import type { SnapshotRef } from "../gen/lyon/v1/dashboard_pb";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import type { Timestamp } from "@bufbuild/protobuf/wkt";
import { errText, fileClient, mindmapClient, sessionClient } from "../api";
import { FileIcon, gitBadge } from "../fileIcons";
import { uploadPaste } from "../paste";
import { parseTodos } from "../todos";
import type { TodoItem } from "../todos";
import { parseNodes } from "../mindmap";
import type { MindmapSnap } from "../mindmap";
import type { StepRow } from "../types";
import { todoProgress } from "./PlanPane";
import OutputDrawer from "./OutputDrawer";
import MindMapCard from "./MindMapCard";
import { KEY_TABLE } from "../keys";
import { fmtDuration } from "../fmt";

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
  | { kind: "text"; text: string; turnMs?: number }
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

function appendToLast(
  blocks: Block[],
  kind: "text" | "thinking",
  delta: string,
): Block[] {
  const last = blocks[blocks.length - 1];
  if (last && last.kind === kind) {
    return [...blocks.slice(0, -1), { ...last, text: last.text + delta }];
  }
  return [...blocks, { kind, text: delta }];
}

function replaceAt(blocks: Block[], i: number, next: Block): Block[] {
  return [...blocks.slice(0, i), next, ...blocks.slice(i + 1)];
}

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
function reduce(blocks: Block[], ev: SessionEvent): Block[] {
  switch (ev.typeName) {
    case "user_message":
      return ev.text ? [...blocks, { kind: "user", text: ev.text }] : blocks;
    case "text_delta":
      return appendToLast(blocks, "text", ev.text);
    case "thinking_delta": {
      // Stamps ride the row: t0/t1 from the event's server TS (replay
      // computes the same duration history showed live), lc from the
      // client receipt (the live tick's tail).
      const ts = ev.ts ? timestampDate(ev.ts).getTime() : Date.now();
      const last = blocks[blocks.length - 1];
      if (last && last.kind === "thinking") {
        return replaceAt(blocks, blocks.length - 1, {
          ...last,
          text: last.text + ev.thinking,
          t1: ts,
          lc: Date.now(),
        });
      }
      return [
        ...blocks,
        { kind: "thinking", text: ev.thinking, t0: ts, t1: ts, lc: Date.now() },
      ];
    }
    case "tool_use_start": {
      const card = newToolBlock(ev);
      if (ev.server) {
        // Server-side tool: the API already has the input and executes
        // it itself — one card carries query + result, no exec events.
        return [
          ...blocks,
          { ...card, status: "server", input: ev.inputJson },
        ];
      }
      // Harness tool: the API's tool_use carries no input yet — the
      // exec row below fills it in. Parallel calls each get their own
      // card, tagged by tool id.
      return [...blocks, card];
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
      let next: Block[];
      if (i >= 0) next = replaceAt(blocks, i, filled(blocks[i] as Extract<Block, { kind: "tool" }>));
      else next = [...blocks, filled({ ...newToolBlock(ev), status: "pending" })];
      // A fan-out inner step also ticks its task's live counter.
      if (ev.sub && ev.subId) {
        for (let j = next.length - 1; j >= 0; j--) {
          const b = next[j];
          if (b.kind === "subtask" && b.subId === ev.subId) {
            next = replaceAt(next, j, { ...b, steps: b.steps + 1 });
            break;
          }
        }
      }
      return next;
    }
    case "tool_exec_output": {
      if (ev.execId === 0n) return blocks;
      const i = lastToolIndex(blocks, (b) => b.execId === ev.execId);
      if (i < 0) return blocks;
      const t = blocks[i] as Extract<Block, { kind: "tool" }>;
      return replaceAt(blocks, i, { ...t, output: t.output + ev.text + ev.detail });
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
        if (i >= 0) return replaceAt(blocks, i, end(blocks[i] as Extract<Block, { kind: "tool" }>));
      }
      // exec_end without a seen exec_start (replay anchor skipped it):
      // open a finished card rather than drop the completion.
      return [...blocks, end({ ...newToolBlock(ev), status: "pending" })];
    }
    case "server_tool_result":
      // ⤷ results: the payload rides input; one card per server tool.
      return [
        ...blocks,
        {
          ...newToolBlock(ev),
          status: ev.isError ? "failed" : "done",
          input: ev.inputJson,
        },
      ];
    case "queue_injected":
      // The parked line joined the run at a turn gap — render it as the
      // prompt it now is (TUI queueInjected), not a generic notice.
      return ev.text
        ? [...blocks, { kind: "user", text: ev.text }]
        : [...blocks, { kind: "notice", text: "queued prompt joined the run" }];
    case "background_end": {
      // A detached run_shell finished: status rides detail, the log tail
      // and path ride attrs so the outcome is readable without opening
      // the file (expandable in the notice's details).
      const task = ev.attrs["task"] || "?";
      const detail = [ev.attrs["log"], ev.attrs["tail"]]
        .filter(Boolean)
        .join("\n");
      const head = ev.isError
        ? `✗ background task #${task} — ${ev.detail}`
        : `⏳ background task #${task} — ${ev.detail}`;
      return [
        ...blocks,
        { kind: "notice", text: head, ...(detail ? { detail } : {}) },
      ];
    }
    case "memory_injected":
      return [
        ...blocks,
        { kind: "notice", text: ev.detail, detail: ev.attrs["memory"] },
      ];
    case "compaction":
      return ev.detail
        ? [...blocks, { kind: "notice", text: `compaction — ${ev.detail}` }]
        : blocks;
    case "subagent_start": {
      // Fan-out task opening (#53): its own card — the header of the
      // main↔sub exchange. A legacy single call has no SubID and draws
      // nothing here (its result rides the parent tool card).
      if (!ev.subId) return blocks;
      return [
        ...blocks,
        {
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
        },
      ];
    }
    case "subagent_end": {
      if (ev.subId) {
        for (let i = blocks.length - 1; i >= 0; i--) {
          const b = blocks[i];
          if (b.kind === "subtask" && b.subId === ev.subId) {
            return replaceAt(blocks, i, {
              ...b,
              status: ev.isError ? ("failed" as const) : ("done" as const),
              durationMs: Number(ev.durationMs),
              error: ev.isError ? ev.error : "",
              answer: ev.isError ? "" : ev.text || "",
              calls: ev.isError ? "" : ev.detail || "",
            });
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
      return [
        ...blocks,
        {
          kind: "notice",
          text: `🧠 subagent ${ev.toolName} ${usage} ${tail}`.trim(),
        },
      ];
    }
    case "files_changed": {
      // Server-side git delta (filecards.go), JSON rows in attrs.files —
      // malformed attrs skip the card, never the stream.
      let files: FileCard[] = [];
      try {
        const raw: unknown = JSON.parse(ev.attrs["files"] || "[]");
        if (Array.isArray(raw)) files = raw as FileCard[];
      } catch {
        return blocks;
      }
      return files.length ? [...blocks, { kind: "files", files }] : blocks;
    }
    case "error":
      return [...blocks, { kind: "error", text: ev.error || ev.detail }];
    case "prompt_end": {
      // The badge is the WHOLE run's wall time (time.Since(submit) on the
      // server), pinned when the run reaches its final state — submit to
      // final resolution, tool loop included. turn_end used to pin here,
      // but that carries one API call: a ten-minute run showed the last
      // leg (the "51.38s" report).
      // One stamp per reply: a text block already carrying a badge
      // belongs to an earlier run — a tool-only run must not re-time it.
      const ms = Number(ev.durationMs || 0);
      let out = blocks;
      if (ms > 0) {
        for (let i = blocks.length - 1; i >= 0; i--) {
          const b = blocks[i];
          if (b.kind === "text") {
            if (!b.turnMs) out = replaceAt(blocks, i, { ...b, turnMs: ms });
            break;
          }
        }
      }
      if (ev.error) {
        return [...out, { kind: "error", text: ev.error }];
      }
      // The output cap ended the reply mid-sentence: nothing failed, but
      // "done" over a truncated answer is a lie — say why it stops there.
      if (ev.stopReason === "max_tokens") {
        return [
          ...out,
          {
            kind: "notice",
            text:
              ev.detail ||
              "reply cut off at the output token cap — raise max_tokens to finish",
          },
        ];
      }
      return out;
    }
    case "turn_end": {
      // The call's output tokens land on its reasoning row: walk back to
      // the trailing thinking block and stamp the API's actual count over
      // the stream estimate (reasoning dominates output; a block already
      // stamped belongs to an earlier call — stop there).
      const out = ev.usage?.output;
      if (!out) return blocks;
      for (let i = blocks.length - 1; i >= 0; i--) {
        const b = blocks[i];
        if (b.kind !== "thinking") continue;
        if (b.tokens !== undefined) return blocks;
        return replaceAt(blocks, i, { ...b, tokens: Number(out) });
      }
      return blocks;
    }
    default:
      // message_start / subagent_start / unknown — nothing to draw (TUI
      // draws nothing for turn boundaries either).
      return blocks;
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
  // The chat-tail mindmap (§7.1): ONE revision-guarded snapshot, not a
  // Block — every distill update would otherwise append another card.
  // Null until GetMindmap or the first event lands; reset on switch.
  const [mindmap, setMindmap] = useState<MindmapSnap | null>(null);
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
    setBlocks([]);
    setErr("");
    setTodos([]);
    setMindmap(null);
    props.onCtx?.(0);
    props.onCost?.(undefined);
    // mindmap snapshot apply (§7.1): one revision-guarded state, never a
    // block. Stale revisions lose; equal rev is an idempotent re-delivery
    // (same content — skip, no re-render churn). mindmap_update carries
    // title/subtitle, mindmap_edit doesn't — empty keeps what's on hand.
    const applyMindmap = (
      rev: number,
      title: string | undefined,
      subtitle: string | undefined,
      nodesJson: string | undefined,
    ) => {
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
    const applyMindmapEv = (ev: SessionEvent) => {
      const upd = ev.typeName === "mindmap_update";
      if (!upd && ev.typeName !== "mindmap_edit") return;
      applyMindmap(
        Number(ev.attrs["revision"] || 0),
        upd ? ev.attrs["title"] : undefined,
        upd ? ev.attrs["subtitle"] : undefined,
        ev.attrs["nodes"],
      );
    };
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
      if (hist && hist.events.length > 0) {
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
          applyMindmapEv(ev); // reduce() ignores these types by design
          seeded = reduce(seeded, ev);
        }
        setBlocks(seeded);
        anchor = hist.lastSeq;
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
            { signal: controller.signal },
          );
          for await (const msg of stream) {
            if (!live) return;
            if (timer) {
              clearTimeout(timer);
              timer = null;
            }
            const ev = msg.event;
            if (!ev) continue;
            if (ev.typeName === "prompt_end") {
              // Undelivered parked lines were dropped by the session in the
              // same critical section that cleared busy (#2). Mirror the
              // TUI's kickQueued: re-send the head as the next prompt (one
              // per prompt_end, so the chain paces itself); the rest stay
              // parked. No head → the seat is genuinely idle again.
              const [head, ...rest] = queuedRef.current;
              if (head !== undefined) {
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
              } else {
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
            applyMindmapEv(ev); // reduce() ignores these types by design
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
    if (el && stickRef.current) el.scrollTo({ top: el.scrollHeight });
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
    try {
      if (busy) {
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

  const send = async () => {
    const prompt = input.trim();
    if (!prompt) return;
    setInput("");
    setErr("");
    // A slash token (with or without args) dispatches instead of sending:
    // "/rollback" alone or "/rollback anything" both reach execCommand.
    if (prompt.startsWith("/")) {
      await execCommand(prompt);
      return;
    }
    setHist((h) => [...h, prompt]); // ↑ browses what this seat actually sent
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

  // Fan-out ordinals: first-appearance order of each SubID across task
  // cards and inner steps — the chip number that ties a sub row to its
  // task ("sub 2" belongs to the card chipped 2).
  const subOrd = new Map<string, number>();
  for (const b of blocks) {
    const sid = b.kind === "subtask" ? b.subId : b.kind === "tool" ? b.subId : "";
    if (sid && !subOrd.has(sid)) subOrd.set(sid, subOrd.size + 1);
  }

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

  return (
    <div className={"chat" + (busy ? " streaming" : "")}>
      <div className="chat-feed" ref={feedRef} onScroll={onFeedScroll}>
        {blocks.map((b, i) => (
          <BlockView
            key={`${i}:${b.kind}`}
            block={b}
            thinkOpen={thinkOpen}
            ord={
              (b.kind === "subtask" || (b.kind === "tool" && b.subId))
                ? subOrd.get(b.kind === "subtask" ? b.subId : b.subId)
                : undefined
            }
            // The newest thinking block while the seat streams is the run
            // the reader came to watch: BlockView shows its body even under
            // a closed fold (#47) — but only until it has served its
            // purpose. Two gates: a long chain-of-thought self-folds
            // (LIVE_THINK_MAX_LINES), and once the run's conclusion text
            // lands after this row the thinking folds too (#40) — the
            // answer is what to read now. With no conclusion text yet the
            // row stays open, so there is always something to read.
            live={busy && i === lastThinking(blocks)}
            autoOpen={
              busy &&
              i === lastThinking(blocks) &&
              !textAfter(blocks, i)
            }
            // The duration clock only ticks on the feed's TRAILING block:
            // the moment a tool row lands after this thinking, the call is
            // over — a timer that keeps counting through run_shell made a
            // 656ms think look like a 4.77s one (#36).
            ticking={busy && i === blocks.length - 1}
            hinted={i === lastThinking(blocks)}
            onOpenFile={props.onOpenFile}
            onOpenDiff={props.onOpenDiff}
          />
        ))}
        {blocks.length === 0 && !mindmap && (
          <div className="placeholder">
            live events appear here — submit a prompt below
          </div>
        )}
        {mindmap && <MindMapCard snap={mindmap} onJump={jumpToTurn} />}
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
          {queued.map((q, i) => (
            <div className="queue-row" key={`${i}:${q}`}>
              <span className="queue-num">{i + 1}›</span>
              <span className="queue-text" title={q}>
                {oneLine(q)}
              </span>
            </div>
          ))}
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
}) {
  switch (block.kind) {
    case "user":
      return (
        <div className="blk-user">
          <div className="user-text">{block.text}</div>
        </div>
      );
    case "text":
      return (
        <div className="blk-text md">
          <Suspense fallback={<pre>{block.text}</pre>}>
            <MarkdownView>{block.text}</MarkdownView>
          </Suspense>
          {block.turnMs ? (
            <div className="turn-ms">{fmtDuration(block.turnMs)}</div>
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
          {block.output && <pre className="tool-output">{block.output}</pre>}
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
