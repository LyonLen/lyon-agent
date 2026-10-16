import { useEffect, useRef, useState } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import type { SessionEvent } from "../gen/lyon/v1/event_pb";
import { errText, sessionClient } from "../api";

// One rendered item on the transcript. The reducer below folds the event
// stream into these blocks — deltas append, boundaries open new ones.
type Block =
  | { kind: "text"; text: string }
  | { kind: "thinking"; text: string }
  | {
      kind: "tool";
      name: string;
      input: string;
      output: string;
      done: boolean;
      isError: boolean;
      error: string;
    }
  | { kind: "notice"; text: string }
  | { kind: "stats"; text: string }
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

// Fold one hub event into the block list. type_name is authoritative
// (forward compatible with types this build doesn't know).
function reduce(blocks: Block[], ev: SessionEvent): Block[] {
  switch (ev.typeName) {
    case "text_delta":
      return appendToLast(blocks, "text", ev.text);
    case "thinking_delta":
      return appendToLast(blocks, "thinking", ev.thinking);
    case "tool_use_start":
      return [
        ...blocks,
        {
          kind: "tool",
          name: ev.toolName || "tool",
          input: ev.inputJson,
          output: "",
          done: false,
          isError: ev.isError,
          error: ev.error,
        },
      ];
    case "tool_exec_output": {
      const last = blocks[blocks.length - 1];
      if (last && last.kind === "tool") {
        return [
          ...blocks.slice(0, -1),
          { ...last, output: last.output + ev.text + ev.detail },
        ];
      }
      return blocks;
    }
    case "tool_exec_end": {
      const last = blocks[blocks.length - 1];
      if (last && last.kind === "tool") {
        return [
          ...blocks.slice(0, -1),
          {
            ...last,
            done: true,
            isError: last.isError || ev.isError,
            error: last.error || ev.error,
          },
        ];
      }
      return blocks;
    }
    case "server_tool_result":
      return [
        ...blocks,
        {
          kind: "notice",
          text: `server tool ${ev.toolName} ${ev.isError ? "failed" : "returned"}`,
        },
      ];
    case "queue_injected":
      return [...blocks, { kind: "notice", text: "queued prompt joined the run" }];
    case "memory_injected":
    case "compaction":
    case "subagent_start":
    case "subagent_end":
      return ev.detail
        ? [...blocks, { kind: "notice", text: ev.detail }]
        : blocks;
    case "turn_end": {
      const u = ev.usage;
      const tokens = u
        ? `in=${u.input} out=${u.output} cache_read=${u.cacheRead}`
        : "";
      return [
        ...blocks,
        {
          kind: "stats",
          text: `turn ${ev.turn} · ${ev.stopReason} · ${ev.durationMs}ms ${tokens}`,
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
      return blocks;
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
  // aborts on unmount / session switch.
  useEffect(() => {
    const ac = new AbortController();
    let live = true;
    void (async () => {
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
    case "text":
      return <pre className="blk-text">{block.text}</pre>;
    case "thinking":
      return (
        <details className="blk-thinking">
          <summary>thinking</summary>
          <pre>{block.text}</pre>
        </details>
      );
    case "tool":
      return (
        <div className={"blk-tool" + (block.isError ? " failed" : "")}>
          <div className="blk-tool-head">
            <span className="tool-name">{block.name}</span>
            {block.done && <span className="tool-state">done</span>}
          </div>
          {block.input && <pre className="tool-input">{block.input}</pre>}
          {block.output && <pre className="tool-output">{block.output}</pre>}
          {block.error && <div className="err">{block.error}</div>}
        </div>
      );
    case "notice":
      return <div className="blk-notice">{block.text}</div>;
    case "stats":
      return <div className="blk-stats">{block.text}</div>;
    case "error":
      return <div className="err">{block.text}</div>;
  }
}
