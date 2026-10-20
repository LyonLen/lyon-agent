import { Suspense, lazy, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import type { Session } from "./gen/lyon/v1/session_pb";
import type { SnapshotRef } from "./gen/lyon/v1/dashboard_pb";
import type { WatchPendingResponse } from "./gen/lyon/v1/approval_pb";
import { approvalClient, dashboardClient, errText, fileClient, getToken, sessionClient, setToken } from "./api";
import type { FileKind } from "./gen/lyon/v1/file_pb";
import SessionList from "./components/SessionList";
import ChatView from "./components/ChatView";
import PlanPane from "./components/PlanPane";
import StepsPane from "./components/StepsPane";
import FilePanel from "./components/FilePanel";
import PreviewPane from "./components/PreviewPane";
import ErrorBoundary from "./components/ErrorBoundary";
import TilingWorkspace, { type PaneDef } from "./tiling/TilingWorkspace";
import { useWorkspaceLayout } from "./tiling/useWorkspaceLayout";
import { collectLeaves } from "./tiling/types";
import { useFileTabs } from "./useFileTabs";

// Dashboard is a separate route-level view — lazy so the chat pane's
// first paint doesn't pay for replay/audit rendering (Phase F chunking).
const DashboardPanel = lazy(() => import("./components/DashboardPanel"));
import type { TodoItem } from "./todos";
import type { StepRow } from "./types";

export default function App() {
  const [tokenInput, setTokenInput] = useState(getToken());
  const [sessions, setSessions] = useState<Session[]>([]);
  const [saved, setSaved] = useState<SnapshotRef[]>([]);
  const [restoring, setRestoring] = useState("");
  const [selectedId, setSelectedId] = useState("");
  const [listErr, setListErr] = useState("");
  const [view, setView] = useState<"chat" | "dashboard">("chat");
  // Approval watch (Phase B): one process-wide WatchPending stream feeds
  // this list; ChatView renders the slice for the selected seat.
  const [pending, setPending] = useState<WatchPendingResponse[]>([]);
  const [toast, setToast] = useState("");
  // Phase A (tiling): plan/steps/files render as workspace panes fed by
  // ChatView's reducer; reloadKey forces the transcript stream to
  // re-anchor after a rollback rewrote the ring.
  const [todos, setTodos] = useState<TodoItem[]>([]);
  const [steps, setSteps] = useState<StepRow[]>([]);
  const [reloadKey, setReloadKey] = useState(0);
  // Bottom-left mode dock: click expands the three-mode picker; the chip
  // mirrors the selected session's mode (falls back to "default").
  const [modeMenu, setModeMenu] = useState(false);
  const modeDockRef = useRef<HTMLDivElement>(null);
  const ws = useWorkspaceLayout();
  // Preview tab stacks ride the tile nodeIds — drag/swap keeps a pane's
  // tabs, closing one prunes its record (plan Phase B).
  const fileTabs = useFileTabs(ws.root);
  // External directory navigation for the files pane (markdown links,
  // slash routing): seq bumps so re-requesting the same path reloads.
  const [filesNav, setFilesNav] = useState<{ path: string; seq: number } | null>(null);
  const filesNavSeq = useRef(0);
  // Refs the stream loop reads without re-subscribing on every change:
  // selectedId for toast suppression, pending for new-vs-replay detection.
  const selectedRef = useRef(selectedId);
  const pendingRef = useRef(pending);
  const toastTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(() => {
    selectedRef.current = selectedId;
  }, [selectedId]);
  useEffect(() => {
    pendingRef.current = pending;
  }, [pending]);

  // flash is the App-wide 3s toast (approval events and slash commands
  // share it); the timer resets so a newer message always gets its slot.
  const flash = (msg: string) => {
    setToast(msg);
    if (toastTimer.current) clearTimeout(toastTimer.current);
    toastTimer.current = setTimeout(() => setToast(""), 3000);
  };

  // Live sessions and the saved snapshot store are independent reads —
  // fetch both concurrently, each with its own error so one failing
  // channel doesn't blank the other.
  const refresh = useCallback(async () => {
    const [live, arch] = await Promise.allSettled([
      sessionClient.listSessions({}),
      dashboardClient.listSnapshots({}),
    ]);
    if (live.status === "fulfilled") {
      const list = [...live.value.sessions].reverse(); // oldest first; newest active sits at the bottom near the chat
      setSessions(list);
      setListErr("");
      // A selected seat that fell out of the live list was reaped or
      // deleted under us (the "click a ghost" hole): clear the selection
      // instead of parking the chat on not_found. Create/restore never
      // trip this — their session is already in the store when the id
      // becomes selected.
      const sel = selectedRef.current;
      if (sel && !list.some((s) => s.id === sel)) {
        setSelectedId("");
        flash("session gone from the list (reaped?) — list refreshed");
      }
    } else {
      setListErr(errText(live.reason));
    }
    if (arch.status === "fulfilled") {
      setSaved(arch.value.snapshots);
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const sessionsRef = useRef(sessions);
  useEffect(() => {
    sessionsRef.current = sessions;
  }, [sessions]);

  // Memoized: ChatView's key handler depends on the slice identity, and a
  // fresh array every render would rebind it on every transcript delta.
  const seatPending = useMemo(
    () => pending.filter((p) => p.sessionId === selectedId),
    [pending, selectedId],
  );

  // One process-wide approval stream, self-healing: transient stream
  // failures reopen after 3s (a dropped connection must not strand the
  // parked cards); Unimplemented means lyond runs without -approve —
  // there is nothing to watch, so stop instead of looping forever.
  useEffect(() => {
    let live = true;
    const sleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));
    void (async () => {
      while (live) {
        try {
          const stream = approvalClient.watchPending({});
          for await (const ev of stream) {
            if (!live) return;
            if (ev.toolName === "") {
              // Resolve (approval.proto): decided, interrupted, or reaped.
              // The all-empty open marker (exec 0) is a header flush —
              // filtering matches nothing, skip the state churn.
              if (ev.execId !== 0n) {
                setPending((prev) =>
                  prev.filter(
                    (p) => !(p.sessionId === ev.sessionId && p.execId === ev.execId),
                  ),
                );
              }
              continue;
            }
            const same = (p: WatchPendingResponse) =>
              p.sessionId === ev.sessionId && p.execId === ev.execId;
            const isNew = !pendingRef.current.some(same);
            if (isNew && ev.sessionId !== selectedRef.current) {
              const title =
                sessionsRef.current.find((s) => s.id === ev.sessionId)?.title ??
                ev.sessionId.slice(0, 8);
              setToast(`${ev.toolName} needs approval — ${title}`);
              if (toastTimer.current) clearTimeout(toastTimer.current);
              toastTimer.current = setTimeout(() => setToast(""), 3000);
            }
            setPending((prev) =>
              prev.some(same) ? prev.map((p) => (same(p) ? ev : p)) : [...prev, ev],
            );
          }
          // Clean server close (slow-kick) — reopen and re-snapshot.
        } catch (e) {
          if (!live) return;
          const code = ConnectError.from(e).code;
          if (code === Code.Unimplemented || code === Code.Canceled) return;
        }
        await sleep(3000);
      }
    })();
    return () => {
      live = false;
    };
  }, []);

  // decide answers a parked approval. The resolve event also removes the
  // card (idempotent); this local removal just makes the click feel
  // instant, and NotFound means someone else — or an interrupt — beat us
  // to it, which is equally a reason to drop the card.
  const decide = async (
    sessionId: string,
    execId: bigint,
    approve: boolean,
    remember: boolean,
  ) => {
    try {
      await approvalClient.decide({ sessionId, execId, approve, remember });
    } catch (e) {
      const code = ConnectError.from(e).code;
      if (code !== Code.NotFound) {
        setListErr(errText(e));
        return;
      }
    }
    setPending((prev) =>
      prev.filter((p) => !(p.sessionId === sessionId && p.execId === execId)),
    );
  };

  const create = async () => {
    try {
      const res = await sessionClient.createSession({});
      setSelectedId(res.session?.id ?? "");
      await refresh();
    } catch (err) {
      setListErr(errText(err));
    }
  };

  // Restore a saved snapshot into the live store; the new session id
  // becomes the selection and ChatView's GetHistory batch draws the
  // whole history (plan Phase A — the "点不进来" fix).
  const restore = async (path: string) => {
    if (restoring) return;
    setRestoring(path);
    setListErr("");
    try {
      const res = await sessionClient.restoreSession({ path });
      if (res.session) setSelectedId(res.session.id);
      await refresh();
    } catch (err) {
      setListErr(errText(err));
    } finally {
      setRestoring("");
    }
  };

  const onTokenCommit = () => {
    setToken(tokenInput.trim());
    void refresh();
  };

  // rollback rewinds the seat to its newest checkpoint: the server
  // rewrites the ring, then the transcript stream re-anchors with -1 and
  // replays the rewound conversation (TUI /rollback parity).
  const rollback = async () => {
    if (!selectedId) return;
    try {
      const res = await sessionClient.rollbackSession({
        sessionId: selectedId,
        id: 0, // int32 — 0 picks the newest checkpoint
      });
      const cp = res.checkpoint;
      flash(`rolled back ${cp ? `— “${cp.label}” undone (${cp.messages} msgs restored)` : ""}`);
      setReloadKey((k) => k + 1);
    } catch (e) {
      flash(errText(e));
    }
  };

  // pickMode sets an explicit mode (the bottom-left dock menu); cycleMode
  // walks default → plan → bypass → default on top of it (TUI shift+tab).
  const pickMode = async (mode: string) => {
    if (!selectedId) {
      flash("select a session first");
      return;
    }
    try {
      await sessionClient.setMode({ sessionId: selectedId, mode });
      flash(`mode → ${mode}`);
      await refresh();
    } catch (e) {
      flash(errText(e));
    }
  };

  const cycleMode = async () => {
    const cur = sessions.find((s) => s.id === selectedId)?.mode ?? "default";
    const next =
      cur === "default" ? "plan" : cur === "plan" ? "bypass" : "default";
    await pickMode(next);
  };

  // Slash actions that need App state: the ChatView handles the local
  // ones (/clear /think /model /stats /help) and routes the rest here.
  const runCommand = async (cmd: string) => {
    switch (cmd) {
      case "/new":
        await create();
        flash("new session");
        break;
      case "/resume": {
        const latest = saved[0]; // ListSnapshots is newest-first
        if (!latest) {
          flash("no saved sessions in this workspace");
          break;
        }
        await restore(latest.path);
        flash(`restored “${latest.title || "(untitled)"}”`);
        break;
      }
      case "/rollback":
        await rollback();
        break;
      case "/sessions":
        flash("live + saved sessions are in the left sidebar");
        break;
      case "/mode":
        await cycleMode();
        break;
      case "/steps":
        // Open-or-focus the steps pane (tiling: no fixed column to switch).
        ws.openPane("steps");
        flash("steps pane");
        break;
      default:
        flash(`unknown command ${cmd}`);
    }
  };

  // openPreviewFile routes a file click to a preview pane (SCC
  // OpenclawPage order): the most recently active preview leaf → the
  // first one → split a new pane beside files/chat. splitLeafAt hands
  // back the fresh nodeId so the tab record can be seeded before paint.
  const openPreviewFile = (path: string, kind?: FileKind) => {
    const leaves = collectLeaves(ws.root).filter((l) => l.pane === "preview");
    let target: string | null = leaves
      .filter((l) => fileTabs.tabs[l.nodeId])
      .sort((a, b) => (fileTabs.tabs[b.nodeId].lastActive) - (fileTabs.tabs[a.nodeId].lastActive))[0]?.nodeId
      ?? leaves[0]?.nodeId
      ?? null;
    if (!target) {
      const anchor = ws.leafOf("files") ?? ws.leafOf("chat");
      if (!anchor) return;
      target = ws.splitLeafAt(anchor, "h", "preview");
    }
    if (!target) return;
    fileTabs.open(target, { path, kind });
    ws.setFocus(target);
  };

  // Workspace paths from markdown links: directories navigate the files
  // pane in place; anything else opens (or focuses) a preview tab. The
  // listDirectory probe doubles as the dir test — a file path errors and
  // falls through to the preview.
  const openWorkspacePath = async (p: string) => {
    try {
      await fileClient.listDirectory({ path: p });
      setFilesNav({ path: p, seq: ++filesNavSeq.current });
      ws.openPane("files");
      const leaf = ws.leafOf("files");
      if (leaf) ws.setFocus(leaf);
    } catch {
      openPreviewFile(p);
    }
  };

  // Pane registry for the tiling workspace: each kind renders its content
  // standalone; layout, focus, and persistence live in useWorkspaceLayout.
  const panes: PaneDef[] = [
    {
      kind: "sessions",
      title: "sessions",
      render: () => (
        <SessionList
          sessions={sessions}
          saved={saved}
          selectedId={selectedId}
          restoring={restoring}
          onSelect={setSelectedId}
          onCreate={() => void create()}
          onRefresh={() => void refresh()}
          onRestore={(p) => void restore(p)}
        />
      ),
    },
    {
      kind: "chat",
      title: selectedId
        ? `chat — ${sessions.find((s) => s.id === selectedId)?.title || selectedId.slice(0, 8)}`
        : "chat",
      render: () =>
        selectedId ? (
          <ChatView
            key={selectedId}
            sessionId={selectedId}
            onChanged={() => void refresh()}
            pending={seatPending}
            onDecide={decide}
            onTodos={setTodos}
            onSteps={setSteps}
            onCommand={runCommand}
            reloadKey={reloadKey}
          />
        ) : (
          <div className="placeholder">select or create a session</div>
        ),
    },
    {
      kind: "files",
      title: "files",
      render: () => <FilePanel onOpenFile={openPreviewFile} nav={filesNav} />,
    },
    { kind: "steps", title: "steps", render: () => <StepsPane steps={steps} /> },
    { kind: "plan", title: "plan", render: () => <PlanPane todos={todos} /> },
    {
      kind: "preview",
      // PaneDef is looked up per kind, so the header title is static; the
      // open file names live on the per-leaf tab strip inside the pane.
      title: "preview",
      render: ({ leafId }) => {
        const tabs = fileTabs.tabs[leafId];
        return (
          <PreviewPane
            files={tabs?.files ?? []}
            active={tabs?.active ?? 0}
            onActivate={(i) => fileTabs.activate(leafId, i)}
            onCloseTab={(i) => fileTabs.closePath(leafId, i)}
            onOpenPath={(p) => void openWorkspacePath(p)}
          />
        );
      },
    },
  ];

  // The dock menu closes on outside click and Escape (portal menu rules).
  useEffect(() => {
    if (!modeMenu) return;
    const onDown = (e: MouseEvent) => {
      if (!modeDockRef.current?.contains(e.target as Node)) setModeMenu(false);
    };
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && setModeMenu(false);
    document.addEventListener("mousedown", onDown);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDown);
      document.removeEventListener("keydown", onKey);
    };
  }, [modeMenu]);

  // The selected session's mode feeds both the dock chip and ChatView's
  // badge (kept until the dock replaced the in-chat controls entirely).
  const curMode = sessions.find((s) => s.id === selectedId)?.mode ?? "default";

  // shift+tab cycles the execution mode anywhere in the app (TUI parity);
  // the browser's default focus-leave is exactly what the chord must NOT do.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Tab" && e.shiftKey) {
        e.preventDefault();
        void cycleMode();
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }); // no deps: the handler always reads the latest sessions/selection

  return (
    <ErrorBoundary>
    <div className="app">
      <header className="topbar">
        <span className="brand">lyon</span>
        <button
          className={"ghost nav" + (view === "chat" ? " active" : "")}
          onClick={() => setView("chat")}
        >
          sessions
        </button>
        <button
          className={"ghost nav" + (view === "dashboard" ? " active" : "")}
          onClick={() => setView("dashboard")}
        >
          dashboard
        </button>
        <input
          className="token"
          type="password"
          placeholder="lyond bearer token"
          value={tokenInput}
          onChange={(e) => setTokenInput(e.target.value)}
          onBlur={onTokenCommit}
          onKeyDown={(e) => e.key === "Enter" && onTokenCommit()}
        />
        {listErr && <span className="err">{listErr}</span>}
      </header>
      {view === "dashboard" ? (
        <Suspense
          fallback={
            <div className="dash">
              <div className="skeleton-rows" aria-hidden="true">
                {Array.from({ length: 6 }, (_, i) => (
                  <div key={i} className="skeleton-row" style={{ width: `${60 + ((i * 13) % 35)}%` }} />
                ))}
              </div>
            </div>
          }
        >
          <DashboardPanel />
        </Suspense>
      ) : (
        <TilingWorkspace
          ws={ws}
          panes={panes}
        />
      )}
      {/* Mode dock — persistent bottom-left control (TUI status bar
          parity): the chip always shows the active mode, click expands a
          checkable picker, shift+tab cycles regardless of focus. */}
      {view === "chat" && (
        <div className="mode-dock" ref={modeDockRef}>
          {modeMenu && (
            <div className="mode-menu">
              {(["default", "plan", "bypass"] as const).map((m) => (
                <button
                  key={m}
                  className={"mode-item" + (curMode === m ? " active" : "")}
                  onClick={() => {
                    setModeMenu(false);
                    void pickMode(m);
                  }}
                >
                  <span className="mode-check">{curMode === m ? "✓" : ""}</span>
                  <span>{m}</span>
                </button>
              ))}
            </div>
          )}
          <button
            className={`mode-chip ${curMode}`}
            title="execution mode — click to pick, shift+tab cycles"
            onClick={() => setModeMenu((v) => !v)}
          >
            {curMode}
          </button>
        </div>
      )}
      {toast && <div className="toast">{toast}</div>}
    </div>
    </ErrorBoundary>
  );
}
