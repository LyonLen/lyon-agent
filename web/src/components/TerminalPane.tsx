import { useEffect, useRef } from "react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { getToken } from "../api";

// Web terminal pane (#26): xterm.js over the /terminal WebSocket. The
// pane's stable nodeId IS the server-side session id, so tiling layout
// persistence doubles as terminal persistence — detach keeps the shell
// alive and the server replays its 256KB output ring on reconnect.
// Mirrors SCC's Terminal.tsx: fit on tile resize, exponential reconnect
// backoff 1s→30s, app-level ping (browsers have no WS ping API).
//
// Unmount means the pane was closed, so the cleanup sends an explicit
// close frame — that kills the shell. A page refresh never reaches the
// cleanup (the socket just drops), which is exactly what keeps the shell
// across a refresh.
export default function TerminalPane({ leafId }: { leafId: string }) {
  const hostRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const host = hostRef.current;
    if (!host) return;

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
    term.open(host);

    let ws: WebSocket | null = null;
    let closed = false; // final teardown — never reconnect after this
    let fatal = false; // server refused (auth/limit) — retrying is pointless
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
        `${proto}://${location.host}/terminal?session=${encodeURIComponent(leafId)}&token=${encodeURIComponent(getToken())}`,
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
            // SCC/tmux semantics: a dead shell respawns on reattach —
            // close this socket and the reconnect rebuilds a fresh one.
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
      ws.onclose = () => {
        if (ping !== undefined) {
          clearInterval(ping);
          ping = undefined;
        }
        if (closed || fatal) return;
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
    ro.observe(host);
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
      sendCtl({ type: "close" }); // pane closed → kill the server shell
      ws?.close();
      term.dispose();
    };
  }, [leafId]);

  return <div className="term-host" ref={hostRef} />;
}
