import { useEffect, useRef, useState } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import type { Components } from "react-markdown";
import type { SessionEvent } from "../gen/lyon/v1/event_pb";
import { errText, sessionClient } from "../api";

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

// Markdown for assistant prose: headings/lists/tables/strikethrough via
// remark-gfm; fenced code stays a plain pre — deliberately no syntax
// highlighter (portal precedent, plan Phase C).
const mdComponents: Components = {
  code({ className, children }) {
    const text = String(children ?? "");
    const match = /language-([\w-]+)/.exec(className || "");
    if (!match) return <code className="md-inline">{text}</code>;
    return <code className={`md-fence ${className}`}>{text}</code>;
  },
  a({ href, children }) {
    return (
      <a href={href} target="_blank" rel="noreferrer">
        {children}
      </a>
    );
  },
};

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
}) {
  const [blocks, setBlocks] = useState<Block[]>([]);
  const [busy, setBusy] = useState(false);
  const [input, setInput] = useState("");
  const [err, setErr] = useState("");
  const feedRef = useRef<HTMLDivElement>(null);

  // One WatchEvents stream per selected session: from_seq=-1 replays the
  // hub ring (whatever the previous prompt left) then goes live. Cleanup
  // aborts on unmount / session switch. Busy seeds from the session
  // record so opening a mid-run session doesn't render it idle.
  useEffect(() => {
    const ac = new AbortController();
    let live = true;
    setBlocks([]);
    setErr("");
    void (async () => {
      try {
        const res = await sessionClient.getSession({ sessionId: props.sessionId });
        if (live && res.session) setBusy(res.session.busy);
      } catch {
        // The stream below surfaces auth/connect problems; busy just
        // falls back to idle until then.
      }
      try {
        const stream = sessionClient.watchEvents(
          { sessionId: props.sessionId, fromSeq: -1n },
          { signal: ac.signal },
        );
        for await (const msg of stream) {
          if (!live) return;
          const ev = msg.event;
          if (!ev) continue;
          if (ev.typeName === "prompt_end") {
            setBusy(false);
          }
          setBlocks((b) => reduce(b, ev));
        }
      } catch (e) {
        const ce = ConnectError.from(e);
        if (live && ce.code !== Code.Canceled) {
          setErr(errText(e));
        }
      }
    })();
    return () => {
      live = false;
      ac.abort();
    };
  }, [props.sessionId]);

  useEffect(() => {
    feedRef.current?.scrollTo({ top: feedRef.current.scrollHeight });
  }, [blocks]);

  const send = async () => {
    const prompt = input.trim();
    if (!prompt) return;
    setInput("");
    setErr("");
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
    <div className="chat">
      <div className="chat-feed" ref={feedRef}>
        {blocks.map((b, i) => (
          <BlockView key={i} block={b} />
        ))}
        {blocks.length === 0 && (
          <div className="placeholder">
            live events appear here — submit a prompt below
          </div>
        )}
      </div>
      {err && <div className="err chat-err">{err}</div>}
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
          placeholder={busy ? "busy — Enter queues for the next turn gap" : "prompt (Enter to send)"}
          onChange={(e) => setInput(e.target.value)}
          onKeyDown={(e) => {
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

function BlockView({ block }: { block: Block }) {
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
          <ReactMarkdown remarkPlugins={[remarkGfm]} components={mdComponents}>
            {block.text}
          </ReactMarkdown>
        </div>
      );
    case "thinking":
      return (
        <details className="blk-thinking">
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
