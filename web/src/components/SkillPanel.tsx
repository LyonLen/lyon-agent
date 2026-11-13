// SkillPanel — three sections (the skill feature's read side):
//   1. 本会话命中 — skill_used events folded out of the session's
//      transcript (the same events the chat cards render), deduped by
//      name; click expands the SKILL.md through SkillService.GetSkill.
//   2. 已安装 — SkillService.ListSkills (personal + commons + workspace
//      .lyon/skills), same expand-on-click viewer.
//   3. 推荐安装 — a curated list (grounded in skills.sh's leaderboard);
//      安装 sends an install prompt to the CURRENT session, where the
//      model runs `npx skills add` — the panel never shells out itself.
//
// Hits come from GetHistory, not a live stream: the panel is opened to
// review, not to watch; the header's refresh re-folds after a run.

import { useCallback, useEffect, useState } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { errText, sessionClient, skillClient } from "../api";

type Skill = { name: string; description: string; path: string; source: string };

// Curated from the skills.sh leaderboard (find-skills' Step 2): official
// sources, 100K+ installs — the bar that doc sets for a recommendation.
const RECOMMENDED: Array<{ pkg: string; title: string; desc: string }> = [
  {
    pkg: "vercel-labs/agent-skills",
    title: "React / Next.js / Web 设计",
    desc: "Vercel 官方 — React、Next.js、web design 技能（100K+ installs）",
  },
  {
    pkg: "anthropics/skills",
    title: "前端设计 / 文档处理",
    desc: "Anthropic 官方 — frontend-design、pdf/docx/xlsx 文档处理（100K+ installs）",
  },
  {
    pkg: "ComposioHQ/awesome-claude-skills",
    title: "社区精选合集",
    desc: "跨域社区技能包 — devops / 文档 / 生产力",
  },
];

// The scan roots are .lyon only (never .claude), so the install has to
// END in the workspace's .lyon/skills: stage with the skills CLI under
// its universal agent (.agents/skills/, --copy for real files), then
// relocate. -g would land in the agent's home dir and stay invisible.
const installPrompt = (pkg: string) =>
  `Install the skill package "${pkg}" into this workspace: run \`npx skills add ${pkg} -a universal -y --copy\` ` +
  `(stages real files under .agents/skills/), then move every newly staged skill directory into this ` +
  `workspace's .lyon/skills/ (mkdir -p .lyon/skills first), then read each new SKILL.md and list what got ` +
  `installed, one line per skill. If the command fails, report the error verbatim — do not improvise an install.`;

export default function SkillPanel({ sessionId, root }: { sessionId: string; root?: string }) {
  const [installed, setInstalled] = useState<Skill[]>([]);
  const [hits, setHits] = useState<Skill[]>([]);
  const [open, setOpen] = useState(""); // expanded row key: `${section}:${name}`
  const [content, setContent] = useState("");
  const [contentErr, setContentErr] = useState("");
  const [loading, setLoading] = useState(false);
  const [flash, setFlash] = useState("");
  const [err, setErr] = useState("");

  // Installed list follows the workspace (workspace .lyon/skills rides
  // the x-lyon-workspace header); hits follow the session.
  const loadInstalled = useCallback(async () => {
    try {
      const res = await skillClient.listSkills({});
      setInstalled(res.skills.map((s) => ({ ...s, source: s.source || "personal" })));
      setErr("");
    } catch (e) {
      setErr(errText(e));
    }
  }, []);

  const loadHits = useCallback(async () => {
    if (!sessionId) {
      setHits([]);
      return;
    }
    try {
      const hist = await sessionClient.getHistory({ sessionId });
      const seen = new Set<string>();
      const out: Skill[] = [];
      for (const ev of hist.events) {
        if (ev.typeName !== "skill_used") continue;
        const name = ev.attrs["name"];
        if (!name || seen.has(name)) continue;
        seen.add(name);
        out.push({
          name,
          path: ev.attrs["path"] || "",
          description: ev.attrs["desc"] || "",
          source: "hit",
        });
      }
      setHits(out);
    } catch {
      // history unavailable (reaped seat): keep the last fold
    }
  }, [sessionId]);

  useEffect(() => {
    void loadInstalled();
  }, [loadInstalled, root]);
  useEffect(() => {
    void loadHits();
  }, [loadHits]);

  // One viewer for both sections: click loads the SKILL.md, click again
  // folds it (per-row state is the shared `open` key — one open row).
  const expand = async (section: string, s: Skill) => {
    const key = `${section}:${s.name}`;
    if (open === key) {
      setOpen("");
      return;
    }
    setOpen(key);
    setContent("");
    setContentErr("");
    setLoading(true);
    try {
      const res = await skillClient.getSkill({ name: s.name, path: s.path });
      setContent(res.content + (res.truncated ? "\n... [truncated]" : ""));
    } catch (e) {
      setContentErr(errText(e));
    } finally {
      setLoading(false);
    }
  };

  // Install = prompt the current session (busy → queue, same fallback
  // chain ChatView's dispatcher uses); the model runs npx skills add.
  const install = async (pkg: string) => {
    if (!sessionId) {
      setErr("select a session first — install runs as its prompt");
      return;
    }
    try {
      try {
        await sessionClient.submitPrompt({ sessionId, prompt: installPrompt(pkg) });
      } catch (e) {
        if (ConnectError.from(e).code !== Code.FailedPrecondition) throw e;
        await sessionClient.enqueuePrompt({ sessionId, prompt: installPrompt(pkg) });
      }
      setErr("");
      setFlash(`已发送安装提示：${pkg} — 去聊天里看执行结果`);
    } catch (e) {
      setErr(errText(e));
    }
  };

  const renderRow = (section: string, s: Skill) => {
    const key = `${section}:${s.name}`;
    const isOpen = open === key;
    return (
      <div key={key} className={"sk-row" + (isOpen ? " open" : "")}>
        <button type="button" className="sk-main" onClick={() => void expand(section, s)}>
          <span className="sk-name">{s.name}</span>
          {s.source && <span className={`sk-src ${s.source}`}>{s.source}</span>}
          {s.description && <span className="sk-desc">{s.description}</span>}
        </button>
        {isOpen && (
          <div className="sk-view">
            {loading ? (
              <div className="sk-loading">loading…</div>
            ) : contentErr ? (
              <div className="err sk-err">{contentErr}</div>
            ) : (
              <pre className="sk-content">{content}</pre>
            )}
          </div>
        )}
      </div>
    );
  };

  return (
    <div className="sk-panel">
      <div className="sk-head">
        <span className="sk-title">skills</span>
        <span className="sk-meta">
          命中 {hits.length} · 已装 {installed.length}
        </span>
        <span className="mem-flash">{flash}</span>
        <button
          className="ghost"
          title="re-fold hits from the transcript and rescan the installed list"
          onClick={() => {
            void loadHits();
            void loadInstalled();
          }}
        >
          refresh
        </button>
      </div>
      {err && <div className="err sk-globalerr">{err}</div>}

      <div className="sk-sec">
        <div className="sk-sechead">
          本会话命中 <span className="sk-badge">{hits.length}</span>
        </div>
        {hits.length === 0 ? (
          <div className="sk-empty">本会话还没有命中 skill — 模型读取 SKILL.md 后这里会出现卡片</div>
        ) : (
          hits.map((s) => renderRow("hit", s))
        )}
      </div>

      <div className="sk-sec">
        <div className="sk-sechead">
          已安装 <span className="sk-badge">{installed.length}</span>
        </div>
        {installed.length === 0 ? (
          <div className="sk-empty">no installed skills — ~/.lyon/skills, commons, workspace .lyon/skills</div>
        ) : (
          installed.map((s) => renderRow("inst", s))
        )}
      </div>

      <div className="sk-sec">
        <div className="sk-sechead">
          推荐安装 <span className="sk-badge">{RECOMMENDED.length}</span>
        </div>
        {RECOMMENDED.map((r) => (
          <div key={r.pkg} className="sk-row sk-rec">
            <div className="sk-main">
              <span className="sk-name">{r.title}</span>
              <span className="sk-desc">{r.desc}</span>
              <span className="sk-pkg">{r.pkg}</span>
            </div>
            <button
              type="button"
              className="sk-install"
              disabled={!sessionId}
              title={sessionId ? "给当前会话发安装提示（npx skills add）" : "先选一个会话"}
              onClick={() => void install(r.pkg)}
            >
              安装
            </button>
          </div>
        ))}
      </div>
    </div>
  );
}
