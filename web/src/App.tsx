import { useCallback, useEffect, useState } from "react";
import type { Session } from "./gen/lyon/v1/session_pb";
import { errText, getToken, sessionClient, setToken } from "./api";
import SessionList from "./components/SessionList";
import ChatView from "./components/ChatView";
import FilePanel from "./components/FilePanel";
import DashboardPanel from "./components/DashboardPanel";

export default function App() {
  const [tokenInput, setTokenInput] = useState(getToken());
  const [sessions, setSessions] = useState<Session[]>([]);
  const [selectedId, setSelectedId] = useState("");
  const [listErr, setListErr] = useState("");
  const [view, setView] = useState<"chat" | "dashboard">("chat");

  const refresh = useCallback(async () => {
    try {
      const res = await sessionClient.listSessions({});
      setSessions([...res.sessions].reverse()); // oldest first; newest active sits at the bottom near the chat
      setListErr("");
    } catch (err) {
      setListErr(errText(err));
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const create = async () => {
    try {
      const res = await sessionClient.createSession({});
      setSelectedId(res.session?.id ?? "");
      await refresh();
    } catch (err) {
      setListErr(errText(err));
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
            selectedId={selectedId}
            onSelect={setSelectedId}
            onCreate={() => void create()}
            onRefresh={() => void refresh()}
          />
        </aside>
        <main className="col-chat">
          {selectedId ? (
            <ChatView
              key={selectedId}
              sessionId={selectedId}
              onChanged={() => void refresh()}
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
    </div>
  );
}
