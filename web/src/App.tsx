import { Suspense, lazy, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import type { Session } from "./gen/lyon/v1/session_pb";
import type { SnapshotRef } from "./gen/lyon/v1/dashboard_pb";
import { ModeAction, type WatchPendingResponse } from "./gen/lyon/v1/approval_pb";
import { approvalClient, dashboardClient, errText, fileClient, getToken, sessionClient, setToken } from "./api";
import type { FileKind } from "./gen/lyon/v1/file_pb";
import SessionList from "./components/SessionList";
import ChatView from "./components/ChatView";
import PlanPane from "./components/PlanPane";
import StepsPane from "./components/StepsPane";
import FilePanel from "./components/FilePanel";
import PreviewPane from "./components/PreviewPane";
import TerminalPane from "./components/TerminalPane";
import MemoryPanel from "./components/MemoryPanel";
import EvolvePanel from "./components/EvolvePanel";
import SettingsPane from "./components/SettingsPane";
import ErrorBoundary from "./components/ErrorBoundary";
import MindMapCard from "./components/MindMapCard";
import TilingWorkspace, { type PaneDef } from "./tiling/TilingWorkspace";
import { useWorkspaceLayout } from "./tiling/useWorkspaceLayout";
import { collectLeaves, findParentSplit, widthShare } from "./tiling/types";
import { useFileTabs } from "./useFileTabs";
import { fmtCount } from "./fmt";

// Dashboard is a separate route-level view — lazy so the chat pane's
// first paint doesn't pay for replay/audit rendering (Phase F chunking).
const DashboardPanel = lazy(() => import("./components/DashboardPanel"));
import type { TodoItem } from "./todos";
import type { StepRow } from "./types";
import type { MindmapSnap } from "./mindmap";

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
  // Leaf id to shake once after a plan-chip click that only focused an
  // already-open pane (see openPlan). Cleared on a timer.
  const [shakeLeaf, setShakeLeaf] = useState("");
  const [steps, setSteps] = useState<StepRow[]>([]);
  // The session mindmap (#51): ChatView owns the revision-guarded snapshot
  // (it sits on the event stream) and reports it up — the map renders in
  // its own pane, a peer of chat. mmJump is the seq'd jump request the
  // pane sends back down to ChatView's feed. The snapshot is TAGGED with
  // its session: during a switch the outgoing session's map is still in
  // hand, and acting on it would mark the incoming session auto-opened
  // against the old pane tree (the round-trip pane-loss bug). The derived
  // `mindmap` stays null until tag and selection agree.
  const [mm, setMm] = useState<{ sid: string; snap: MindmapSnap | null } | null>(null);
  const mindmap = mm && mm.sid === selectedId ? mm.snap : null;
  const [mmJump, setMmJump] = useState<{ turn: number; seq: number } | null>(null);
  // M2 edit channel: the pane's UpdateMindmap response (rev + nodes →
  // ChatView applies under the revision guard) or a conflict refetch
  // (no rev) — seq bumps so duplicate commands still re-fire. sid tags
  // the owning session (Image #69: the incoming ChatView must be able
  // to reject a cmd that belongs to the outgoing seat).
  const [mmCmd, setMmCmd] = useState<{ seq: number; sid: string; rev?: number; nodesJson?: string } | null>(null);
  const mmSeq = useRef(0);
  const mmAutoOpened = useRef<string | null>(null);
  // §7.3 selection-ask: ids live here (both panes render from one truth),
  // selAsk is the seq'd dispatch ChatView's effect consumes, and
  // mmAgentBatches counts agent-origin UpdateMindmap batches whose
  // proposed nodes are still unconfirmed (A5's "先处理" warning).
  const [mmSel, setMmSel] = useState<string[] | null>(null);
  // sid tags the owning seat (same contract as mmCmd): on a switch the
  // incoming ChatView's mount effect runs BEFORE this file's clear effect,
  // so without the tag it would re-dispatch the outgoing seat's ask into
  // the new session (Image #74 — b-open-next-step rode into another seat).
  const [selAsk, setSelAsk] = useState<{ seq: number; sid: string; text: string } | null>(null);
  const [mmAgentBatches, setMmAgentBatches] = useState(0);
  // Session switch: the old seat's selection/ask state must not leak into
  // the next session's map.
  useEffect(() => {
    setMmSel(null);
    setSelAsk(null);
    setMmAgentBatches(0);
  }, [selectedId]);
  // A map with no proposed nodes has nothing pending — A5 disarms.
  useEffect(() => {
    if (mindmap && !mindmap.nodes.some((n) => !n.tombstone && n.state === "proposed")) {
      setMmAgentBatches(0);
    }
  }, [mindmap]);
  const [reloadKey, setReloadKey] = useState(0);
  // Bottom-left mode dock: click expands the three-mode picker; the chip
  // mirrors the selected session's mode (falls back to "default").
  const [modeMenu, setModeMenu] = useState(false);
  // Footer ctx gauge (bottom-right): prompt load reported per turn_end by
  // ChatView; the denominator is the session's context_window (reset to 0
  // on session/rollback reseeds, so no stale gauge across switches).
  const [ctxTok, setCtxTok] = useState(0);
  // Session-cumulative USD (undefined = no unit price on file → hide $)
  const [ctxCost, setCtxCost] = useState<number | undefined>(undefined);
  const modeDockRef = useRef<HTMLDivElement>(null);
  // Layout and preview tab stacks are session-bound: switching the selected
  // session restores that session's own pane tree and open tabs (each
  // inherits the on-screen state the first time it is selected).
  const ws = useWorkspaceLayout(selectedId);

  // The plan chip in the chat footer doubles as the plan pane's remote
  // control: open the pane when it is absent, else focus it and shake the
  // tile so the eye lands on it. openPane already focuses an existing pane,
  // but the shake needs the leaf id and a pause the hook does not model.
  const openPlan = () => {
    if (!ws.hasPane("plan")) {
      ws.openPane("plan");
      return;
    }
    const id = ws.leafOf("plan");
    if (!id) return;
    ws.setFocus(id);
    setShakeLeaf(id);
    window.setTimeout(() => setShakeLeaf((cur) => (cur === id ? "" : cur)), 600);
  };
  // #51: the map is a pane, not a chat-tail card. Open it beside chat the
  // first time the session's snapshot carries nodes — focus stays in the
  // composer — and only once per session so a manual close sticks until
  // the session is re-selected (its saved layout may restore it anyway).
  // Image #71: only a mindmap-mode seat gets the shove — other modes can
  // still VIEW a map (manual /mindmap), they just don't summon it.
  // sessions rides the deps because its mode arrives with the list: a
  // slow list must not leave the gate stuck on the "default" fallback.
  useEffect(() => {
    if (!selectedId || mmAutoOpened.current === selectedId) return;
    if (!mindmap || mindmap.nodes.length === 0) return;
    if ((sessions.find((s) => s.id === selectedId)?.mode ?? "default") !== "mindmap") return;
    mmAutoOpened.current = selectedId;
    if (!ws.hasPane("mindmap")) ws.openPane("mindmap", { focus: false });
  }, [mindmap, selectedId, sessions, ws]);

  const fileTabs = useFileTabs(ws.root, selectedId);
  // External directory navigation for the files pane (markdown links,
  // slash routing): seq bumps so re-requesting the same path reloads.
  const [filesNav, setFilesNav] = useState<{ path: string; seq: number } | null>(null);
  const filesNavSeq = useRef(0);
  // Refs the stream loop reads without re-subscribing on every change:
  // selectedId for toast suppression, pending for new-vs-replay detection.
  const selectedRef = useRef(selectedId);
  const pendingRef = useRef(pending);
  const toastTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  // When the selection last CHANGED — refresh() must not apply a stale
  // list (requested before this selection) to it: "+ new session" during
  // the initial load saw its fresh seat cleared by that very first
  // response, which couldn't contain the id yet ("click a ghost" guard
  // firing on pre-selection data).
  const selectedAtRef = useRef(0);
  useEffect(() => {
    selectedRef.current = selectedId;
    selectedAtRef.current = Date.now();
  }, [selectedId]);
  // Plan/steps are App state (the panes outlive ChatView's key remount), so
  // a fresh selection must start clean — otherwise a new session inherited
  // the previous seat's todo list. Restored seats re-seed the RIGHT todos
  // from their own history right after (ChatView's GetHistory fold).
  useEffect(() => {
    setTodos([]);
    setSteps([]);
    setMm(null);
    setMmJump(null);
    // Image #69: the pane's last command (rev + nodesJson) is the OLD
    // session's map — drop it so it can never re-apply on the new seat
    // (the sid tag in ChatView is the primary guard; this is the belt).
    setMmCmd(null);
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
    const startedAt = Date.now();
    const [live, arch] = await Promise.allSettled([
      sessionClient.listSessions({}),
      dashboardClient.listSnapshots({}),
    ]);
    if (live.status === "fulfilled") {
      const list = [...live.value.sessions]; // server already sorts LastActive desc — newest active on top
      setSessions(list);
      setListErr("");
      // A selected seat that fell out of the live list was reaped or
      // deleted under us (the "click a ghost" hole): clear the selection
      // instead of parking the chat on not_found. Create/restore never
      // trip this — their session is already in the store when the id
      // becomes selected. A response whose request predates the current
      // selection is stale by definition (it cannot contain the id) —
      // skip it, or a fast "+ new session" during load clears itself.
      const sel = selectedRef.current;
      if (sel && startedAt > selectedAtRef.current && !list.some((s) => s.id === sel)) {
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

  // Interval titles (server: beat prompts 1, 6, 26, 46 …) are written by
  // an LLM call that starts AFTER prompt_end lands — one refresh right
  // away would race it and keep showing (untitled). Two beats: a fast one
  // for the common case, a late one for a slow model round trip; a newer
  // prompt_end re-arms both (previous timers cleared).
  const titleTimers = useRef<ReturnType<typeof setTimeout>[]>([]);
  const scheduleTitleRefresh = useCallback(() => {
    for (const t of titleTimers.current) clearTimeout(t);
    titleTimers.current = [];
    for (const ms of [3500, 10000]) {
      titleTimers.current.push(setTimeout(() => void refresh(), ms));
    }
  }, [refresh]);
  useEffect(() => {
    const timers = titleTimers.current;
    return () => {
      for (const t of timers) clearTimeout(t);
    };
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  // List liveness for seats the transcript does not watch (#36): turn
  // counts and busy flags only stream through the SELECTED session's
  // ChatView, so a background run finishing left its row frozen at
  // "running · 0 turns" until a manual ↻. Poll while anything is busy —
  // 2s is enough to keep the row honest and costs one lightweight unary
  // call; an idle workspace polls nothing at all.
  const anyBusy = sessions.some((s) => s.busy);
  const wasBusy = useRef(false);
  useEffect(() => {
    if (anyBusy) {
      wasBusy.current = true;
      const t = setInterval(() => void refresh(), 2000);
      return () => clearInterval(t);
    }
    if (wasBusy.current) {
      wasBusy.current = false;
      // A background run just landed. The interval's last response is the
      // one that showed it idle, which can precede the server's title pass
      // (it runs after prompt_end) — take the same delayed beats the
      // selected seat uses.
      scheduleTitleRefresh();
    }
  }, [anyBusy, refresh, scheduleTitleRefresh]);

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
          const stream = approvalClient.watchPending(
            {},
            {
              // Chrome won't complete the first read on a held-open
              // connect+json chunked stream until ~1s of continued flow;
              // without beats the approval dialog never renders and the
              // parked tool hangs forever (#72/#73).
              headers: { "x-lyon-keepalive": "1" },
            },
          );
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
              setToast(
                ev.modeBlock
                  ? `plan 拦截 ${ev.toolName} — ${title}`
                  : `${ev.toolName} needs approval — ${title}`,
              );
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

  // decide answers a parked approval or plan interception. action is the
  // mode_block vocabulary (approval.proto ModeAction); approvals leave it
  // UNSPECIFIED. The resolve event also removes the card (idempotent);
  // this local removal just makes the click feel instant, and NotFound
  // means someone else — or an interrupt — beat us to it, which is
  // equally a reason to drop the card.
  const decide = async (
    sessionId: string,
    execId: bigint,
    approve: boolean,
    remember: boolean,
    action?: ModeAction,
  ) => {
    try {
      await approvalClient.decide({
        sessionId,
        execId,
        approve,
        remember,
        action: action ?? ModeAction.UNSPECIFIED,
      });
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

  // rollback rewinds the seat to one checkpoint (id 0 = newest): the
  // server rewrites the ring, then the transcript stream re-anchors with
  // -1 and replays the rewound conversation (TUI /rollback parity).
  const rollback = async (id = 0) => {
    if (!selectedId) return;
    try {
      const res = await sessionClient.rollbackSession({
        sessionId: selectedId,
        id, // int32 — 0 picks the newest checkpoint
      });
      const cp = res.checkpoint;
      flash(`rolled back ${cp ? `— “${cp.label}” undone (${cp.messages} msgs restored)` : ""}`);
      setReloadKey((k) => k + 1);
    } catch (e) {
      flash(errText(e));
    }
  };

  // pickMode sets an explicit mode (the bottom-left dock menu); cycleMode
  // walks default → plan → bypass → mindmap → default on top of it (TUI
  // shift+tab cycles only the first three — modeui.go's cycleMode).
  // Picking mindmap (Image #61) opens the map pane with it: that mode is
  // where the map's edits start auto-prompting the agent, so the thing
  // the mode is about should be in front.
  const pickMode = async (mode: string) => {
    if (!selectedId) {
      flash("select a session first");
      return;
    }
    try {
      await sessionClient.setMode({ sessionId: selectedId, mode });
      // Patch locally before the refresh round-trip: the chip (and every
      // mode gate in ChatView) must flip the instant SetMode lands —
      // waiting on refresh left the stale mode visible for ~a round trip,
      // and a quick map edit right after picking mindmap missed the arm.
      setSessions((prev) =>
        prev.map((s) => (s.id === selectedId ? { ...s, mode } : s)),
      );
      if (mode === "mindmap") {
        if (!ws.hasPane("mindmap")) ws.openPane("mindmap");
        flash("mode → mindmap — 图上编辑会自动追问");
      } else {
        flash(`mode → ${mode}`);
      }
      await refresh();
    } catch (e) {
      flash(errText(e));
    }
  };

  const cycleMode = async () => {
    const cur = sessions.find((s) => s.id === selectedId)?.mode ?? "default";
    const next =
      cur === "default"
        ? "plan"
        : cur === "plan"
          ? "bypass"
          : cur === "bypass"
            ? "mindmap"
            : "default";
    await pickMode(next);
  };

  // Slash actions that need App state: ChatView owns the local rows
  // (/clear /think /model /stats /help /resume /rollback /remember)
  // and routes the rest here. The raw string keeps its
  // argument tail — "/steps width 40" parses below.
  const runCommand = async (raw: string) => {
    const [cmd] = raw.trim().split(/\s+/);
    switch (cmd) {
      case "/new":
        await create();
        flash("new session");
        break;
      case "/mode":
        await cycleMode();
        break;
      case "/steps": {
        // subcommands: bare|open opens-or-focuses, "close" drops the pane,
        // "width N" resizes (TUI alt+←→ parity).
        const leaf = ws.leafOf("steps");
        const m = raw.match(/^\/steps\s+width\s+(\d+(?:\.\d+)?)$/i);
        if (m) {
          const pct = Math.min(90, Math.max(10, parseFloat(m[1])));
          const split = leaf && findParentSplit(ws.root, leaf, "h");
          if (split) {
            const ratio = pct / 100;
            ws.updateRatio(split.splitId, split.isFirst ? ratio : 1 - ratio);
            flash(`steps width ${pct}%`);
          } else {
            ws.openPane("steps");
            flash("steps pane opened — width needs a horizontal split");
          }
          break;
        }
        const sub = raw.trim().slice("/steps".length).trim().toLowerCase();
        if (sub === "close") {
          if (leaf) {
            ws.closeLeaf(leaf);
            flash("steps pane closed");
          } else {
            flash("no steps pane open");
          }
          break;
        }
        ws.openPane("steps");
        flash("steps pane");
        break;
      }
      case "/memory":
        // openPane focuses the new (or existing) pane itself.
        ws.openPane("memory");
        flash("memory pane — project scope");
        break;
      case "/mindmap":
      case "/mm":
        ws.openPane("mindmap");
        flash("mindmap pane — 会话的实时理解图");
        break;
      case "/settings":
        ws.openPane("settings");
        flash("settings pane — engine & models");
        break;
      // The evolution family opens one panel (BACKLOG §2.6 — read-only
      // web v1; running an exam/contest still needs the TUI). Each
      // slash names the section the user came for; the pane shows all.
      case "/evolve":
      case "/exam":
      case "/contest":
      case "/route":
      case "/agents":
      case "/flow":
      case "/market":
        ws.openPane("evolve");
        flash("evolve pane — lineage, seats, exam banks");
        break;
      default:
        flash(`unknown command ${raw.trim()}`);
    }
  };

  // openPreviewFile routes a file click to a preview pane (the sibling product
  // the sibling page order): the most recently active preview leaf → the
  // first one → split a new pane. splitLeafAt hands back the fresh nodeId
  // so the tab record can be seeded before paint.
  // #47/#48: a pane split off the old files anchor rendered as an unreadable
  // sliver — a narrow existing pane is NOT reused when splitting off CHAT
  // would be wider; fresh panes land to chat's right at half of chat.
  // diff=true opens the 变更模式 (unified diff) instead (#42) — reached
  // ONLY from the 产出文件 card's change rows; every other caller leaves
  // it false, which also flips a diff tab back to plain preview.
  const openPreviewFile = (path: string, kind?: FileKind, diff = false) => {
    const leaves = collectLeaves(ws.root).filter((l) => l.pane === "preview");
    const recent: string | null = leaves
      .filter((l) => fileTabs.tabs[l.nodeId])
      .sort((a, b) => (fileTabs.tabs[b.nodeId].lastActive) - (fileTabs.tabs[a.nodeId].lastActive))[0]?.nodeId
      ?? leaves[0]?.nodeId
      ?? null;
    // Sliver heuristic (#48): a pane under ~320px can't show a document —
    // replace it with a fresh chat-split when that would actually be wider
    // (splitLeaf's default ratio is 0.5 of the anchor's share).
    const shareOf = (id: string | null) => (id ? widthShare(ws.root, id) : 0);
    const minShare = 320 / Math.max(window.innerWidth, 1);
    const chat = ws.leafOf("chat");
    const freshShare = 0.5 * shareOf(chat);
    let target: string | null = recent;
    if (!target || (shareOf(recent) < minShare && freshShare > shareOf(recent))) {
      // #47: anchor CHAT first — the preview is produced on chat's right;
      // files only as the no-chat fallback (which is what made slivers).
      const anchor = chat ?? ws.leafOf("files");
      if (anchor) target = ws.splitLeafAt(anchor, "h", "preview");
    }
    if (!target) return;
    fileTabs.open(target, { path, kind, diff });
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

  // The selected session's mode feeds the dock chip in the chat footer.
  const curMode = sessions.find((s) => s.id === selectedId)?.mode ?? "default";

  // Resident status strip at the chat pane's bottom (TUI idleStatus +
  // corner-gauge parity): mode chip left with the idle hints (the resident
  // key legend is gone — key help lives in /help, ctrl+o on the thinking
  // block itself), context gauge pinned bottom-right.
  const curSession = sessions.find((s) => s.id === selectedId);
  const ctxWin = Number(curSession?.contextWindow ?? 0);
  // HP-bar gauge (#49): fill by load, color by band — green under 20%,
  // amber through 70%, soft red beyond (digits only, no percentage).
  const ctxPct = ctxWin > 0 ? Math.min(100, (ctxTok / ctxWin) * 100) : 0;
  const ctxBand = ctxPct < 20 ? "" : ctxPct <= 70 ? " warn" : " hot";
  // TUI fmtCost twin (cmd/lyon/usage.go): three decimals under $10 —
  // prompts often cost cents — two above.
  const fmtUSD = (usd: number) =>
    usd < 10 ? `$${usd.toFixed(3)}` : `$${usd.toFixed(2)}`;
  const chatFooter = (
    <footer className="chat-footer">
      <div className="mode-dock" ref={modeDockRef}>
        {modeMenu && (
          <div className="mode-menu">
            {(["default", "plan", "bypass", "mindmap"] as const).map((m) => (
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
          title={
            curMode === "mindmap"
              ? "mindmap — 图上编辑会自动追问（其他模式只入 chat 不发轮）"
              : "execution mode — click to pick, shift+tab cycles"
          }
          onClick={() => setModeMenu((v) => !v)}
        >
          {curMode}
        </button>
      </div>
      <span className="chat-hint">/ for commands · shift+tab mode · ↑ history</span>
      {curSession?.model && (
        <button
          type="button"
          className="chat-model"
          title={
            (curSession.vision
              ? `${curSession.model} — accepts image input`
              : curSession.model) + " · click to open settings"
          }
          onClick={() => ws.openPane("settings")}
        >
          {curSession.vision && (
            <svg
              viewBox="0 0 14 14"
              width="12"
              height="12"
              fill="none"
              stroke="currentColor"
              strokeWidth="1.2"
              strokeLinecap="round"
              strokeLinejoin="round"
              aria-hidden
            >
              <path d="M1 7s2.2-3.6 6-3.6S13 7 13 7s-2.2 3.6-6 3.6S1 7 1 7Z" />
              <circle cx="7" cy="7" r="1.8" />
            </svg>
          )}
          {curSession.model}
        </button>
      )}
      {ctxWin > 0 && (
        <span
          className={"ctx-gauge" + ctxBand}
          title="context window as an HP bar — green < 20% ≤ amber ≤ 70% < soft red"
        >
          <span className="ctx-bar">
            <span
              className={"ctx-fill" + (ctxTok > 0 ? " on" : "")}
              style={{ width: `${ctxTok > 0 ? Math.max(ctxPct, 1.5) : 0}%` }}
            />
          </span>
          {fmtCount(ctxTok)}/{fmtCount(ctxWin)}
          {ctxCost !== undefined && ctxCost > 0 && (
            <span className="ctx-cost"> · {fmtUSD(ctxCost)}</span>
          )}
        </span>
      )}
    </footer>
  );

  // Pane registry for the tiling workspace: each kind renders its content
  // standalone; layout, focus, and persistence live in useWorkspaceLayout.
  // Icons mirror a sibling product's pane picker (title + one-line description each).
  const panes: PaneDef[] = [
    {
      kind: "sessions",
      title: "sessions",
      desc: "session list — create & restore",
      icon: (
        <svg viewBox="0 0 14 14" width="14" height="14" fill="none" stroke="currentColor" strokeWidth="1.3" strokeLinecap="round" aria-hidden>
          <rect x="2" y="2.5" width="10" height="3" rx="1" />
          <rect x="2" y="8.5" width="10" height="3" rx="1" />
        </svg>
      ),
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
      desc: "conversation with the model",
      icon: (
        <svg viewBox="0 0 14 14" width="14" height="14" fill="none" stroke="currentColor" strokeWidth="1.3" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
          <path d="M2 3.5a1 1 0 0 1 1-1h8a1 1 0 0 1 1 1v5a1 1 0 0 1-1 1H5.5L3 12V9.5H3a1 1 0 0 1-1-1z" />
        </svg>
      ),
      render: () =>
        selectedId ? (
          <div className="chat-slot">
            <ChatView
              key={selectedId}
              sessionId={selectedId}
              onChanged={() => void refresh()}
              pending={seatPending}
              onDecide={decide}
              onTodos={setTodos}
              onOpenPlan={openPlan}
              onSteps={setSteps}
              onCommand={runCommand}
              onOpenFile={openPreviewFile}
              onOpenDiff={(p) => openPreviewFile(p, undefined, true)}
              onPromptEnd={scheduleTitleRefresh}
              onCtx={setCtxTok}
              onCost={setCtxCost}
              reloadKey={reloadKey}
              snapshots={saved}
              onRestore={(p) => void restore(p)}
              onRollbackTo={(id) => void rollback(id)}
              onMindmap={(snap, sid) => setMm({ sid, snap })}
              jumpTo={mmJump}
              mmCmd={mmCmd}
              mmSelIds={mmSel}
              onMmSelect={setMmSel}
              selAsk={selAsk}
              mode={curMode}
              onAgentBatch={() => setMmAgentBatches((c) => c + 1)}
            />
            {chatFooter}
          </div>
        ) : (
          <div className="placeholder">select or create a session</div>
        ),
    },
    {
      kind: "mindmap",
      title: mindmap?.title ? `mindmap — ${mindmap.title}` : "mindmap",
      desc: "session's live understanding map",
      icon: (
        <svg viewBox="0 0 14 14" width="14" height="14" fill="none" stroke="currentColor" strokeWidth="1.3" strokeLinecap="round" aria-hidden>
          <circle cx="7" cy="3.2" r="1.7" />
          <circle cx="3" cy="10.8" r="1.5" />
          <circle cx="11" cy="10.8" r="1.5" />
          <path d="M6.2 4.7 3.7 9.3M7.8 4.7l2.5 4.6" />
        </svg>
      ),
      render: () =>
        selectedId ? (
          mindmap ? (
            <div className="mindmap-pane">
              <MindMapCard
                snap={mindmap}
                sessionId={selectedId}
                onJump={(turn) => setMmJump({ turn, seq: ++mmSeq.current })}
                onApplied={(rev, nodesJson) =>
                  setMmCmd((c) => ({ seq: (c?.seq ?? 0) + 1, sid: selectedId, rev, nodesJson }))
                }
                onResync={() =>
                  setMmCmd((c) => ({ seq: (c?.seq ?? 0) + 1, sid: selectedId }))
                }
                selected={mmSel}
                onSelect={setMmSel}
                onAsk={(text) =>
                  setSelAsk((s) => ({ seq: (s?.seq ?? 0) + 1, sid: selectedId, text }))
                }
                agentBatches={mmAgentBatches}
              />
            </div>
          ) : (
            <div className="placeholder">no mindmap yet — it grows with the conversation</div>
          )
        ) : (
          <div className="placeholder">select or create a session</div>
        ),
    },
    {
      kind: "files",
      title: "files",
      desc: "browse & manage workspace files",
      icon: (
        <svg viewBox="0 0 14 14" width="14" height="14" fill="none" stroke="currentColor" strokeWidth="1.3" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
          <path d="M1.8 4.5a1 1 0 0 1 1-1h2.4l1.2 1.5h4.8a1 1 0 0 1 1 1v4.5a1 1 0 0 1-1 1H2.8a1 1 0 0 1-1-1z" />
        </svg>
      ),
      render: () => <FilePanel onOpenFile={openPreviewFile} nav={filesNav} />,
    },
    {
      kind: "steps",
      title: "steps",
      desc: "live tool execution stream",
      icon: (
        <svg viewBox="0 0 14 14" width="14" height="14" fill="none" stroke="currentColor" strokeWidth="1.3" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
          <path d="M2 3.5h1.5M2 7h1.5M2 10.5h1.5M5.5 3.5H12M5.5 7H12M5.5 10.5H12" />
        </svg>
      ),
      render: () => <StepsPane steps={steps} />,
    },
    {
      kind: "plan",
      title: "plan",
      desc: "task checklist & progress",
      icon: (
        <svg viewBox="0 0 14 14" width="14" height="14" fill="none" stroke="currentColor" strokeWidth="1.3" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
          <rect x="2.5" y="2" width="9" height="10" rx="1" />
          <path d="M4.5 5l1 1 1.5-1.5M4.5 9l1 1 1.5-1.5M8.5 5.5H10M8.5 9.5H10" />
        </svg>
      ),
      render: () => <PlanPane todos={todos} />,
    },
    {
      kind: "preview",
      // PaneDef is looked up per kind, so the header title is static; the
      // open file names live on the per-leaf tab strip inside the pane.
      title: "preview",
      desc: "view & edit file contents",
      icon: (
        <svg viewBox="0 0 14 14" width="14" height="14" fill="none" stroke="currentColor" strokeWidth="1.3" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
          <path d="M3.5 2h5L11 4.5V12h-7.5z" />
          <path d="M8.5 2v2.5H11" />
        </svg>
      ),
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
    {
      kind: "terminal",
      // One shell per (chat session, pane): switching sessions switches
      // terminals, and a session may keep several terminal panes open.
      title: "terminal",
      desc: "shell in the workspace",
      icon: (
        <svg viewBox="0 0 14 14" width="14" height="14" fill="none" stroke="currentColor" strokeWidth="1.3" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
          <rect x="1.8" y="2.5" width="10.4" height="9" rx="1" />
          <path d="M4 5.5l2 1.75L4 9M7.5 9H10" />
        </svg>
      ),
      render: ({ leafId }) => (
        <TerminalPane sessionId={selectedId ?? ""} paneId={leafId} />
      ),
    },
    {
      kind: "memory",
      title: "memory — project",
      desc: "project memory — browse & curate",
      icon: (
        <svg viewBox="0 0 14 14" width="14" height="14" fill="none" stroke="currentColor" strokeWidth="1.3" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
          <path d="M4.5 3.2a2.2 2.2 0 0 0-1.7 3.6 2.2 2.2 0 0 0 1 3.9c.5 1.1 1.9 1.5 2.9.8.4.4 1 .6 1.6.5a2.2 2.2 0 0 0 1.9-2.6 2.2 2.2 0 0 0 1.3-3.3c-.3-1-1.2-1.7-2.3-1.7-.3-1-1.3-1.7-2.4-1.5-.8.1-1.5.7-1.8 1.4z" />
        </svg>
      ),
      render: () => <MemoryPanel />,
    },
    {
      kind: "settings",
      title: "settings — engine & models",
      desc: "live model tier & environment",
      icon: (
        <svg viewBox="0 0 14 14" width="14" height="14" fill="none" stroke="currentColor" strokeWidth="1.3" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
          <circle cx="7" cy="7" r="1.8" />
          <path d="M7 1.6v1.6M7 10.8v1.6M1.6 7h1.6M10.8 7h1.6M3.2 3.2l1.1 1.1M9.7 9.7l1.1 1.1M10.8 3.2l-1.1 1.1M4.3 9.7l-1.1 1.1" />
        </svg>
      ),
      render: () => <SettingsPane />,
    },
    {
      kind: "evolve",
      title: "evolve — lineage & exams",
      desc: "candidate lineage, seats, exam banks",
      icon: (
        <svg viewBox="0 0 14 14" width="14" height="14" fill="none" stroke="currentColor" strokeWidth="1.3" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
          <path d="M7 12.5V8M7 8L3.8 5.4M7 8l3.2-2.6M7 8v-4.5M4.5 1.8a1 1 0 1 0 0-.1M9.5 3.3a1 1 0 1 0 0-.1M3.3 6.6a1 1 0 1 0 0-.1" />
        </svg>
      ),
      render: () => <EvolvePanel />,
    },
  ];

  // The dock menu closes on outside click and Escape (usual menu rules).
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

  // alt+arrows resize the focused pane (TUI alt+←→ width parity,
  // extended to vertical). Guard chain per D2: defaultPrevented first,
  // then never while typing — the textarea owns its keys.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.defaultPrevented) return;
      if (!e.altKey || e.key.startsWith("Arrow") === false) return;
      const ae = document.activeElement;
      // isContentEditable catches CodeMirror's .cm-content (a div — the
      // tagName check alone lets alt+arrow steal keys mid-code-edit)
      if (ae && (ae.tagName === "TEXTAREA" || ae.tagName === "INPUT" || (ae as HTMLElement).isContentEditable)) return;
      const leafId = ws.focus;
      if (!leafId) return;
      const map: Record<string, { dir: "h" | "v"; delta: number }> = {
        // TUI physical intuition: ← widens (gutter moves left), → narrows.
        ArrowLeft: { dir: "h", delta: +0.04 },
        ArrowRight: { dir: "h", delta: -0.04 },
        ArrowUp: { dir: "v", delta: +0.04 },
        ArrowDown: { dir: "v", delta: -0.04 },
      };
      const act = map[e.key];
      if (!act) return;
      e.preventDefault();
      const split = findParentSplit(ws.root, leafId, act.dir);
      if (!split) {
        flash(`no ${act.dir === "h" ? "horizontal" : "vertical"} split on this pane`);
        return;
      }
      const ratio = split.ratio + (split.isFirst ? act.delta : -act.delta);
      ws.updateRatio(split.splitId, ratio);
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }); // no deps: always reads the latest focus/root

  // shift+tab cycles the execution mode anywhere in the app (TUI parity);
  // the browser's default focus-leave is exactly what the chord must NOT do.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Tab" && e.shiftKey) {
        const ae = document.activeElement;
        // in an editor (textarea, input, CM contenteditable) shift+tab is
        // the editor's unindent — the mode cycle must not eat it
        if (ae && (ae.tagName === "TEXTAREA" || ae.tagName === "INPUT" || (ae as HTMLElement).isContentEditable)) return;
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
          shakeLeaf={shakeLeaf}
        />
      )}
      {/* (the resident mode chip + legend now live inside the chat pane —
          see the chatFooter definition above the pane registry) */}
      {toast && <div className="toast">{toast}</div>}
    </div>
    </ErrorBoundary>
  );
}
