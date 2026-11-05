// MindMapCard — the session mindmap (docs/design/mindmap.md §7.1, M1+M2+
// M2.5+M3). ChatView holds ONE revision-guarded snapshot (not a Block —
// every distill update would otherwise append another card) and reports
// it to App, which renders this in the `mindmap` pane — a peer of chat
// in the tiling workspace (#51), not a chat-tail card. Click = jump to
// source turn; M2 adds the human edit channel — double-click renames
// inline, hover actions confirm / park / delete, the header confirms
// all — all through UpdateMindmap (origin="user": §4.1 full authority,
// touched nodes pin; the mindmap_edit event round-trips the result).
//
// §7.3 selection (M2): marquee on the blank canvas (Shift unions, a
// <4px drag clears), ctrl/cmd-click toggles, Esc clears — ids live in
// App so the chat composer's hint row and this pane share one truth.
// The toolbar DOCKS as a layout row above the canvas (Image #55: the
// floating version occluded map nodes): local verbs (合并/归类到/
// 停靠/删除) ride origin="user" applyOps; semantic verbs (改写/
// 矛盾吗/解释/提问) compose the marked prompt through onAsk — App
// seq-bumps it into ChatView's selAsk, the only submission path.
//
// Canvas gestures: middle-button (wheel press) drag pans from anywhere
// (node pills included — they own only left-clicks); ctrl+wheel zooms
// around the cursor (dbl-click blank resets to the auto fit).
//
// Layout: single-direction tree (user directive, Image #53) — root
// bubble at the LEFT, every branch growing rightward in original order,
// bezier curves out to content-sized child rows. (Earlier radial
// two-sided split confused peers; the ref's curve language — hues,
// joint dots, pill branches — stays.) Labels are NEVER ellipsized
// (Image #56): rows take natural width and each depth column advances
// by the measured max of the previous column — one measure pass feeds
// both the column x positions and the curve anchors.
//
// Visual baseline: docs/design/mindmap-sample.html (light sheet),
// restyled onto the app's dark tokens. The §4.3 honesty rule rides on
// the borders: proposed = dashed gray (the model's guess), confirmed =
// solid branch color (yours) — a dishonest map is worse than none.
import { useEffect, useLayoutEffect, useRef, useState } from "react";
import type { CSSProperties, KeyboardEvent, MouseEvent, ReactNode } from "react";
import { mindmapClient, v1Json } from "../api";
import type { MapNode, MindmapSnap } from "../mindmap";

// One UpdateMindmap op — pkg/mindmap Op's JSON shape (§7.3 vocabulary:
// rewrite / park / delete / confirm / set_parent / merge / add).
type UserOp = {
  op: string;
  id: string;
  label?: string;
  parent_id?: string;
  status?: string;
  state?: string;
  source_ids?: string[];
};

// §10 M2.5 — POST /v1/mindmap/{id}/extract answers this on both modes;
// dry_run probes eligibility, the real write upserts 导图-* facts.
type ExtractRes = {
  eligible: boolean;
  written: number;
  facts: { scope: string; name: string; description: string }[];
};

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

// ── radial geometry (CSS px; PILL_W/TEXT_W/TEXT_W3 are first-pass
// estimates — the measure pass replaces them with per-column maxima) ──
const PILL_H = 30; // branch pill row
const ROW_H = 22; // child text row
const SIB_GAP = 6; // between siblings
const ROOT_GAP = 20; // between branch groups on one side
const CURVE_ROOT_MIN = 54; // root → branch run floor; grows with the root bubble
const PILL_W = 160; // depth-1 column estimate
const CURVE_N = 30; // parent → child horizontal run
const TEXT_W = 150; // depth-2 column estimate
const TEXT_W3 = 135; // depth-3 column estimate
const MAX_DEPTH = 3; // render children while depth < 3 (root=0 … L3)
const JOINT_GAP = 4; // curve end pulls back so the joint dot misses glyphs

type Side = 1 | -1; // 1 = right of the root, -1 = left

type Pos = {
  node: MapNode;
  depth: number; // 1 = branch, 2/3 = children
  side: Side;
  top: number; // box top (canvas coords)
  cy: number; // vertical center — curve anchor
  hue: string; // subtree shares the branch's hue (ref behavior)
};

export default function MindMapCard({
  snap,
  sessionId,
  onJump,
  onApplied,
  onResync,
  selected,
  onSelect,
  onAsk,
  agentBatches,
}: {
  snap: MindmapSnap;
  sessionId: string;
  onJump: (turn: number) => void;
  /** The UpdateMindmap response — fresh revision + nodes for ChatView's
   *  guard (the live mindmap_edit event dedups by revision). */
  onApplied?: (rev: number, nodesJson: string) => void;
  /** §5 conflict / transport failure — ChatView refetches the map. */
  onResync?: () => void;
  /** §7.3 selection ids — owned by App (composer hint + this pane share
   *  one truth); null/[] = no selection, toolbar hidden. */
  selected?: string[] | null;
  onSelect?: (ids: string[] | null) => void;
  /** Semantic verbs compose the marked prompt; App seq-bumps it into
   *  ChatView's selAsk (the single submission path). */
  onAsk?: (text: string) => void;
  /** A5: agent-origin batches whose proposed nodes are still pending —
   *  at ≥3 the toolbar arms local verbs behind a second click. */
  agentBatches?: number;
}) {
  const [open, setOpen] = useState(true);
  const [fresh, setFresh] = useState<ReadonlySet<string>>(() => new Set());
  const [newCount, setNewCount] = useState(0);
  // Inline rename (double-click): the id being edited and its draft.
  const [editing, setEditing] = useState<{ id: string; value: string } | null>(null);
  const prevIdsRef = useRef<Set<string> | null>(null);
  // Measured text widths — curves start at the text's inner edge, and
  // that edge depends on content width. One pass after each layout.
  const [widths, setWidths] = useState<Record<string, number>>({});
  // Measured root width — the root bubble must not swallow the first
  // branch column (the live screenshot: a long title overlapped pills
  // on both sides), so the root→branch run grows with the bubble.
  const [rootW, setRootW] = useState(1); // 0 is the "not measured yet" floor
  // Scale-to-fit: a 4-pane layout gives this pane ~600px while a real
  // map wants ~900 — without shrinking, the outer children clip and
  // the curves dangle. Floor at 0.7 keeps text readable; below that we
  // keep the scale and scroll instead.
  const [fit, setFit] = useState(1);
  // Manual zoom (ctrl+wheel): null = follow the auto fit. Once set it
  // overrides the auto scale until dbl-click blank resets it.
  const [zoom, setZoom] = useState<number | null>(null);
  const scale = zoom ?? fit;
  const scaleRef = useRef(scale);
  scaleRef.current = scale; // live value for the wheel handler
  const [panning, setPanning] = useState(false);
  const canvasRef = useRef<HTMLDivElement>(null);
  const centeredRef = useRef(false);
  // Canvas pixel box — the docked toolbar and pane resizes change it,
  // and the fit pass below must re-run when they do.
  const [canvasBox, setCanvasBox] = useState({ w: 0, h: 0 });
  useEffect(() => {
    const el = canvasRef.current;
    if (!el) return;
    const ro = new ResizeObserver(() => {
      const w = el.clientWidth;
      const h = el.clientHeight;
      setCanvasBox((b) => (b.w === w && b.h === h ? b : { w, h }));
    });
    ro.observe(el);
    return () => ro.disconnect();
  }, [open]);

  // ── §7.3 selection ────────────────────────────────────────────────
  // Ids live in App; the card owns the marquee draft, the free-input
  // line and the A5 arm.
  const sel = selected ?? [];
  const [marquee, setMarquee] = useState<{
    x: number;
    y: number;
    w: number;
    h: number;
  } | null>(null);
  const dragRef = useRef<{ x0: number; y0: number; shift: boolean } | null>(null);
  const planeRef = useRef<HTMLDivElement>(null);
  const [tbQ, setTbQ] = useState("");
  const [armed, setArmed] = useState(false); // A5 second-click gate
  useEffect(() => {
    if ((agentBatches ?? 0) < 3) setArmed(false);
  }, [agentBatches]);
  // §10 M2.5 — probe eligibility on every revision (dry run): the
  // banner/chip appear only when the map has ≥4 confirmed decision|fact
  // rows and no 导图-* fact yet (server-side gates, extract.go). The
  // write upserts through Store.Save — same seam as save_memory.
  const [mem, setMem] = useState<ExtractRes | null>(null);
  const [memNote, setMemNote] = useState("");
  useEffect(() => {
    if (!open) return;
    let dead = false;
    v1Json<ExtractRes>(`/v1/mindmap/${sessionId}/extract`, { dry_run: true })
      .then((j) => {
        if (!dead) setMem(j);
      })
      .catch(() => {}); // route absent/offline — the affordance stays hidden
    return () => {
      dead = true;
    };
  }, [sessionId, snap.revision, open]);
  const memTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const saveMemory = () => {
    void v1Json<ExtractRes>(`/v1/mindmap/${sessionId}/extract`, {
      dry_run: false,
    })
      .then((j) => {
        setMem({ ...j, eligible: false });
        setMemNote(`已沉淀 ${j.written} 条记忆 ✓`);
        if (memTimer.current) clearTimeout(memTimer.current);
        memTimer.current = setTimeout(() => setMemNote(""), 6000);
      })
      .catch((e) => {
        setMemNote(`沉淀失败 — ${e instanceof Error ? e.message : String(e)}`);
        if (memTimer.current) clearTimeout(memTimer.current);
        memTimer.current = setTimeout(() => setMemNote(""), 6000);
      });
  };

  // Screen → plane coords (the plane carries scale(scale)).
  const toPlane = (cx: number, cy: number) => {
    const r = planeRef.current?.getBoundingClientRect();
    if (!r) return { x: 0, y: 0 };
    return { x: (cx - r.left) / scale, y: (cy - r.top) / scale };
  };

  // Marquee drag: starts on blank canvas (node mouses are filtered by
  // the closest() guard below), tracks on the window so the drag
  // survives leaving the pane, collects by rect-intersect on release.
  const canvasDown = (e: MouseEvent<HTMLDivElement>) => {
    // Middle button (wheel press) pans the canvas — grab from anywhere,
    // node pills included (they own only left-clicks). preventDefault
    // kills the browser's autoscroll mode.
    if (e.button === 1) {
      e.preventDefault();
      const cv = e.currentTarget;
      const sx = e.clientX;
      const sy = e.clientY;
      const sl = cv.scrollLeft;
      const st = cv.scrollTop;
      setPanning(true);
      const move = (ev: WindowEventMap["mousemove"]) => {
        cv.scrollLeft = sl - (ev.clientX - sx);
        cv.scrollTop = st - (ev.clientY - sy);
      };
      const up = () => {
        setPanning(false);
        window.removeEventListener("mousemove", move);
        window.removeEventListener("mouseup", up);
      };
      window.addEventListener("mousemove", move);
      window.addEventListener("mouseup", up);
      return;
    }
    if (e.button !== 0) return;
    const t = e.target as HTMLElement;
    if (t.closest("[data-mm-id]")) return; // node owns its own click
    e.preventDefault(); // no text selection while dragging
    const p = toPlane(e.clientX, e.clientY);
    dragRef.current = { x0: p.x, y0: p.y, shift: e.shiftKey };
    setMarquee({ x: p.x, y: p.y, w: 0, h: 0 });
  };
  useEffect(() => {
    const move = (e: WindowEventMap["mousemove"]) => {
      const d = dragRef.current;
      if (!d) return;
      const p = toPlane(e.clientX, e.clientY);
      setMarquee({
        x: Math.min(d.x0, p.x),
        y: Math.min(d.y0, p.y),
        w: Math.abs(p.x - d.x0),
        h: Math.abs(p.y - d.y0),
      });
    };
    const up = (e: WindowEventMap["mouseup"]) => {
      const d = dragRef.current;
      if (!d) return;
      dragRef.current = null;
      const p = toPlane(e.clientX, e.clientY);
      const r = {
        x: Math.min(d.x0, p.x),
        y: Math.min(d.y0, p.y),
        w: Math.abs(p.x - d.x0),
        h: Math.abs(p.y - d.y0),
      };
      setMarquee(null);
      // A click, not a drag: blank-click clears the selection (§7.3).
      if (r.w < 4 && r.h < 4) {
        onSelect?.(null);
        return;
      }
      const plane = planeRef.current;
      if (!plane) return;
      const pr = plane.getBoundingClientRect();
      const hits: string[] = [];
      for (const el of plane.querySelectorAll<HTMLElement>("[data-mm-id]")) {
        const er = el.getBoundingClientRect();
        const ex = (er.left - pr.left) / scale;
        const ey = (er.top - pr.top) / scale;
        const ew = er.width / scale;
        const eh = er.height / scale;
        if (
          ex < r.x + r.w &&
          ex + ew > r.x &&
          ey < r.y + r.h &&
          ey + eh > r.y
        ) {
          const id = el.dataset.mmId;
          if (id && !hits.includes(id)) hits.push(id);
        }
      }
      // Shift unions with the standing selection; a plain drag replaces.
      onSelect?.(
        d.shift ? Array.from(new Set([...sel, ...hits])) : hits,
      );
    };
    window.addEventListener("mousemove", move);
    window.addEventListener("mouseup", up);
    return () => {
      window.removeEventListener("mousemove", move);
      window.removeEventListener("mouseup", up);
    };
  }, [sel, scale, onSelect]); // eslint-disable-line react-hooks/exhaustive-deps

  // Esc clears — but never while typing (the inline rename input and
  // the toolbar's free-input own their own Esc via stopPropagation).
  useEffect(() => {
    if (sel.length === 0) return;
    const h = (e: WindowEventMap["keydown"]) => {
      if (e.key !== "Escape") return;
      const t = e.target as HTMLElement | null;
      if (
        t &&
        (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.isContentEditable)
      )
        return;
      onSelect?.(null);
    };
    window.addEventListener("keydown", h);
    return () => window.removeEventListener("keydown", h);
  }, [sel.length, onSelect]);

  // ctrl+wheel zoom: scales around the cursor (the scroll offsets are
  // rebased so the point under the pointer stays put); plain wheel keeps
  // native scroll. Native listener with passive:false — React's onWheel
  // can't reliably preventDefault the browser's page zoom.
  useEffect(() => {
    const el = canvasRef.current;
    if (!el) return;
    const onWheel = (ev: WheelEvent) => {
      if (!ev.ctrlKey && !ev.metaKey) return;
      ev.preventDefault();
      ev.stopPropagation();
      const prev = scaleRef.current;
      const next = Math.min(
        2.5,
        Math.max(0.4, prev * (ev.deltaY < 0 ? 1.1 : 1 / 1.1)),
      );
      if (next === prev) return;
      const r = el.getBoundingClientRect();
      const cx = ev.clientX - r.left;
      const cy = ev.clientY - r.top;
      el.scrollLeft = ((el.scrollLeft + cx) * next) / prev - cx;
      el.scrollTop = ((el.scrollTop + cy) * next) / prev - cy;
      setZoom(next);
    };
    el.addEventListener("wheel", onWheel, { passive: false });
    return () => el.removeEventListener("wheel", onWheel);
  }, [open]);

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

  const branches = root
    ? [...(byParent.get(root.id) ?? []), ...tops.filter((t) => t.id !== root.id)]
    : [];
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

  // Root→branch clearance grows with the measured root bubble — a long
  // title must not swallow the first pills on either side (live shot:
  // the white root covered "BACKLOG 四…"). First pass runs at the floor;
  // the measure pass below corrects it before the map settles.
  const curveRoot = Math.max(CURVE_ROOT_MIN, Math.ceil(rootW / 2) + 18);

  // ── layout ────────────────────────────────────────────────────────
  const own = (depth: number) => (depth === 1 ? PILL_H : ROW_H);
  const kidsOf = (n: MapNode, depth: number): MapNode[] =>
    depth < MAX_DEPTH ? byParent.get(n.id) ?? [] : [];

  const subH = (n: MapNode, depth: number): number => {
    const kids = kidsOf(n, depth);
    const o = own(depth);
    if (kids.length === 0) return o;
    let block = 0;
    kids.forEach((k, i) => {
      block += subH(k, depth + 1);
      if (i > 0) block += SIB_GAP;
    });
    return Math.max(o, block);
  };

  const pos: Pos[] = [];
  const place = (n: MapNode, top: number, depth: number, side: Side, hue: string) => {
    const H = subH(n, depth);
    const cy = top + H / 2;
    pos.push({ node: n, depth, side, top: cy - own(depth) / 2, cy, hue });
    const kids = kidsOf(n, depth);
    if (kids.length === 0) return;
    let block = 0;
    kids.forEach((k, i) => {
      block += subH(k, depth + 1);
      if (i > 0) block += SIB_GAP;
    });
    let ky = top + (H - block) / 2;
    kids.forEach((k) => {
      place(k, ky, depth + 1, side, hue);
      ky += subH(k, depth + 1) + SIB_GAP;
    });
  };

  // Single direction (user directive, Image #53): every branch grows
  // RIGHT of the root, stacked top-to-bottom in original order. The
  // two-sided split read as chaos, and the right-aligned left-side rows
  // hid the fact that siblings are peers — one direction, one alignment.
  const groups = branches.map((b, i) => ({ b, h: subH(b, 1), i }));
  const stackH = (arr: typeof groups) =>
    arr.reduce((a, g) => a + g.h, 0) + (arr.length > 1 ? ROOT_GAP * (arr.length - 1) : 0);
  const totalH = stackH(groups);

  // The root bubble hangs LEFT of CX — park it clear of the plane edge.
  const CX = Math.ceil(rootW / 2) + 16;
  const H = Math.max(totalH, ROW_H) + 56;
  const CY = H / 2;

  let y = CY - totalH / 2;
  groups.forEach((g) => {
    place(g.b, y, 1, 1, hueOf(g.b.label));
    y += g.h + ROOT_GAP;
  });

  // Columns are content-width (Image #56: no ellipsis anywhere): each
  // depth's x advances by the MEASURED max of the previous column, so
  // a long label widens the plane instead of clipping. The first pass
  // has no measurements yet — fall back to the estimate constants; the
  // measure pass below re-runs this with real widths until it settles.
  const colW = (depth: number, est: number) => {
    let m = est;
    for (const p of pos) {
      if (p.depth === depth) m = Math.max(m, widths[p.node.id] ?? 0);
    }
    return m;
  };
  const CW1 = colW(1, PILL_W);
  const CW2 = colW(2, TEXT_W);
  const CW3 = colW(3, TEXT_W3);
  const has2 = pos.some((p) => p.depth === 2);
  const has3 = pos.some((p) => p.depth === 3);
  let e = curveRoot + CW1;
  if (has2) e += CURVE_N + CW2;
  if (has3) e += CURVE_N + CW3;
  const W = CX + e + 12;

  // Box x for a placed node (left edge, canvas coords).
  const xOf = (p: Pos): number => {
    const s = p.side;
    if (p.depth === 1) return s === 1 ? CX + curveRoot : CX - curveRoot - CW1;
    if (p.depth === 2)
      return s === 1
        ? CX + curveRoot + CW1 + CURVE_N
        : CX - curveRoot - CW1 - CURVE_N - CW2;
    return s === 1
      ? CX + curveRoot + CW1 + CURVE_N + CW2 + CURVE_N
      : CX - curveRoot - CW1 - CURVE_N - CW2 - CURVE_N - CW3;
  };
  const slotW = (p: Pos) => (p.depth === 1 ? CW1 : p.depth === 2 ? CW2 : CW3);

  // Measure pass: text widths anchor the curves' outer ends, and the
  // root bubble width sets the root→branch clearance. offsetWidth is
  // read once per layout; positions are CSS-fixed so nothing reflows.
  // While measurements are still moving the geometry shifts next commit
  // — centering waits until a pass finds nothing changed, so the root
  // lands centered with the FINAL clearance (not the floor pass).
  useLayoutEffect(() => {
    if (!open) return;
    const canvas = canvasRef.current;
    if (!canvas) return;
    const next: Record<string, number> = {};
    for (const el of canvas.querySelectorAll<HTMLElement>("[data-mm-id]")) {
      next[el.dataset.mmId!] = el.offsetWidth;
    }
    const nextRootW = canvas.querySelector<HTMLElement>(".mm-croot")?.offsetWidth ?? 0;
    let widthsChanged = Object.keys(widths).length !== Object.keys(next).length;
    if (!widthsChanged) {
      for (const k of Object.keys(next)) {
        if (widths[k] !== next[k]) { widthsChanged = true; break; }
      }
    }
    const rootChanged = nextRootW !== rootW;
    // Fit ratio against the canvas, clamped to [0.7, 1] — see `fit`.
    const nextFit = Math.min(
      1,
      Math.max(
        0.7,
        Math.min(
          (canvas.clientWidth - 20) / Math.max(1, W),
          (canvas.clientHeight - 20) / Math.max(1, H),
        ),
      ),
    );
    const fitChanged = nextFit !== fit;
    if (widthsChanged || rootChanged || fitChanged) {
      if (widthsChanged) setWidths(next);
      if (rootChanged) setRootW(nextRootW);
      if (fitChanged) setFit(nextFit);
      return; // geometry shifts next commit — center after it settles
    }
    if (!centeredRef.current) {
      centeredRef.current = true;
      // When the scaled sizer fits, margin:auto already centers it and
      // these scroll to 0; the floor-clamped case pans to the root.
      canvas.scrollLeft = Math.max(0, CX * scale - canvas.clientWidth / 2);
      canvas.scrollTop = Math.max(0, CY * scale - canvas.clientHeight / 2);
    }
  }, [open, pos.length, snap.revision, rootW, widths, fit, canvasBox, scale]); // eslint-disable-line react-hooks/exhaustive-deps

  // ── curves ────────────────────────────────────────────────────────
  const wOf = (id: string, fallback: number) => widths[id] ?? fallback;
  const paths: { d: string; hue: string; w: number; joint?: { x: number; y: number; hue: string } }[] = [];
  const rootKids = pos.filter((p) => p.depth === 1);
  for (const p of rootKids) {
    const sx = CX + p.side * 8;
    const ex = p.side === 1 ? CX + curveRoot : CX - curveRoot;
    paths.push({
      d: `M ${sx} ${CY} C ${sx + p.side * 34} ${CY}, ${ex - p.side * 34} ${p.cy}, ${ex} ${p.cy}`,
      hue: p.hue,
      w: 2.4,
    });
  }
  for (const p of pos) {
    if (p.depth < 1 || p.depth >= MAX_DEPTH) continue;
    const kids = pos.filter(
      (k) =>
        k.depth === p.depth + 1 &&
        k.side === p.side &&
        k.node.parent_id === p.node.id,
    );
    if (kids.length === 0) continue;
    // Parent's outer edge: its measured content width (pills are
    // content-width too — Image #56 killed the fixed slot).
    const px0 = xOf(p);
    const pw = wOf(p.node.id, slotW(p));
    const sx = p.side === 1 ? px0 + pw : px0 + slotW(p) - pw;
    for (const k of kids) {
      const kx0 = xOf(k);
      // The child's inner edge is its anchor (column left edge), so
      // pull the curve end JOINT_GAP back toward the parent — the joint
      // dot lands beside the text, not on its first/last glyph (the
      // live shot read t2 as "t:○").
      const inner = p.side === 1 ? kx0 : kx0 + slotW(k);
      const ex = inner - p.side * JOINT_GAP;
      paths.push({
        d: `M ${sx} ${p.cy} C ${sx + p.side * 18} ${p.cy}, ${ex - p.side * 18} ${k.cy}, ${ex} ${k.cy}`,
        hue: k.hue,
        w: p.depth === 1 ? 1.7 : 1.4,
        joint: { x: ex, y: k.cy, hue: k.hue },
      });
    }
  }

  // M3 `relate` (§9): dashed cross-branch edges — a curved arc over the
  // tree between the two node centers. Only drawn when BOTH ends are
  // rendered (the depth gate hides deeper nodes; an invisible target
  // draws nothing rather than a line to nowhere).
  const rels: { d: string }[] = [];
  {
    const posById = new Map(pos.map((p) => [p.node.id, p]));
    for (const p of pos) {
      const t = p.node.relation?.target;
      if (!t) continue;
      const tp = posById.get(t);
      if (!tp) continue;
      const px = xOf(p) + wOf(p.node.id, slotW(p)) / 2;
      const tx = xOf(tp) + wOf(tp.node.id, slotW(tp)) / 2;
      const lift = Math.min(p.cy, tp.cy) - 22;
      rels.push({
        d: `M ${px} ${p.cy} Q ${(px + tx) / 2} ${lift} ${tx} ${tp.cy}`,
      });
    }
  }

  const toggle = () => {
    setNewCount(0);
    setFresh(new Set());
    centeredRef.current = false; // re-expand re-centers on the root
    setOpen((o) => !o);
  };

  // ── M2 edit channel (§4.1 / §7.1) ─────────────────────────────────
  // Every human action is one UpdateMindmap batch, origin="user": the
  // server grants full authority, pins what it touches, bumps
  // HumanRevision (蒸馏 then loses any racing diff — 人工优先), and
  // injects mindmap_edit back through the hub. The response applies
  // through onApplied under ChatView's revision guard (the event
  // dedups); a FailedPrecondition (§5 gap with a human edit) refetches.
  const applyOps = async (ops: UserOp[]) => {
    if (ops.length === 0) return;
    try {
      const res = await mindmapClient.updateMindmap({
        sessionId,
        revision: snap.revision,
        origin: "user",
        opsJson: JSON.stringify(ops),
      });
      onApplied?.(Number(res.revision), res.nodesJson);
    } catch {
      onResync?.();
    }
  };
  const startEdit = (n: MapNode) => (e: MouseEvent) => {
    e.stopPropagation();
    setEditing({ id: n.id, value: n.label });
  };
  // Commit OUTSIDE the state updater: React StrictMode double-invokes
  // functional updaters in dev, and a side effect in there fired the
  // RPC twice (second call → revision conflict 400). Enter also unmounts
  // the input (blur can follow), so a consume-once ref makes the second
  // call a no-op instead of a duplicate batch.
  const commitLock = useRef(false);
  const commitEdit = () => {
    if (commitLock.current) return;
    const ed = editing;
    if (!ed) return;
    commitLock.current = true;
    setEditing(null);
    const v = ed.value.trim();
    const cur = snap.nodes.find((n) => n.id === ed.id);
    if (v && cur && v !== cur.label) void applyOps([{ op: "rewrite", id: ed.id, label: v }]);
    setTimeout(() => {
      commitLock.current = false;
    }, 0);
  };
  const actClick = (fn: () => void) => (e: MouseEvent) => {
    e.stopPropagation(); // actions live inside the jump/expand targets
    fn();
  };
  const toggleConfirm = (n: MapNode) =>
    actClick(() =>
      void applyOps([
        { op: "confirm", id: n.id, state: n.state === "confirmed" ? "proposed" : "confirmed" },
      ]),
    );
  const togglePark = (n: MapNode) =>
    actClick(() =>
      void applyOps([
        { op: "park", id: n.id, status: n.status === "parked" ? "active" : "parked" },
      ]),
    );
  const removeNode = (n: MapNode) =>
    actClick(() => void applyOps([{ op: "delete", id: n.id }]));

  // ── §7.3 selection verbs ──────────────────────────────────────────
  // Local verbs are origin="user" batches through the same channel as
  // the hover acts; the root bubble is never a target (it isn't a
  // movable node). A5: while ≥3 agent batches sit unconfirmed, the
  // first click arms, the second executes.
  const selIds = sel.filter((id) => id !== root?.id);
  const guard =
    (fn: () => void) =>
    () => {
      if ((agentBatches ?? 0) >= 3 && !armed) {
        setArmed(true);
        return;
      }
      setArmed(false);
      fn();
    };
  const mergeSel = () => {
    if (selIds.length < 2) return;
    // A source that is an ANCESTOR of the survivor would swallow it —
    // drop those; descendants folding up are the normal case.
    const byId = new Map(snap.nodes.map((n) => [n.id, n]));
    const isAnc = (a: string, b: string) => {
      let cur = byId.get(b);
      let hops = 0;
      while (cur && cur.parent_id && hops++ < 64) {
        if (cur.parent_id === a) return true;
        cur = byId.get(cur.parent_id);
      }
      return false;
    };
    const sources = selIds.slice(1).filter((s) => !isAnc(s, selIds[0]));
    if (sources.length === 0) return;
    void applyOps([{ op: "merge", id: selIds[0], source_ids: sources }]);
  };
  // 归类到… candidates: alive, not selected, and not DESCEDANTS of the
  // selection (re-parenting under your own child is a cycle — the
  // server would reject the whole batch).
  const selSet = new Set(sel);
  const descOfSel = new Set<string>();
  {
    const walk = (pid: string) => {
      for (const c of byParent.get(pid) ?? []) {
        if (!descOfSel.has(c.id)) {
          descOfSel.add(c.id);
          walk(c.id);
        }
      }
    };
    selSet.forEach((id) => walk(id));
  }
  const reparentCands = alive.filter(
    (n) => n.id !== root?.id && !selSet.has(n.id) && !descOfSel.has(n.id),
  );
  const reparent = (parentId: string) => {
    if (selIds.length === 0) return;
    void applyOps(
      selIds.map((id) => ({ op: "set_parent", id, parent_id: parentId })),
    );
  };
  const parkSel = () => {
    if (selIds.length === 0) return;
    void applyOps(
      selIds.map((id) => ({
        op: "park",
        id,
        status:
          snap.nodes.find((n) => n.id === id)?.status === "parked"
            ? "active"
            : "parked",
      })),
    );
  };
  const delSel = () => {
    if (selIds.length === 0) return;
    void applyOps(selIds.map((id) => ({ op: "delete", id })));
  };
  // Semantic verbs: the marker carries the ids (ChatView strips them
  // for display and renders label chips), the discipline suffix tells
  // the model how a selection-ask may land on the map (§7.3 — prose
  // never mutates; map_ops rides one fenced block or not at all).
  const SEL_DISCIPLINE =
    "【系统 · 选区提问纪律】你在回答一个「基于导图选区」的提问，选区节点 id 在上一行方括号里。规则：\n" +
    "1. 需要改图时：只输出一个 ```map_ops 代码块，内容是 JSON 数组（每项带 op 字段，可用 rewrite/set_parent/add/park/delete/confirm/merge），代码块之外不写任何改动指令。\n" +
    "2. 不需要改图时：直接散文回答，不要输出 map_ops 代码块。\n" +
    "3. 改动落在选区上：rewrite/confirm/park/delete/merge/set_parent 只针对方括号里的选区 id；add 的新节点挂在选区节点下、提案态。\n" +
    "4. 选区外的既有节点一律不动；已确认/已固定节点不要改（服务器会整批拒绝）。";
  const askSel = (q: string, expectMap = false) => {
    if (sel.length === 0) return;
    const question = q.trim() || "这些节点讲了什么？彼此是什么关系？";
    // 【期望落图】 rides only verbs that must mutate (改写): ChatView's
    // reply parser uses it to decide whether a prose-only answer earns
    // the A4 notice — 解释/矛盾吗 answering in prose is correct, not a
    // miss, and must stay quiet.
    const flag = expectMap ? "\n【期望落图】本次请求必须以 map_ops 围栏落图。" : "";
    onAsk?.(
      `[导图选区 ${sel.length} 个节点|${sel.join(",")}] ${question}\n${SEL_DISCIPLINE}${flag}`,
    );
  };

  // The hover button cluster — same controls on every rendered row
  // (pill, text row, root). ⌫ is delete (tombstone: 负信号, never grows
  // back); the confirm button doubles as the §4.3 one-key toggle.
  const acts = (n: MapNode): ReactNode => {
    const conf = n.state === "confirmed";
    const parked = n.status === "parked";
    return (
      <span
        className="mm-acts"
        onClick={(e) => e.stopPropagation()}
        onDoubleClick={(e) => e.stopPropagation()}
        onMouseDown={(e) => e.stopPropagation()}
      >
        <button
          className={conf ? "on" : ""}
          title={conf ? "取消确认（回到提案态）" : "确认 — 认定这句，蒸馏不再动它"}
          onClick={toggleConfirm(n)}
        >
          ✓
        </button>
        <button
          className={parked ? "on" : ""}
          title={parked ? "恢复这一支" : "停靠 — 弱化展示，蒸馏倾向保留"}
          onClick={togglePark(n)}
        >
          {parked ? "▶" : "⏸"}
        </button>
        <button title="删除（保留墓碑，之后不再长回）" onClick={removeNode(n)}>
          ✕
        </button>
      </span>
    );
  };
  const editingHere = (id: string) =>
    editing?.id === id ? (
      <input
        className="mm-ein"
        autoFocus
        value={editing.value}
        onChange={(e) => setEditing((ed) => ed && { ...ed, value: e.target.value })}
        onClick={(e) => e.stopPropagation()}
        onDoubleClick={(e) => e.stopPropagation()}
        onKeyDown={(e: KeyboardEvent<HTMLInputElement>) => {
          e.stopPropagation();
          if (e.key === "Enter") commitEdit();
          else if (e.key === "Escape") setEditing(null);
        }}
        onBlur={commitEdit}
      />
    ) : null;

  const title = snap.title || "对话实时思维导图";
  const sub = snap.subtitle;
  const cut = sub.lastIndexOf(" · ");

  const renderRow = (p: Pos): ReactNode => {
    const n = p.node;
    const conf = n.state === "confirmed";
    const cls = [
      p.depth === 1 ? "mm-cbranch" : "mm-cnode",
      n.kind,
      conf ? "confirmed" : "",
      n.status === "parked" ? "parked" : "",
      fresh.has(n.id) ? "fresh" : "",
      selSet.has(n.id) ? "sel" : "",
      p.side === -1 ? "left" : "",
    ]
      .filter(Boolean)
      .join(" ");
    // Content-width rows (Image #56 — labels never clip): x anchors
    // the left edge at the column and the box grows with the label.
    // (Legacy left-side rows would anchor by their right edge; the
    // single-direction layout renders none.)
    const style: CSSProperties & Record<"--b" | "--dot", string> = {
      top: `${p.top}px`,
      "--b": p.hue,
      "--dot": p.hue,
    };
    if (p.depth === 1 || p.side === 1) {
      style.left = `${xOf(p)}px`;
    } else {
      style.right = `${W - (xOf(p) + slotW(p))}px`;
    }
    const kidCount = p.depth === 1 ? countDesc(n.id) : 0;
    const inner =
      editingHere(n.id) ??
      (p.depth === 1 ? (
        <>
          <span className="mm-cl">{n.label}</span>
          {(n.pinned || n.edited_by_user) && (
            <span className="mm-pen" title="人工改过">✎</span>
          )}
          {kidCount > 0 && <span className="n">{kidCount}</span>}
          {acts(n)}
        </>
      ) : (
        <>
          <span className="mm-i">{ICON[n.kind] ?? "·"}</span>
          {n.label}
          {(n.pinned || n.edited_by_user) && (
            <span className="mm-pen" title="人工改过">✎</span>
          )}
          <span className="mm-t">t{n.source_turn}</span>
          {acts(n)}
        </>
      ));
    return (
      <div
        key={n.id}
        className={cls}
        data-mm-id={n.id}
        style={style}
        onClick={(e) => {
          // §7.3: ctrl/cmd-click toggles the selection in place — the
          // jump is the plain click's job, not the modifier's.
          if (e.ctrlKey || e.metaKey) {
            e.stopPropagation();
            onSelect?.(
              selSet.has(n.id)
                ? sel.filter((i) => i !== n.id)
                : [...sel, n.id],
            );
            return;
          }
          onJump(n.source_turn);
        }}
        onDoubleClick={startEdit(n)}
        // Full label FIRST in the tooltip too (Image #54); the row text
        // itself is never clipped since Image #56.
        title={`${n.label}\n${n.kind} · 出自第 ${n.source_turn} 轮 · ${conf ? "已确认" : "待你确认"}${n.status === "parked" ? " · 停靠" : ""} — 点击跳回原话 · 双击改名`}
      >
        {inner}
      </div>
    );
  };

  // No anchor (empty seat, everything tombstoned, LYON_MINDMAP=off never
  // seeded) → the card degrades to nothing, same contract as the
  // non-repo file card. The return sits AFTER every hook — React counts
  // hooks across renders, and a map that empties mid-session (all
  // tombstones) must not drop a layout effect.
  if (!root) return null;

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
            {alive.length - confirmed > 0 && (
              <button
                className="mm-chip mm-allconf"
                title="把所有提案态节点一次转为已确认（§4.3 一键动作，不打断对话）"
                onClick={(e) => {
                  e.stopPropagation();
                  void applyOps(
                    alive
                      .filter((n) => n.state !== "confirmed")
                      .map((n) => ({ op: "confirm", id: n.id, state: "confirmed" })),
                  );
                }}
              >
                全部确认 <b>{alive.length - confirmed}</b>
              </button>
            )}
            {opened > 0 && (
              <span className="mm-chip warn">
                <b>{opened}</b> 未决
              </span>
            )}
            {mem?.eligible && (
              <button
                className="mm-chip mm-mem"
                title="把已确认的结论沉淀为记忆（导图-* 前缀，scope 按 §10.3 判定；已写过则不再出现）"
                onClick={(e) => {
                  e.stopPropagation();
                  saveMemory();
                }}
              >
                存成记忆 <b>{mem.facts.length}</b>
              </button>
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
                <span className="mm-k">○</span> 待答的洞
              </span>
              <span>t# = 出自第几轮</span>
            </span>
          </div>

          {mem?.eligible && (
            <div className="mm-banner">
              <span>
                💾 这份图有 <b>{mem.facts.length}</b>{" "}
                条可沉淀的记忆 — 已确认的结论写进知识库，下次开局就在
              </span>
              <button type="button" className="mm-chip mm-mem" onClick={saveMemory}>
                存成记忆
              </button>
            </div>
          )}
          {memNote && <div className="mm-banner done">{memNote}</div>}

          {sel.length > 0 && (
            <div className={"mm-toolbar" + (armed ? " armed" : "")}>
              {(agentBatches ?? 0) >= 3 && (
                <span className={"mm-tb-warn" + (armed ? " on" : "")}>
                  ⚠ 先处理这 {agentBatches} 批未确认改动
                  {armed ? " — 再点一次执行" : ""}
                </span>
              )}
              <button
                type="button"
                onClick={guard(mergeSel)}
                disabled={selIds.length < 2}
                title="合并：其余选中节点折进第一个（源节点墓碑化，子节点上移）"
              >
                合并
              </button>
              <select
                className="mm-tb-sel"
                value=""
                title="把选中节点挂到某个节点之下（候选已排除选区与选区的后代，避免成环）"
                onChange={(e) => {
                  if (e.target.value) guard(() => reparent(e.target.value))();
                }}
              >
                <option value="">归类到…</option>
                {reparentCands.map((n) => (
                  <option key={n.id} value={n.id}>
                    {n.label}
                  </option>
                ))}
              </select>
              <button
                type="button"
                onClick={guard(parkSel)}
                title="停靠 / 恢复选中节点"
              >
                停靠
              </button>
              <button
                type="button"
                onClick={guard(delSel)}
                title="删除选中节点（墓碑，不再长回）"
              >
                删除
              </button>
              <span className="mm-tb-sep" aria-hidden="true" />
              <button
                type="button"
                onClick={guard(() =>
                  askSel("改写这些节点，让同级表述一致、更准确。", true),
                )}
                title="让模型重写选区节点的表述（map_ops rewrite）"
              >
                改写
              </button>
              <button
                type="button"
                onClick={guard(() => askSel("这些节点之间有矛盾吗？指出冲突之处。"))}
                disabled={sel.length < 2}
                title="检查选区内的结论是否互相冲突"
              >
                矛盾吗
              </button>
              <button
                type="button"
                onClick={guard(() => askSel("解释这些节点的含义与彼此关系。"))}
                title="让模型解释选区（散文回答，不改图）"
              >
                解释
              </button>
              <input
                className="mm-tb-q"
                value={tbQ}
                placeholder="对选区提问…"
                onChange={(e) => setTbQ(e.target.value)}
                onMouseDown={(e) => e.stopPropagation()}
                onKeyDown={(e) => {
                  e.stopPropagation(); // Esc here clears the line, not the selection
                  if (e.key === "Enter") {
                    guard(() => {
                      askSel(tbQ);
                      setTbQ("");
                    })();
                  }
                }}
              />
              <button
                type="button"
                className="mm-tb-go"
                onClick={guard(() => {
                  askSel(tbQ);
                  setTbQ("");
                })}
              >
                提问
              </button>
              <button
                type="button"
                className="mm-tb-x"
                title="清除选区 (Esc)"
                onClick={() => onSelect?.(null)}
              >
                ✕
              </button>
            </div>
          )}
          <div
            className={"mm-canvas" + (panning ? " panning" : "")}
            ref={canvasRef}
            onMouseDown={canvasDown}
            onDoubleClick={(e) => {
              // dbl-click blank: manual zoom → back to auto fit.
              // Node pills own their own dblclick (rename) — skip.
              if ((e.target as HTMLElement).closest("[data-mm-id]")) return;
              setZoom(null);
            }}
          >
            <div
              className="mm-sizer"
              style={{ width: `${W * scale}px`, height: `${H * scale}px` }}
            >
            <div
              className="mm-plane"
              ref={planeRef}
              style={{
                width: `${W}px`,
                height: `${H}px`,
                transform: scale < 1 ? `scale(${scale})` : undefined,
                transformOrigin: "0 0",
              }}
            >
              <svg
                className="mm-links"
                width={W}
                height={H}
                viewBox={`0 0 ${W} ${H}`}
                aria-hidden="true"
              >
                {paths.map((p, i) => (
                  <g key={i}>
                    <path
                      d={p.d}
                      fill="none"
                      stroke={p.hue}
                      strokeWidth={p.w}
                      strokeLinecap="round"
                      opacity={0.9}
                    />
                    {p.joint && (
                      <circle
                        cx={p.joint.x}
                        cy={p.joint.y}
                        r={3}
                        fill="var(--bg-plan)"
                        stroke={p.joint.hue}
                        strokeWidth={1.5}
                      />
                    )}
                  </g>
                ))}
                {rels.map((r, i) => (
                  <path
                    key={`rel-${i}`}
                    d={r.d}
                    fill="none"
                    stroke="var(--dim)"
                    strokeWidth={1.3}
                    strokeDasharray="5 4"
                    opacity={0.75}
                  />
                ))}
              </svg>

              <div
                className={"mm-croot" + (selSet.has(root.id) ? " sel" : "")}
                data-mm-id={root.id}
                style={{ left: `${CX}px`, top: `${CY}px` }}
                onClick={(e) => {
                  if (e.ctrlKey || e.metaKey) {
                    e.stopPropagation();
                    onSelect?.(
                      selSet.has(root.id)
                        ? sel.filter((i) => i !== root.id)
                        : [...sel, root.id],
                    );
                    return;
                  }
                  onJump(root.source_turn);
                }}
                onDoubleClick={startEdit(root)}
                title={`${root.label}（t${root.source_turn}）— 点击跳回原话 · 双击改名 · ctrl+点击圈选`}
              >
                {editingHere(root.id) ?? (
                  <>
                    <span className="mm-rl">
                      {root.label}
                      {(root.pinned || root.edited_by_user) && (
                        <span className="mm-pen" title="人工改过">✎</span>
                      )}
                    </span>
                    <span className="mm-rt">t{root.source_turn} · {alive.length} 节点</span>
                    {acts(root)}
                  </>
                )}
              </div>

              {branches.length === 0 && (
                <div className="mm-empty mm-cempty">还没有分支 — 蒸馏还在攒</div>
              )}
              {pos.map(renderRow)}

              {marquee && (marquee.w > 0 || marquee.h > 0) && (
                <div
                  className="mm-marquee"
                  style={{
                    left: `${marquee.x}px`,
                    top: `${marquee.y}px`,
                    width: `${marquee.w}px`,
                    height: `${marquee.h}px`,
                  }}
                />
              )}

            </div>
            </div>
          </div>

          <div className="mm-foot">
            <span>
              <b>读法</b>：这不是结论，是 lyon 对你这句话的理解。可怀疑是它的本分。
            </span>
            <span>
              <b>生成</b>：旁路蒸馏 · 关掉无痕
            </span>
            <span>
              <b>点</b> 跳回原话 · <b>双击</b> 改名 · 悬停 <b>✓ ⏸ ✕</b> 确认 /
              停靠 / 删除 · <b>框选 / ctrl+点</b> 圈选 · <b>Esc</b> 清除 ·{" "}
              <b>滚轮按住</b> 拖动平移 · <b>ctrl+滚轮</b> 缩放（双击空白复位）
            </span>
          </div>
        </>
      )}
    </div>
  );
}
