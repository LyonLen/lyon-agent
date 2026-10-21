// SettingsPane — the running engine's brain, as it actually is.
//
// Two jobs, one pane: (1) answer "which model is answering and what can it
// do" from the knowledge base (pkg/models, the same table the TUI's /model
// card prints), and (2) switch the main / subagent tier at runtime through
// SettingsService.UpdateSettings — the engine's atomic tier, so long-lived
// sessions pick the change up on their next turn and the ctx gauge adopts
// the new model's window immediately.
//
// lyond is configured at launch (flags + environment) and never reads
// ~/.lyon/config.toml, so the lower half reports the process's effective
// config and environment inventory read-only. Credential-shaped variables
// arrive masked and are rendered as "set" — never echoed.

import { useCallback, useEffect, useMemo, useState } from "react";
import { errText, settingsClient } from "../api";
import { fmtCount } from "../fmt";
import type { EngineSettings, EnvVar, ModelInfo } from "../gen/lyon/v1/settings_pb";

// scaleBars renders one 0–5 capability dimension as filled/empty blocks —
// five glyphs read faster than a number ("推理 ■■■■□").
function scaleBars(v: number): string {
  const n = Math.max(0, Math.min(5, Math.round(v)));
  return "■".repeat(n) + "□".repeat(5 - n);
}

function priceText(m: ModelInfo): string {
  const p = m.pricing;
  if (!p || !p.known) return "价格未知";
  return `$${p.input.toFixed(2)} / $${p.output.toFixed(2)}`;
}

function ctxText(n: bigint): string {
  const v = Number(n);
  return v > 0 ? fmtCount(v) : "未注明";
}

export default function SettingsPane() {
  const [engine, setEngine] = useState<EngineSettings | null>(null);
  const [models, setModels] = useState<ModelInfo[]>([]);
  const [env, setEnv] = useState<EnvVar[]>([]);
  const [err, setErr] = useState("");
  const [flash, setFlash] = useState("");
  const [loading, setLoading] = useState(false);
  const [q, setQ] = useState("");
  const [visionOnly, setVisionOnly] = useState(false);
  const [picked, setPicked] = useState(""); // row selected as the next main model
  const [saving, setSaving] = useState(false);

  const load = useCallback(async () => {
    setLoading(true);
    setErr("");
    try {
      const res = await settingsClient.getSettings({});
      setEngine(res.engine ?? null);
      setModels(res.models);
      setEnv(res.env);
    } catch (e) {
      setErr(errText(e));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  useEffect(() => {
    if (!flash) return;
    const t = setTimeout(() => setFlash(""), 2500);
    return () => clearTimeout(t);
  }, [flash]);

  const live = engine?.model ?? "";
  const liveSub = engine?.subagentModel ?? "";

  // The knowledge base is queried by id, but a gateway may serve an id the
  // table has never heard of — say so instead of showing a blank card.
  const liveInfo = useMemo(
    () => models.find((m) => m.id.toLowerCase() === live.toLowerCase()) ?? null,
    [models, live],
  );

  const rows = useMemo(() => {
    const needle = q.trim().toLowerCase();
    return models.filter((m) => {
      if (visionOnly && !m.vision) return false;
      if (!needle) return true;
      return (
        m.id.toLowerCase().includes(needle) ||
        m.family.toLowerCase().includes(needle) ||
        m.provider.toLowerCase().includes(needle)
      );
    });
  }, [models, q, visionOnly]);

  const apply = async (model: string, sub?: string) => {
    if (!model.trim()) return;
    setSaving(true);
    setErr("");
    try {
      const res = await settingsClient.updateSettings({
        model,
        ...(sub !== undefined ? { subagentModel: sub } : {}),
      });
      setEngine(res.engine ?? null);
      setFlash(`model → ${model}`);
    } catch (e) {
      setErr(errText(e));
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="set-panel">
      <div className="set-head">
        <span className="set-stat">
          main <b>{live || "—"}</b>
          {liveSub && liveSub !== live && (
            <>
              {" "}· sub <b>{liveSub}</b>
            </>
          )}
        </span>
        <span className="set-flash">{flash}</span>
        <label className="set-check">
          <input
            type="checkbox"
            checked={visionOnly}
            onChange={(e) => setVisionOnly(e.target.checked)}
          />
          只看视觉
        </label>
        <input
          className="set-search"
          placeholder="搜索模型 id / 厂商…"
          value={q}
          onChange={(e) => setQ(e.target.value)}
        />
        <button className="ghost" disabled={loading} onClick={() => void load()}>
          {loading ? "…" : "刷新"}
        </button>
      </div>
      {err && <div className="set-err err">{err}</div>}

      {/* Current brain: the only card that is about THIS process. */}
      <div className="set-card">
        <div className="set-card-title">当前引擎</div>
        <div className="set-grid">
          <span>主模型</span>
          <span className="set-val">
            {live || "—"}
            {liveInfo?.vision && <span className="set-badge vision">视觉</span>}
          </span>
          <span>子代理模型</span>
          <span className="set-val">{liveSub || "—"}</span>
          <span>上下文窗口</span>
          <span className="set-val">
            {ctxText(engine?.contextWindow ?? 0n)}
            <span className="set-dim">
              {engine?.gaugeModelSource === "knowledge-base"
                ? " · 来自知识库"
                : " · 来自启动配置（知识库不认识该模型）"}
            </span>
          </span>
          <span>压缩阈值</span>
          <span className="set-val">{engine ? (engine.compactThreshold * 100).toFixed(0) : "—"}%</span>
          <span>effort</span>
          <span className="set-val">
            {engine?.effort || "—"} · sub {engine?.subagentEffort || "—"}
          </span>
          <span>max tokens</span>
          <span className="set-val">
            {fmtCount(Number(engine?.maxTokens ?? 0))} · sub{" "}
            {fmtCount(Number(engine?.subagentMaxTokens ?? 0))}
          </span>
          <span>每 prompt 迭代上限</span>
          <span className="set-val">
            {Number(engine?.maxIterations ?? 0) > 0 ? engine?.maxIterations : "不限"}
          </span>
          <span>web 工具</span>
          <span className="set-val">{engine?.webToolMode || "—"}</span>
          <span>OpenAI 网关</span>
          <span className="set-val">{engine?.openaiBase || "直连 Anthropic 协议"}</span>
          <span>审批桥</span>
          <span className="set-val">
            {engine?.approvalBridge ? "已启用" : "未启用（-approve 关闭时工具自动放行）"}
          </span>
          <span>并发 turn 上限</span>
          <span className="set-val">
            {Number(engine?.maxConcurrentTurns ?? 0) > 0
              ? engine?.maxConcurrentTurns
              : "不限"}
          </span>
          <span>shell 允许列表</span>
          <span className="set-val">
            {engine?.shellAllow?.length ? engine.shellAllow.join(", ") : "未启用"}
          </span>
          <span>工作区</span>
          <span className="set-val mono">{engine?.workspace || "—"}</span>
        </div>
        <div className="set-note">
          切换模型立即生效于下一轮请求（主循环、压缩、子代理与 cost gauge 一致）；
          单个 prompt 的 per-prompt override 仍然优先。lyond 不读取 config.toml，
          这里显示的是进程真实使用的配置。
        </div>
      </div>

      {/* Picker: the knowledge base, filterable. */}
      <div className="set-list-head">
        <span>模型知识库 {rows.length}/{models.length}</span>
        {picked && (
          <span className="set-picked">
            选中 <b>{picked}</b>
            <button
              disabled={saving || picked === live}
              onClick={() => void apply(picked)}
            >
              设为主模型
            </button>
            <button
              className="ghost"
              disabled={saving}
              onClick={() => void apply(live, picked)}
            >
              设为子代理
            </button>
            <button className="ghost" onClick={() => setPicked("")}>
              取消
            </button>
          </span>
        )}
      </div>
      <div className="set-rows">
        {rows.map((m) => {
          const isLive = m.id.toLowerCase() === live.toLowerCase();
          const isSub = m.id.toLowerCase() === liveSub.toLowerCase();
          const prof = m.profile;
          return (
            <div
              key={m.id}
              className={
                "set-row" + (isLive ? " live" : "") + (picked === m.id ? " picked" : "")
              }
              onClick={() => setPicked(m.id)}
              title={m.notes.join(" · ")}
            >
              <div className="set-row-main">
                <div className="set-row-id">
                  <span className="mono">{m.id}</span>
                  {isLive && <span className="set-badge live">主</span>}
                  {isSub && <span className="set-badge sub">子</span>}
                  {m.vision && <span className="set-badge vision">视觉</span>}
                  {m.reasoning && <span className="set-badge">推理</span>}
                </div>
                <div className="set-row-sub">
                  {m.family} · {m.provider}
                  {m.cutoff ? ` · 知识截止 ${m.cutoff}` : ""}
                </div>
              </div>
              <div className="set-row-num mono">{ctxText(m.context)}</div>
              <div className="set-row-num mono">{priceText(m)}</div>
              <div className="set-row-prof mono" title="推理 / 长跑 / 开放 / 速度 / 性价比">
                {prof ? scaleBars(prof.reasoning) : "—"}
              </div>
            </div>
          );
        })}
        {rows.length === 0 && <div className="placeholder">没有匹配的模型</div>}
      </div>

      {/* Environment inventory: read-only, credentials masked server-side. */}
      <div className="set-list-head">
        <span>环境变量（只读）</span>
        <span className="set-dim">启动即固定 — 改需重启 lyond</span>
      </div>
      <div className="set-rows">
        {env.map((e) => (
          <div key={e.name} className={"set-env" + (e.set ? " on" : "")}>
            <span className="mono set-env-name">{e.name}</span>
            <span className="set-env-val mono">
              {e.masked
                ? e.set
                  ? "已设置（已隐藏）"
                  : "未设置"
                : e.set
                  ? e.value || "已设置（空）"
                  : "未设置"}
            </span>
            <span className="set-env-note">{e.note}</span>
          </div>
        ))}
      </div>
    </div>
  );
}