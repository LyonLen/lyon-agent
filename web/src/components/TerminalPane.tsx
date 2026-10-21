import { useEffect, useRef, useState } from "react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { getToken } from "../api";

// Quick-command strip under the viewport (#35): click a chip and the text
// (+ Enter) rides the WS stdin frames. User-extensible — "+" appends a
// command, hover "×" removes one — stored per browser, not per session.
const SNIPS_KEY = "lyon_term_snippets_v1";
const DEFAULT_SNIPS = ["git status", "git log --oneline -5", "ls -la", "pwd"];

function loadSnips(): string[] {
  try {
    const raw = localStorage.getItem(SNIPS_KEY);
    if (raw) {
      const v: unknown = JSON.parse(raw);
      if (Array.isArray(v) && v.every((s) => typeof s === "string")) return v;
    }
  } catch {
    // corrupt storage falls back to the defaults below
  }
  return [...DEFAULT_SNIPS];
}

// Web terminal pane (#26): xterm.js over the /terminal WebSocket. One
// shell per (chat session, pane): the WS keys the server hub as
// sid~leafId, so a session may keep several terminal panes open at once
// (each with its own shell) while hopping chat sessions still swaps in
// that session's shells — parallel tasks never share a prompt. The
// server keeps each shell while nobody is attached (256KB output ring)
// and replays the tail on reconnect, so a refresh, a pane close, or a
// hop to another session never costs the shell; only 30min idle
// reaping, deleting the chat session, or lyond exiting ends it. Mirrors
// a sibling product's Terminal.tsx: fit on tile resize, exponential reconnect backoff
// 1s→30s, app-level ping (no WS ping API in browsers).
export default function TerminalPane({
  sessionId,
  paneId,
}: {
  sessionId: string;
  paneId: string;
}) {
  const hostRef = useRef<HTMLDivElement>(null);
  // The chip strip lives outside the xterm effect; it writes stdin
  // through here (null while no socket exists — a click is a no-op).
  const sendRef = useRef<((data: string) => void) | null>(null);
  const [snips, setSnips] = useState(loadSnips);
  const [adding, setAdding] = useState(false);
  const [draft, setDraft] = useState("");

  const persistSnips = (next: string[]) => {
    setSnips(next);
    try {
      localStorage.setItem(SNIPS_KEY, JSON.stringify(next));
    } catch {
      // private-mode storage failure: kept for this mount only
    }
  };

  useEffect(() => {
    if (!sessionId) return; // no chat session selected yet

    // Concrete colors from the lyon tokens (index.css) — xterm reads JS
    // values, not CSS variables.
    const term = new Terminal({
      fontSize: 12,
      fontFamily: "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace",
      cursorBlink: true,
      scrollback: 5000,
      theme: {
        background: "#101418",
        foreground: "#d7dee8",
        cursor: "#4da3ff",
        cursorAccent: "#101418",
        selectionBackground: "#1c2735",
      },
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(hostRef.current!);

    let ws: WebSocket | null = null;
    let closed = false; // final teardown — never reconnect after this
    let fatal = false; // superseded or refused — retrying is pointless
    let attempt = 0;
    let ping: number | undefined;

    const sendCtl = (v: unknown) => {
      if (ws?.readyState === WebSocket.OPEN) ws.send(JSON.stringify(v));
    };
    const sendResize = () =>
      sendCtl({ type: "resize", cols: term.cols, rows: term.rows });

    // Quick-command chips: same binary stdin frames as typed keys.
    // Closures over `ws` see the current socket (let rebinding in connect).
    sendRef.current = (data: string) => {
      if (ws?.readyState === WebSocket.OPEN) {
        ws.send(new TextEncoder().encode(data));
      }
    };

    const connect = () => {
      if (closed || fatal) return;
      const proto = location.protocol === "https:" ? "wss" : "ws";
      ws = new WebSocket(
        `${proto}://${location.host}/terminal?session=${encodeURIComponent(sessionId)}&pane=${encodeURIComponent(paneId)}&token=${encodeURIComponent(getToken())}`,
      );
      ws.binaryType = "arraybuffer";
      ws.onopen = () => {
        attempt = 0;
        sendResize();
        ping = window.setInterval(() => sendCtl({ type: "ping" }), 25_000);
      };
      ws.onmessage = (ev) => {
        if (typeof ev.data === "string") {
          let note: { type?: string; message?: string } = {};
          try {
            note = JSON.parse(ev.data);
          } catch {
            return;
          }
          if (note.type === "exited") {
            // A dead shell respawns on reattach (tmux semantics) —
            // closing this socket makes the reconnect build a fresh one.
            term.write(
              "\r\n\x1b[2m[shell exited — starting a new one]\x1b[0m\r\n",
            );
            ws?.close();
          } else if (note.type === "error") {
            fatal = true;
            term.write(
              `\r\n\x1b[31m[terminal: ${note.message ?? "error"}]\x1b[0m\r\n`,
            );
          }
          return;
        }
        term.write(new Uint8Array(ev.data as ArrayBuffer));
      };
      ws.onclose = (ev) => {
        if (ping !== undefined) {
          clearInterval(ping);
          ping = undefined;
        }
        if (closed || fatal) return;
        if (ev.reason === "replaced") {
          // Another pane already holds this session's terminal (the
          // server allows one attach per session). Reconnecting would
          // just ping-pong supersessions forever.
          fatal = true;
          term.write(
            "\r\n\x1b[2m[this session's terminal is open in another pane]\x1b[0m\r\n",
          );
          return;
        }
        // Exponential backoff 1s→30s (sibling-product cadence): a lyond restart heals itself.
        attempt += 1;
        const delay = Math.min(30_000, 1000 * 2 ** Math.min(attempt, 5));
        term.write(
          `\r\n\x1b[2m[disconnected — retrying in ${Math.round(delay / 1000)}s]\x1b[0m\r\n`,
        );
        window.setTimeout(connect, delay);
      };
      ws.onerror = () => ws?.close(); // onclose schedules the retry
    };

    // stdin: xterm data (keys, paste) → binary frames.
    term.onData((data) => {
      if (ws?.readyState === WebSocket.OPEN) {
        ws.send(new TextEncoder().encode(data));
      }
    });
    term.onResize(sendResize);

    // Fit whenever the TILE resizes — separator drag, split, maximize.
    const ro = new ResizeObserver(() => {
      try {
        fit.fit();
      } catch {
        // zero-size mid-layout; the next observe fires with real geometry
      }
    });
    ro.observe(hostRef.current!);
    try {
      fit.fit();
    } catch {
      // zero-size on first mount is fine — the observer follows
    }

    connect();

    return () => {
      closed = true;
      sendRef.current = null;
      if (ping !== undefined) clearInterval(ping);
      ro.disconnect();
      // Deliberately NO close frame: the shell belongs to the chat
      // session, not the pane. Switching sessions or closing the pane
      // just drops the socket — the server holds the shell and replays
      // the ring on return; idle reaping collects it for good.
      ws?.close();
      term.dispose();
    };
  }, [sessionId, paneId]);

  if (!sessionId) {
    return (
      <div className="placeholder">select a session — its shell lives here</div>
    );
  }
  const run = (cmd: string) => sendRef.current?.(cmd + "\n");
  return (
    <>
      <div className="term-host" ref={hostRef} />
      <div className="term-snips">
        {snips.map((s, i) => (
          <div
            key={`${i}-${s}`}
            className="term-snip"
            title={s}
            onClick={() => run(s)}
          >
            <code>{s}</code>
            <span
              className="snip-x"
              title="移除"
              onClick={(e) => {
                e.stopPropagation();
                persistSnips(snips.filter((_, j) => j !== i));
              }}
            >
              ×
            </span>
          </div>
        ))}
        {adding ? (
          <input
            className="snip-input"
            autoFocus
            value={draft}
            placeholder="命令 + Enter"
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={(e) => {
              e.stopPropagation();
              if (e.key === "Escape") {
                setDraft("");
                setAdding(false);
              } else if (e.key === "Enter") {
                const v = draft.trim();
                if (v) persistSnips([...snips, v]);
                setDraft("");
                setAdding(false);
              }
            }}
          />
        ) : (
          <button
            className="term-snip add"
            title="添加快捷指令"
            onClick={() => setAdding(true)}
          >
            +
          </button>
        )}
      </div>
    </>
  );
}
