import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import type { Session } from "./gen/lyon/v1/session_pb";
import type { SnapshotRef } from "./gen/lyon/v1/dashboard_pb";
import type { WatchPendingResponse } from "./gen/lyon/v1/approval_pb";
import { approvalClient, dashboardClient, errText, getToken, sessionClient, setToken } from "./api";
import SessionList from "./components/SessionList";
import ChatView from "./components/ChatView";
import FilePanel from "./components/FilePanel";
import DashboardPanel from "./components/DashboardPanel";

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

  // Live sessions and the saved snapshot store are independent reads —
  // fetch both concurrently, each with its own error so one failing
  // channel doesn't blank the other.
  const refresh = useCallback(async () => {
    const [live, arch] = await Promise.allSettled([
      sessionClient.listSessions({}),
      dashboardClient.listSnapshots({}),
    ]);
    if (live.status === "fulfilled") {
      setSessions([...live.value.sessions].reverse()); // oldest first; newest active sits at the bottom near the chat
      setListErr("");
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
  // becomes the selection and ChatView's from_seq=-1 replay draws the
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

  return (
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
        <DashboardPanel />
      ) : (
        <div className="cols">
        <aside className="col-sessions">
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
        </aside>
        <main className="col-chat">
          {selectedId ? (
            <ChatView
              key={selectedId}
              sessionId={selectedId}
              onChanged={() => void refresh()}
              pending={seatPending}
              onDecide={decide}
            />
          ) : (
            <div className="placeholder">select or create a session</div>
          )}
        </main>
        <aside className="col-files">
          <FilePanel />
        </aside>
        </div>
      )}
      {toast && <div className="toast">{toast}</div>}
    </div>
  );
}
