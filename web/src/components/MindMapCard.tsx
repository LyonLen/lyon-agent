// MindMapCard — the chat-tail mindmap (docs/design/mindmap.md §7.1, M1).
// ChatView holds ONE revision-guarded snapshot (not a Block — every
// distill update would otherwise append another card) and re-renders
// this in place at the feed's tail. M1 is read-only + click-to-jump;
// the human edit channel (UpdateMindmap) is M2.
//
// Visual baseline: docs/design/mindmap-sample.html (light sheet),
// restyled onto the app's dark tokens. The §4.3 honesty rule rides on
// the borders: proposed = dashed gray (the model's guess), confirmed =
// solid branch color (yours) — a dishonest map is worse than none.
import { useEffect, useRef, useState } from "react";
import type { CSSProperties, ReactNode } from "react";
import type { MapNode, MindmapSnap } from "../mindmap";

// The sample's label-keyed palette. Real labels are dynamic, so hash
// the label into the same muted set — deterministic across renders and
// revisions, and white text reads on all of them.
const PALETTE = [
  "#2d6a4f", "#3a5a8c", "#9c4221", "#5b4b8a",
  "#a4262c", "#0f766e", "#b45309", "#475569",
];
const ICON: Record<string, string> = {
  branch: "▸",
  decision: "◆",
  fact: "●",
  open_question: "○",
};

function hueOf(label: string): string {
  let h = 0;
  for (let i = 0; i < label.length; i++) h = (h * 31 + label.charCodeAt(i)) >>> 0;
  return PALETTE[h % PALETTE.length];
}

export default function MindMapCard({
  snap,
  onJump,
}: {
  snap: MindmapSnap;
  onJump: (turn: number) => void;
}) {
  const [open, setOpen] = useState(true);
  const [fresh, setFresh] = useState<ReadonlySet<string>>(() => new Set());
  const [newCount, setNewCount] = useState(0);
  const prevIdsRef = useRef<Set<string> | null>(null);

  // Tombstones are deletions (negative signal) — out of the render and
  // out of every count.
  const alive = snap.nodes.filter((n) => !n.tombstone);
  const byParent = new Map<string, MapNode[]>();
  for (const n of alive) {
    const arr = byParent.get(n.parent_id);
    if (arr) arr.push(n);
    else byParent.set(n.parent_id, [n]);
  }
  const tops = byParent.get("") ?? [];
  const root = tops.find((r) => r.id === "root") ?? tops[0];

  // New-node highlight (§7.1): ids that appeared since the previous
  // snapshot. The first snapshot seeds the baseline (nothing is "new");
  // while collapsed the additions accumulate into the one-liner's +k
  // instead of flashing (the reader isn't looking).
  useEffect(() => {
    const ids = new Set(alive.map((n) => n.id));
    const prev = prevIdsRef.current;
    prevIdsRef.current = ids;
    if (!prev) return; // first snapshot: baseline
    const added = [...ids].filter((id) => !prev.has(id));
    if (added.length === 0) return;
    if (open) {
      setFresh(new Set(added));
      const t = setTimeout(() => setFresh(new Set()), 450);
      return () => clearTimeout(t);
    }
    setNewCount((c) => c + added.length);
  }, [snap, open]); // eslint-disable-line react-hooks/exhaustive-deps

  // No anchor (empty seat, everything tombstoned, LYON_MINDMAP=off never
  // seeded) → the card degrades to nothing, same contract as the
  // non-repo file card. Hooks above stay unconditional.
  if (!root) return null;

  const branches = [
    ...(byParent.get(root.id) ?? []),
    ...tops.filter((t) => t.id !== root.id),
  ];
  const confirmed = alive.filter((n) => n.state === "confirmed").length;
  const opened = alive.filter((n) => n.kind === "open_question").length;

  const countDesc = (id: string): number => {
    let n = 0;
    const walk = (pid: string) => {
      for (const c of byParent.get(pid) ?? []) {
        n++;
        walk(c.id);
      }
    };
    walk(id);
    return n;
  };

  // One node row; children nest under a guide line (the server's depth
  // cap is 4 — root 0, branch 1, so rel 0..2 under a branch; the rel<3
  // guard is belt-and-braces against a runaway tree).
  const renderNode = (n: MapNode, rel: number): ReactNode => {
    const conf = n.state === "confirmed";
    const cls = [
      "mm-node",
      n.kind,
      conf ? "confirmed" : "",
      n.status === "parked" ? "parked" : "",
      fresh.has(n.id) ? "fresh" : "",
    ]
      .filter(Boolean)
      .join(" ");
    const kids = byParent.get(n.id) ?? [];
    return (
      <div className="mm-node-wrap" key={n.id}>
        <div
          className={cls}
          onClick={() => onJump(n.source_turn)}
          title={`${n.kind} · 出自第 ${n.source_turn} 轮 · ${conf ? "已确认" : "待你确认"}${n.status === "parked" ? " · 停靠" : ""} — 点击跳回原话`}
        >
          <span className="mm-i">{ICON[n.kind] ?? "·"}</span>
          {n.label}
          {(n.pinned || n.edited_by_user) && (
            <span className="mm-pen" title="人工改过">✎</span>
          )}
          <span className="mm-t">t{n.source_turn}</span>
        </div>
        {kids.length > 0 && rel < 3 && (
          <div className="mm-kids">
            {kids.map((k) => renderNode(k, rel + 1))}
          </div>
        )}
      </div>
    );
  };

  const toggle = () => {
    setNewCount(0);
    setFresh(new Set());
    setOpen((o) => !o);
  };

  const title = snap.title || "对话实时思维导图";
  const sub = snap.subtitle;
  const cut = sub.lastIndexOf(" · ");

  return (
    <div className="mindmap-card">
      <div
        className="mm-head"
        role="button"
        tabIndex={0}
        aria-expanded={open}
        onClick={toggle}
        onKeyDown={(e) => {
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            toggle();
          }
        }}
      >
        <span className="mm-glyph" aria-hidden="true">🧠</span>
        {open ? (
          <div className="mm-titles">
            <div className="mm-title">{title}</div>
            {sub && (
              <div className="mm-sub">
                {cut > 0 ? (
                  <>
                    {sub.slice(0, cut + 3)}
                    <b>{sub.slice(cut + 3)}</b>
                  </>
                ) : (
                  sub
                )}
              </div>
            )}
          </div>
        ) : (
          <div className="mm-oneline">
            <b>{branches.length}</b> 个分支
            {newCount > 0 && (
              <>
                {" "}· 最近 <b>+{newCount}</b>
              </>
            )}
          </div>
        )}
        {open && (
          <div className="mm-chips">
            <span className="mm-chip">
              <b>{alive.length}</b> 节点
            </span>
            <span className="mm-chip">
              <b>{branches.length}</b> 分支
            </span>
            <span className="mm-chip">
              <b>{confirmed}</b> 已确认
            </span>
            <span className="mm-chip warn">
              <b>{alive.length - confirmed}</b> 待你确认
            </span>
            {opened > 0 && (
              <span className="mm-chip warn">
                <b>{opened}</b> 未决
              </span>
            )}
            <span className="mm-chip">
              rev <b>{snap.revision}</b>
            </span>
          </div>
        )}
        <span className="mm-chev" aria-hidden="true">
          {open ? "▾" : "▸"}
        </span>
      </div>
      {open && (
        <>
          <div className="mm-legend">
            <span className="mm-lg">
              <span className="mm-sw conf" />
              确认态 — 你认定的 / 你亲手改过的
            </span>
            <span className="mm-lg">
              <span className="mm-sw prop" />
              提案态 — 模型猜的，待你确认
            </span>
            <span className="mm-kinds">
              <span>
                <span className="mm-k">▸</span> 分支
              </span>
              <span>
                <span className="mm-k">◆</span> 决策
              </span>
              <span>
                <span className="mm-k">●</span> 事实
              </span>
              <span>
                <span className="mm-k">○</span> 未决
              </span>
              <span>t# = 出自第几轮</span>
            </span>
          </div>

          <div className="mm-spine">
            <div
              className="mm-root"
              onClick={() => onJump(root.source_turn)}
              title={`出自第 ${root.source_turn} 轮 — 点击跳回`}
            >
              <span className="mm-rl">{root.label}</span>
              <span className="mm-rt">
                t{root.source_turn} · {alive.length} 节点
              </span>
            </div>
            <div className="mm-trunk" />
          </div>

          <div className="mm-cols">
            {branches.length === 0 && (
              <div className="mm-empty">还没有分支 — 蒸馏还在攒</div>
            )}
            {branches.map((br) => {
              const hue = hueOf(br.label);
              const kids = byParent.get(br.id) ?? [];
              return (
                <div
                  className="mm-col"
                  key={br.id}
                  style={{ "--b": hue, "--dot": hue } as CSSProperties}
                >
                  <div
                    className={
                      "mm-col-head" +
                      (fresh.has(br.id) ? " fresh" : "") +
                      (br.status === "parked" ? " parked" : "")
                    }
                    onClick={() => onJump(br.source_turn)}
                    title={`${br.label} · 出自第 ${br.source_turn} 轮 — 点击跳回`}
                  >
                    <span>{br.label}</span>
                    <span className="n">{countDesc(br.id)}</span>
                  </div>
                  {kids.length === 0 ? (
                    <div className="mm-empty">—</div>
                  ) : (
                    kids.map((k) => renderNode(k, 0))
                  )}
                </div>
              );
            })}
          </div>

          <div className="mm-foot">
            <span>
              <b>读法</b>：这不是结论，是 lyon 对你这句话的理解。可怀疑是它的本分。
            </span>
            <span>
              <b>生成</b>：旁路蒸馏 · 关掉无痕
            </span>
            <span>
              <b>点节点 / t#</b> 跳回原话
            </span>
          </div>
        </>
      )}
    </div>
  );
}
