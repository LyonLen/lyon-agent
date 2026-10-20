// MemoryPanel — all three memory layers via MemoryService (team/personal
// live in $LYON_HOME, project in the workspace; the server resolves both
// through pkg/memory's process-wide Store — the same instance the agent's
// save_memory tool writes, so panel edits and model writes share one
// index). The previous FileService-only version could reach project facts
// alone and said so in a footnote; that footnote is gone because the
// footnote is gone.

import { useCallback, useEffect, useRef, useState } from "react";
import { errText, memoryClient } from "../api";

type Scope = "project" | "personal" | "team";

interface MemFact {
  name: string;
  description: string;
  scope: Scope;
  hot: boolean;
  hits: number;
  body: string;
}

// Grouping order for the rail: closest scope first (the store already
// returns project → personal → team, name-ascending within).
const SCOPES: Scope[] = ["project", "personal", "team"];

function isScope(s: string): s is Scope {
  return s === "project" || s === "personal" || s === "team";
}

export default function MemoryPanel() {
  const [facts, setFacts] = useState<MemFact[]>([]);
  const [err, setErr] = useState("");
  const [confirmDel, setConfirmDel] = useState(""); // scope\x00name
  const [flash, setFlash] = useState("");
  const flashTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

  const note = (m: string) => {
    setFlash(m);
    if (flashTimer.current) clearTimeout(flashTimer.current);
    flashTimer.current = setTimeout(() => setFlash(""), 2500);
  };

  const load = useCallback(async () => {
    setErr("");
    try {
      const res = await memoryClient.listMemory({});
      setFacts(
        res.facts.map((f) => ({
          name: f.name,
          description: f.description,
          scope: isScope(f.scope) ? f.scope : "project",
          hot: f.hot,
          hits: Number(f.hits) || 0,
          body: f.body,
        })),
      );
    } catch (e) {
      setFacts([]);
      setErr(errText(e));
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  // Second click confirms the delete; the prompt self-expires.
  useEffect(() => {
    if (!confirmDel) return;
    const t = setTimeout(() => setConfirmDel(""), 3000);
    return () => clearTimeout(t);
  }, [confirmDel]);

  const key = (f: MemFact) => `${f.scope}\u0000${f.name}`;

  const toggleHot = async (f: MemFact) => {
    try {
      // hot is a set (proto optional): the server toggles only on change.
      await memoryClient.updateMemory({ scope: f.scope, name: f.name, hot: !f.hot });
      setFacts((fs) => fs.map((x) => (key(x) === key(f) ? { ...x, hot: !f.hot } : x)));
      note(!f.hot ? `hot: ${f.name}` : `warm: ${f.name}`);
    } catch (e) {
      setErr(errText(e));
    }
  };

  const forget = async (f: MemFact) => {
    if (confirmDel !== key(f)) {
      setConfirmDel(key(f));
      return;
    }
    setConfirmDel("");
    try {
      await memoryClient.deleteMemory({ scope: f.scope, name: f.name });
      setFacts((fs) => fs.filter((x) => key(x) !== key(f)));
      note(`forgotten: ${f.name}`);
    } catch (e) {
      setErr(errText(e));
    }
  };

  const byScope = (s: Scope) => facts.filter((f) => f.scope === s);
  const hotCount = facts.filter((f) => f.hot).length;
  return (
    <div className="mem-panel">
      <div className="mem-head">
        <span className="mem-stats">
          memory {SCOPES.map((s) => `${s}=${byScope(s).length}`).join(" ")} hot={hotCount}
        </span>
        <span className="mem-flash">{flash}</span>
        <button className="ghost" onClick={() => void load()}>
          reload
        </button>
      </div>
      {err && <div className="mem-err err">{err}</div>}
      {facts.length === 0 && !err && (
        <div className="placeholder">
          empty — save_memory from the model, /remember to distill this session
        </div>
      )}
      {SCOPES.map((s) => {
        const group = byScope(s);
        if (group.length === 0) return null;
        return (
          <div key={s} className="mem-group">
            <div className="mem-group-head">
              {s}
              <span className="mem-badge">{group.length}</span>
            </div>
            {group.map((f) => (
              <div key={key(f)} className={"mem-row" + (f.hot ? " hot" : "")}>
                <button
                  className={"mem-hot" + (f.hot ? " on" : "")}
                  title="toggle hot tier"
                  onClick={() => void toggleHot(f)}
                >
                  ●
                </button>
                <div className="mem-main">
                  <div className="mem-name">
                    {f.name}
                    {f.hits > 0 && <span className="mem-hits">hits={f.hits}</span>}
                  </div>
                  {f.description && <div className="mem-desc">{f.description}</div>}
                </div>
                <span className="mem-scope">{f.scope}</span>
                <button className="ghost mem-forget" onClick={() => void forget(f)}>
                  {confirmDel === key(f) ? "confirm?" : "forget"}
                </button>
              </div>
            ))}
          </div>
        );
      })}
    </div>
  );
}
