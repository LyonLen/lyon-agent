// MemoryPanel — all three memory layers via MemoryService (team/personal
// live in $LYON_HOME, project in the workspace; the server resolves both
// through pkg/memory's process-wide Store — the same instance the agent's
// save_memory tool writes, so panel edits and model writes share one
// index). The previous FileService-only version could reach project facts
// alone and said so in a footnote; that footnote is gone because the
// footnote is gone.
//
// Image #70 — two additions:
//   F1 检索验证 — a query box re-runs the agent's BM25 ranking client
//      side (memsearch.ts mirrors index.go): you see the score, the
//      matched terms and the matching passage, i.e. whether read_memory
//      could find the fact you expect. 0 hits says so plainly.
//   F2 分类 — tag chips filter the list (mindmap facts carry their
//      topic tag, hand-written facts often none → 未分类). Tags are
//      read-only: UpdateMemory has no tags field.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { errText, memoryClient } from "../api";
import { excerpt, highlight, memSearch, uniqueTokens } from "../memsearch";

type Scope = "project" | "personal" | "team";

interface MemFact {
  name: string;
  description: string;
  scope: Scope;
  tags: string[];
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
  const [openFact, setOpenFact] = useState(""); // scope\x00name
  const [flash, setFlash] = useState("");
  // F1: the verification query; non-empty switches the rail from the
  // grouped browse to the ranked hit list.
  const [q, setQ] = useState("");
  // F2: active tag filter — null = 全部, "" = 未分类, else a real tag.
  const [tag, setTag] = useState<string | null>(null);
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
          tags: f.tags ?? [],
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

  // F2: tag inventory — counts across ALL facts (the chip row is the
  // classification index; it must not shrink when a filter is active).
  const tagInv = useMemo(() => {
    const counts = new Map<string, number>();
    let untagged = 0;
    for (const f of facts) {
      if (f.tags.length === 0) untagged++;
      for (const t of f.tags) counts.set(t, (counts.get(t) ?? 0) + 1);
    }
    const entries = [...counts].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]));
    return { entries, untagged };
  }, [facts]);

  // The browse set after the F2 filter; F1 searches within it too, so
  // "分类里搜不到" and "全库搜不到" are distinguishable readouts.
  const base = useMemo(() => {
    if (tag === null) return facts;
    if (tag === "") return facts.filter((f) => f.tags.length === 0);
    return facts.filter((f) => f.tags.includes(tag));
  }, [facts, tag]);

  const query = q.trim();
  const qTerms = useMemo(() => (query ? uniqueTokens(query) : []), [query]);
  const hits = useMemo(() => (query ? memSearch(query, base) : null), [query, base]);

  // Head stats stay GLOBAL (the inventory line); group badges below
  // follow the active filter.
  const countScope = (fs: MemFact[], s: Scope) => fs.filter((f) => f.scope === s).length;
  const hotCount = facts.filter((f) => f.hot).length;

  const renderRow = (f: MemFact, hitScore?: number) => {
    const isOpen = openFact === key(f);
    // F1 rows lead with a score bar relative to the top hit; browse
    // rows keep the hot dot.
    const rel =
      hitScore !== undefined && hits && hits.length > 0 && hits[0].score > 0
        ? Math.max(4, Math.round((hitScore / hits[0].score) * 100))
        : 0;
    return (
      <div
        key={key(f)}
        className={
          "mem-row" +
          (f.hot ? " hot" : "") +
          (isOpen ? " open" : "") +
          (hitScore !== undefined ? " hit" : "")
        }
      >
        {hitScore !== undefined ? (
          <span
            className="mem-score"
            title={`bm25 ${hitScore.toFixed(3)} — 与顶部命中的相对分`}
          >
            <i style={{ width: `${rel}%` }} />
          </span>
        ) : (
          <button
            className={"mem-hot" + (f.hot ? " on" : "")}
            title="toggle hot tier"
            onClick={(e) => {
              e.stopPropagation();
              void toggleHot(f);
            }}
          >
            ●
          </button>
        )}
        <div
          className="mem-main"
          title="click to expand"
          onClick={() => setOpenFact(isOpen ? "" : key(f))}
        >
          <div className="mem-name">
            {f.name}
            {hitScore !== undefined && (
              <span className="mem-hits">score {hitScore.toFixed(2)}</span>
            )}
            {hitScore === undefined && f.hits > 0 && (
              <span className="mem-hits">hits={f.hits}</span>
            )}
          </div>
          {hitScore !== undefined ? (
            // F1: the passage that made the score — highlighted terms
            // are the verification (you see exactly what matched).
            <div className="mem-excerpt">
              {highlight(excerpt(f.body || f.description, qTerms), qTerms).map((seg, i) =>
                seg.on ? <mark key={i}>{seg.s}</mark> : <span key={i}>{seg.s}</span>,
              )}
            </div>
          ) : (
            f.description && <div className="mem-desc">{f.description}</div>
          )}
          {isOpen && (
            <>
              {f.tags.length > 0 && (
                <div className="mem-tags-in">
                  {f.tags.map((t) => (
                    <span key={t} className="mem-tag-s">
                      {t}
                    </span>
                  ))}
                </div>
              )}
              <pre className="mem-body">{f.body || "(empty)"}</pre>
            </>
          )}
        </div>
        <span className="mem-scope">{f.scope}</span>
        <button
          className="ghost mem-forget"
          onClick={(e) => {
            e.stopPropagation();
            void forget(f);
          }}
        >
          {confirmDel === key(f) ? "confirm?" : "forget"}
        </button>
      </div>
    );
  };

  return (
    <div className="mem-panel">
      <div className="mem-head">
        <span className="mem-stats">
          memory {SCOPES.map((s) => `${s}=${countScope(facts, s)}`).join(" ")} hot={hotCount}
        </span>
        <input
          className="mem-q"
          value={q}
          placeholder="检索验证 — 输入问题，看记忆排不排得到"
          title="F1：用 agent 的 BM25 排序在浏览器里重跑一遍 — 命中分、命中词、命中段落"
          onChange={(e) => setQ(e.target.value)}
        />
        {q && (
          <button className="ghost" onClick={() => setQ("")} title="clear query">
            ✕
          </button>
        )}
        <span className="mem-flash">{flash}</span>
        <button className="ghost" onClick={() => void load()}>
          reload
        </button>
      </div>
      {facts.length > 0 && (tagInv.entries.length > 0 || tagInv.untagged > 0) && (
        <div className="mem-tags">
          <button
            className={"mem-tag" + (tag === null ? " on" : "")}
            onClick={() => setTag(null)}
          >
            全部 <b>{facts.length}</b>
          </button>
          {tagInv.entries.map(([t, n]) => (
            <button
              key={t}
              className={"mem-tag" + (tag === t ? " on" : "")}
              title={`只看带 ${t} 标签的记忆`}
              onClick={() => setTag(tag === t ? null : t)}
            >
              {t} <b>{n}</b>
            </button>
          ))}
          {tagInv.untagged > 0 && (
            <button
              className={"mem-tag" + (tag === "" ? " on" : "")}
              title="没有任何标签的记忆"
              onClick={() => setTag(tag === "" ? null : "")}
            >
              未分类 <b>{tagInv.untagged}</b>
            </button>
          )}
        </div>
      )}
      {err && <div className="mem-err err">{err}</div>}
      {facts.length === 0 && !err && (
        <div className="placeholder">
          empty — save_memory from the model, /remember to distill this session
        </div>
      )}
      {hits !== null && facts.length > 0 && (
        <>
          {hits.length === 0 ? (
            <div className="mem-nohit">
              <b>0 命中</b> — 这条查询在{tag !== null ? "当前分类 " : ""}
              {base.length} 条记忆里检索不到（agent 的 read_memory 同样搜不到）
              {qTerms.length > 0 && (
                <div className="mem-nohit-terms">查询词：{qTerms.join(" / ")}</div>
              )}
            </div>
          ) : (
            <div className="mem-group">
              <div className="mem-group-head">
                检索验证
                <span className="mem-badge">{hits.length}</span>
                <span className="mem-qmeta">
                  {hits.length}/{base.length} 命中 · top {hits[0].score.toFixed(2)}
                  {tag !== null && " · 已按分类过滤"}
                </span>
              </div>
              {hits.map((h) => renderRow(h.fact, h.score))}
            </div>
          )}
        </>
      )}
      {(hits === null || facts.length === 0) &&
        SCOPES.map((s) => {
          const group = base.filter((f) => f.scope === s);
          if (group.length === 0) return null;
          return (
            <div key={s} className="mem-group">
              <div className="mem-group-head">
                {s}
                <span className="mem-badge">{group.length}</span>
                {tag !== null && (
                  <span className="mem-qmeta">分类：{tag === "" ? "未分类" : tag}</span>
                )}
              </div>
              {group.map((f) => renderRow(f))}
            </div>
          );
        })}
      {hits === null && tag !== null && base.length === 0 && facts.length > 0 && (
        <div className="placeholder">这个分类下没有记忆</div>
      )}
    </div>
  );
}
