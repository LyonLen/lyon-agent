import { lazy, Suspense, useEffect, useMemo, useRef, useState } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import type { SessionEvent } from "../gen/lyon/v1/event_pb";
import type { WatchPendingResponse } from "../gen/lyon/v1/approval_pb";
import { errText, sessionClient } from "../api";
import { parseTodos } from "../todos";
import type { TodoItem } from "../todos";
import type { StepRow } from "../types";
import { todoProgress } from "./PlanPane";

// The markdown parser rides its own chunk — Suspense fallback shows the
// raw text until it lands (Phase F split).
const MarkdownView = lazy(() => import("./Markdown"));

// The web slice of TUI's slashCommands table (cmd/lyon/tui.go): session/
// model/panel/misc categories, descriptions mirrored from the terminal
// build. The evolve/evolution rows are TUI-only surfaces (no web panel
// yet); unknown tokens fall through to App's runCommand and get a toast.
type SlashKind = "session" | "model" | "panel" | "misc";
const SLASH: Array<{ cmd: string; desc: string; kind: SlashKind }> = [
  { cmd: "/new", desc: "start a fresh session", kind: "session" },
  { cmd: "/resume", desc: "restore the newest saved session", kind: "session" },
  { cmd: "/rollback", desc: "undo to the newest checkpoint", kind: "session" },
  { cmd: "/sessions", desc: "list live + saved sessions", kind: "session" },
  { cmd: "/clear", desc: "clear the transcript view (history kept)", kind: "session" },
  { cmd: "/mode", desc: "cycle default → plan → bypass", kind: "model" },
  { cmd: "/model", desc: "show the session's model", kind: "model" },
  { cmd: "/think", desc: "expand/collapse thinking blocks", kind: "model" },
  { cmd: "/steps", desc: "open the action audit panel", kind: "panel" },
  { cmd: "/stats", desc: "usage totals — turns, tokens, cache", kind: "misc" },
  { cmd: "/help", desc: "list commands and keys", kind: "misc" },
];

// One rendered item on the transcript. The reducer below folds the event
// stream into these blocks — deltas append, boundaries open new ones,
// tool execs attach to their own card by id (parallel calls never
// cross-attach).
type Block =
  | { kind: "user"; text: string }
  | { kind: "text"; text: string }
  | { kind: "thinking"; text: string }
  | {
      kind: "tool";
      name: string;
      toolId: string;
      execId: bigint | null;
      input: string;
      output: string;
      status: "pending" | "running" | "done" | "failed" | "server";
      durationMs: number;
      error: string;
      server: boolean;
      sub: boolean;
    }
  | { kind: "notice"; text: string; detail?: string }
  | { kind: "error"; text: string };

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
    case "thinking_delta":
      return appendToLast(blocks, "thinking", ev.thinking);
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
        sub: base.sub || ev.sub,
      });
      if (i >= 0) return replaceAt(blocks, i, filled(blocks[i] as Extract<Block, { kind: "tool" }>));
      return [...blocks, filled({ ...newToolBlock(ev), status: "pending" })];
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
    case "memory_injected":
      return [
        ...blocks,
        { kind: "notice", text: ev.detail, detail: ev.attrs["memory"] },
      ];
    case "compaction":
      return ev.detail
        ? [...blocks, { kind: "notice", text: `compaction — ${ev.detail}` }]
        : blocks;
    case "subagent_end": {
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
    case "error":
      return [...blocks, { kind: "error", text: ev.error || ev.detail }];
    case "prompt_end":
      if (ev.error) {
        return [...blocks, { kind: "error", text: ev.error }];
      }
      return blocks;
    default:
      // message_start / turn_end / subagent_start / unknown — nothing to
      // draw (TUI draws nothing for turn boundaries either).
      return blocks;
  }
}

// Pretty-print tool input JSON when it is JSON; pass anything else through.
function prettyInput(raw: string): string {
  try {
    return JSON.stringify(JSON.parse(raw), null, 2);
  } catch {
    return raw;
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
  ) => void;
  onTodos: (t: TodoItem[]) => void;
  onSteps: (s: StepRow[]) => void;
  onCommand: (cmd: string) => void;
  reloadKey: number; // bump re-anchors the stream (rollback rewrote the ring)
  mode: string; // App owns the session record — badge follows its refresh
}) {
  const [blocks, setBlocks] = useState<Block[]>([]);
  const [busy, setBusy] = useState(false);
  const [input, setInput] = useState("");
  const [err, setErr] = useState("");
  const [apprSel, setApprSel] = useState(0);
  const feedRef = useRef<HTMLDivElement>(null);
  // Phase D: plan corner / status line / slash panel local state.
  const [todos, setTodos] = useState<TodoItem[]>([]);
  const [ctxTokens, setCtxTokens] = useState(0); // last turn's prompt load
  const [model, setModel] = useState(""); // "" = engine default
  const [thinkOpen, setThinkOpen] = useState(false);
  const [cmdNote, setCmdNote] = useState(""); // local command receipt, 3s
  const [slashSel, setSlashSel] = useState(0);
  const [hist, setHist] = useState<string[]>([]);
  const [histIdx, setHistIdx] = useState(-1); // -1 = editing a fresh line
  const noteTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

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
  // contains "y" must not approve anything.
  useEffect(() => {
    if (pendings.length === 0) return;
    const answer = (approve: boolean, remember: boolean) => {
      const p = pendings[sel];
      if (p) props.onDecide(p.sessionId, p.execId, approve, remember);
    };
    const onKey = (e: KeyboardEvent) => {
      const ae = document.activeElement;
      if (ae && (ae.tagName === "TEXTAREA" || ae.tagName === "INPUT")) return;
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
          answer(true, false);
          break;
        case "n":
          answer(false, false);
          break;
        case "a":
          answer(true, true);
          break;
        case "Enter":
          e.preventDefault();
          answer(true, false);
          break;
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [pendings, sel, props]);

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
    setCtxTokens(0);
    void (async () => {
      // Both reads are unary (content-length) responses — they arrive
      // even while a streaming socket would sit unread. Parallel:
      // independent RPCs, and each failure degrades on its own (no
      // history → the stream's full replay below; no record → idle).
      const [hist, sres] = await Promise.all([
        sessionClient.getHistory({ sessionId: props.sessionId }).catch(() => null),
        sessionClient.getSession({ sessionId: props.sessionId }).catch(() => null),
      ]);
      if (!live) return;
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
          if (ev.typeName === "turn_end" && ev.usage) {
            const u = ev.usage;
            setCtxTokens(Number(u.input + u.cacheRead + u.cacheCreation));
          }
          seeded = reduce(seeded, ev);
        }
        setBlocks(seeded);
        anchor = hist.lastSeq;
      }
      if (sres?.session) {
        setBusy(sres.session.busy);
        setModel(sres.session.model);
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
              setBusy(false);
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
            // turn_end carries the turn's usage — the TUI ctx gauge reads
            // prompt load (input + cache read + cache creation).
            if (ev.typeName === "turn_end" && ev.usage) {
              const u = ev.usage;
              setCtxTokens(Number(u.input + u.cacheRead + u.cacheCreation));
            }
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
  // command with args falls through to send()'s dispatcher.
  const slashOpen = input.startsWith("/") && !/\s/.test(input);
  const slashMatches = useMemo(() => {
    if (!slashOpen) return [];
    const q = input.trim().toLowerCase();
    return SLASH.filter((c) => c.cmd.startsWith(q));
  }, [slashOpen, input]);
  const ssel = Math.min(slashSel, Math.max(0, slashMatches.length - 1));
  useEffect(() => setSlashSel(0), [input]);

  useEffect(() => {
    feedRef.current?.scrollTo({ top: feedRef.current.scrollHeight });
  }, [blocks]);

  // note prints a local command receipt into the status line (3s, the
  // TUI status toast slot — App's flash() covers cross-component ones).
  const note = (m: string) => {
    setCmdNote(m);
    if (noteTimer.current) clearTimeout(noteTimer.current);
    noteTimer.current = setTimeout(() => setCmdNote(""), 3000);
  };

  // execCommand runs one slash token: the local rows ChatView owns, the
  // cross-component ones routed up to App (/new /resume /rollback …).
  const execCommand = async (raw: string) => {
    const cmd = raw.trim().split(/\s+/)[0];
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
      case "/model":
        note(`model: ${model || "engine default"}`);
        break;
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
          note(
            `stats: ${res.turns.length} turns · in=${inT} out=${outT} cache_r=${cr} cache_w=${cw}`,
          );
        } catch (e) {
          note(errText(e));
        }
        break;
      }
      case "/help":
        note(`${SLASH.map((c) => c.cmd).join(" ")} · shift+tab mode · ↑ history`);
        break;
      default:
        void props.onCommand(cmd);
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
    try {
      if (busy) {
        const res = await sessionClient.enqueuePrompt({
          sessionId: props.sessionId,
          prompt,
        });
        setBusy(true);
        setBlocks((b) => [
          ...b,
          { kind: "notice", text: `queued (position ${res.queueLen})` },
        ]);
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

  const interrupt = async () => {
    try {
      await sessionClient.interrupt({ sessionId: props.sessionId });
    } catch (e) {
      setErr(errText(e));
    }
  };

  const setMode = async (mode: string) => {
    try {
      await sessionClient.setMode({ sessionId: props.sessionId, mode });
      props.onChanged();
    } catch (e) {
      setErr(errText(e));
    }
  };

  return (
    <div className={"chat" + (busy ? " streaming" : "")}>
      <div className="chat-feed" ref={feedRef}>
        {blocks.map((b, i) => (
          <BlockView key={`${i}:${b.kind}`} block={b} thinkOpen={thinkOpen} />
        ))}
        {blocks.length === 0 && (
          <div className="placeholder">
            live events appear here — submit a prompt below
          </div>
        )}
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
          {pendings.map((p, i) => (
            <div
              key={`${p.sessionId}:${p.execId}`}
              className={"approval-card" + (i === sel ? " active" : "")}
            >
              <div className="appr-head">
                <span className="appr-title">
                  <span className="appr-q">approve?</span>
                  <span className="tool-name">{p.toolName}</span>
                </span>
                {i === sel && (
                  <span className="appr-hint">
                    y approve · n deny · a always · ↑↓ switch
                  </span>
                )}
              </div>
              {p.inputJson && (
                <pre className="appr-input">{prettyInput(p.inputJson)}</pre>
              )}
              <div className="appr-actions">
                <button
                  onClick={() => props.onDecide(p.sessionId, p.execId, true, false)}
                >
                  approve
                </button>
                <button
                  className="ghost"
                  onClick={() => props.onDecide(p.sessionId, p.execId, false, false)}
                >
                  deny
                </button>
                <button
                  className="ghost"
                  onClick={() => props.onDecide(p.sessionId, p.execId, true, true)}
                >
                  always
                </button>
              </div>
            </div>
          ))}
        </div>
      )}
      {slashOpen && slashMatches.length > 0 && (
        <div className="slash-panel">
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
      {busy && input.trim() && (
        <div className="queue-preview">
          <span className="queue-caret">⏎</span>
          <span className="queue-text">{input.trim().slice(0, 120)}</span>
          <span className="queue-hint">Enter queues for the next turn gap</span>
        </div>
      )}
      <div className="status-line">
        <span className={`mode-badge ${props.mode}`}>{props.mode}</span>
        {todos.length > 0 && (
          <span className="sl-plan">
            {(() => {
              const p = todoProgress(todos);
              return `📋 ${p.done}/${p.total}${p.current ? ` ▸ ${p.current}` : ""}`;
            })()}
          </span>
        )}
        {ctxTokens > 0 && <span className="sl-ctx">ctx {ctxTokens} tok</span>}
        <span className="sl-fill" />
        {cmdNote ? (
          <span className="sl-note">{cmdNote}</span>
        ) : pendings.length > 0 ? (
          <span className="sl-appr">approval pending — y approve · n deny · a always</span>
        ) : busy ? (
          <span className="sl-hint">
            <span className="spin" aria-hidden="true" />
            running · interrupt to stop · Enter queues
          </span>
        ) : (
          <span className="sl-legend">/ for commands · shift+tab mode · ↑ history</span>
        )}
      </div>
      <div className="chat-controls">
        <button className="ghost" onClick={() => void setMode("plan")}>
          plan
        </button>
        <button className="ghost" onClick={() => void setMode("default")}>
          default
        </button>
        {busy && (
          <button className="ghost danger" onClick={() => void interrupt()}>
            interrupt
          </button>
        )}
      </div>
      <div className="chat-input">
        <textarea
          value={input}
          placeholder={busy ? "busy — Enter queues for the next turn gap" : "prompt (Enter to send) · / for commands"}
          onChange={(e) => setInput(e.target.value)}
          onKeyDown={(e) => {
            // Slash panel first: it is open only while the input is one
            // bare token, so these keys never fight prose editing.
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

function BlockView({ block, thinkOpen }: { block: Block; thinkOpen: boolean }) {
  switch (block.kind) {
    case "user":
      return (
        <div className="blk-user">
          <span className="user-caret">❯</span>
          <div className="user-text">{block.text}</div>
        </div>
      );
    case "text":
      return (
        <div className="blk-text md">
          <Suspense fallback={<pre>{block.text}</pre>}>
            <MarkdownView>{block.text}</MarkdownView>
          </Suspense>
        </div>
      );
    case "thinking":
      return (
        // key remounts on toggle so <details> picks up the new open
        // state — /think flips every thinking block at once.
        <details key={String(thinkOpen)} className="blk-thinking" open={thinkOpen}>
          <summary>thinking</summary>
          <pre>{block.text}</pre>
        </details>
      );
    case "tool":
      return (
        <details
          className={"blk-tool" + (block.status === "failed" ? " failed" : "")}
        >
          <summary className="blk-tool-head">
            <span className="tool-title">
              <span className="tool-chevron" aria-hidden="true">
                ▸
              </span>
              <span className="tool-name">{block.name}</span>
              {block.sub && <span className="tool-badge">sub</span>}
              {block.server && <span className="tool-badge server">server</span>}
            </span>
            <span className={`tool-status ${block.status}`}>
              {block.status === "running" && <span className="spin" aria-hidden="true" />}
              {block.status === "pending" && "…"}
              {block.status === "running" && "running"}
              {/* Replayed history has no timing — show the marker bare
                  instead of a misleading "0ms". */}
              {block.status === "done" &&
                (block.durationMs ? `✓ ${block.durationMs}ms` : "✓")}
              {block.status === "failed" &&
                (block.durationMs ? `✗ ${block.durationMs}ms` : "✗")}
              {block.status === "server" && "🌐 server"}
            </span>
          </summary>
          {block.input && <pre className="tool-input">{prettyInput(block.input)}</pre>}
          {block.output && <pre className="tool-output">{block.output}</pre>}
          {block.error && <div className="err">{block.error}</div>}
        </details>
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
