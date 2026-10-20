import { useEffect, useRef } from "react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { getToken } from "../api";

// Web terminal pane (#26): xterm.js over the /terminal WebSocket. The
// WS session id IS the chat session id — every chat session owns one
// shell, so switching sessions switches terminals and parallel tasks
// never share (or clobber) a prompt. The server keeps the shell while
// nobody is attached (256KB output ring) and replays the tail on
// reconnect, so a refresh, a pane close, or a hop to another session
// never costs the shell; only 30min idle reaping or lyond exiting ends
// it. Mirrors SCC's Terminal.tsx: fit on tile resize, exponential
// reconnect backoff 1s→30s, app-level ping (no WS ping API in browsers).
export default function TerminalPane({ sessionId }: { sessionId: string }) {
  const hostRef = useRef<HTMLDivElement>(null);

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

    const connect = () => {
      if (closed || fatal) return;
      const proto = location.protocol === "https:" ? "wss" : "ws";
      ws = new WebSocket(
        `${proto}://${location.host}/terminal?session=${encodeURIComponent(sessionId)}&token=${encodeURIComponent(getToken())}`,
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
            // A dead shell respawns on reattach (SCC/tmux semantics) —
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
        // Exponential backoff 1s→30s (SCC): a lyond restart heals itself.
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
      if (ping !== undefined) clearInterval(ping);
      ro.disconnect();
      // Deliberately NO close frame: the shell belongs to the chat
      // session, not the pane. Switching sessions or closing the pane
      // just drops the socket — the server holds the shell and replays
      // the ring on return; idle reaping collects it for good.
      ws?.close();
      term.dispose();
    };
  }, [sessionId]);

  if (!sessionId) {
    return (
      <div className="placeholder">select a session — its shell lives here</div>
    );
  }
  return <div className="term-host" ref={hostRef} />;
}
