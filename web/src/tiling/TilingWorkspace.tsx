// TilingWorkspace — renders the tile tree from useWorkspaceLayout as flat
// absolute-positioned panes keyed by nodeId (splits never remount existing
// panes), with resize handles, title-bar drag-to-dock/swap, maximize, and
// a pane picker for splits. Ported from stereo-claw-cloud's
// TilingWorkspace (zero-dependency, no antd — lyon draws its own icons).

import {
  memo,
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type CSSProperties,
  type ReactNode,
} from "react";
import { type Dir, type DropZone, type PaneKind } from "./types";
import { computeGeometry, type HandleRect } from "./geometry";
import type { WorkspaceLayout } from "./useWorkspaceLayout";

// VSCode-style split affordances: two rectangles side by side / stacked.
const SplitHIcon = () => (
  <svg viewBox="0 0 14 14" width="13" height="13" fill="none" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round" aria-hidden>
    <rect x="1.5" y="2.5" width="4.5" height="9" rx="0.5" />
    <rect x="8" y="2.5" width="4.5" height="9" rx="0.5" />
  </svg>
);
const SplitVIcon = () => (
  <svg viewBox="0 0 14 14" width="13" height="13" fill="none" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round" aria-hidden>
    <rect x="2.5" y="1.5" width="9" height="4.5" rx="0.5" />
    <rect x="2.5" y="8" width="9" height="4.5" rx="0.5" />
  </svg>
);
const PlusIcon = () => (
  <svg viewBox="0 0 14 14" width="13" height="13" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" aria-hidden>
    <path d="M7 2.5v9M2.5 7h9" />
  </svg>
);
const MaxIcon = () => (
  <svg viewBox="0 0 14 14" width="13" height="13" fill="none" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
    <rect x="2.5" y="2.5" width="9" height="9" rx="1" />
  </svg>
);
const RestoreIcon = () => (
  <svg viewBox="0 0 14 14" width="13" height="13" fill="none" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
    <rect x="2.5" y="4.5" width="7" height="7" rx="1" />
    <path d="M4.5 4.5V3.5a1 1 0 0 1 1-1h5a1 1 0 0 1 1 1v5a1 1 0 0 1-1 1h-1" />
  </svg>
);
const CloseIcon = () => (
  <svg viewBox="0 0 14 14" width="13" height="13" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" aria-hidden>
    <path d="M3.5 3.5l7 7M10.5 3.5l-7 7" />
  </svg>
);

export interface PaneDef {
  kind: PaneKind;
  title: string;
  /** Second line in the pane picker (SCC-style icon list). */
  desc?: string;
  icon?: ReactNode;
  /**
   * Render the pane body. leafId is the hosting tile's nodeId — panes
   * with per-leaf state (preview tab stacks) key their records by it;
   * identity is stable across splits and drag-moves.
   */
  render: (ctx: { leafId: string }) => ReactNode;
}

const MIN_PANE_PX = 180; // floors each side of a resize drag (D7)

// ── Pane header ───────────────────────────────────────────────────────────────

const PaneHeader = memo(function PaneHeader({
  def,
  focused,
  isMaximized,
  canClose,
  isDragSource,
  onSplitH,
  onSplitV,
  onAddPanel,
  onMaximize,
  onRestore,
  onClose,
  onDragStart,
}: {
  def: PaneDef | undefined;
  focused: boolean;
  isMaximized: boolean;
  canClose: boolean;
  isDragSource: boolean;
  onSplitH: () => void;
  onSplitV: () => void;
  onAddPanel: () => void;
  onMaximize: () => void;
  onRestore: () => void;
  onClose: () => void;
  onDragStart: (e: React.PointerEvent) => void;
}) {
  const btnStyle: CSSProperties = {
    display: "flex",
    alignItems: "center",
    justifyContent: "center",
    width: 24,
    height: 22,
    padding: 0,
    border: "none",
    borderRadius: "var(--radius-1)",
    background: "transparent",
    color: "var(--dim)",
    cursor: "pointer",
    flexShrink: 0,
  };
  const hoverIn = (e: React.MouseEvent, danger = false) => {
    const el = e.currentTarget as HTMLElement;
    el.style.background = danger ? "rgba(255,107,107,0.12)" : "var(--bg-hover)";
    el.style.color = danger ? "var(--danger)" : "var(--accent)";
  };
  const hoverOut = (e: React.MouseEvent) => {
    const el = e.currentTarget as HTMLElement;
    el.style.background = "transparent";
    el.style.color = "var(--dim)";
  };

  return (
    <div className={"pane-header" + (focused ? " focused" : "")}>
      <span
        className="pane-title"
        onPointerDown={onDragStart}
        title="drag to move / swap / dock"
        style={{ cursor: isDragSource ? "grabbing" : "grab" }}
      >
        {def?.icon && <span className="pane-icon">{def.icon}</span>}
        <span className="pane-title-text">{def?.title ?? "pane"}</span>
      </span>
      {!isMaximized && (
        <>
          <button style={btnStyle} title="add pane" aria-label="add pane" onClick={onAddPanel}
            onMouseEnter={(e) => hoverIn(e)} onMouseLeave={hoverOut}>
            <PlusIcon />
          </button>
          <button style={btnStyle} title="split left/right" aria-label="split horizontally" onClick={onSplitH}
            onMouseEnter={(e) => hoverIn(e)} onMouseLeave={hoverOut}>
            <SplitHIcon />
          </button>
          <button style={btnStyle} title="split top/bottom" aria-label="split vertically" onClick={onSplitV}
            onMouseEnter={(e) => hoverIn(e)} onMouseLeave={hoverOut}>
            <SplitVIcon />
          </button>
        </>
      )}
      <button
        style={btnStyle}
        title={isMaximized ? "restore" : "maximize"}
        aria-label={isMaximized ? "restore" : "maximize"}
        onClick={isMaximized ? onRestore : onMaximize}
        onMouseEnter={(e) => hoverIn(e)}
        onMouseLeave={hoverOut}
      >
        {isMaximized ? <RestoreIcon /> : <MaxIcon />}
      </button>
      {canClose && (
        <button
          style={btnStyle}
          title="close pane"
          aria-label="close pane"
          onClick={onClose}
          onMouseEnter={(e) => hoverIn(e, true)}
          onMouseLeave={hoverOut}
        >
          <CloseIcon />
        </button>
      )}
    </div>
  );
});

// ── Pane picker (split target) ────────────────────────────────────────────────

const PanePicker = memo(function PanePicker({
  panes,
  onPick,
  onCancel,
}: {
  panes: PaneDef[];
  onPick: (kind: PaneKind) => void;
  onCancel: () => void;
}) {
  return (
    <div className="picker-scrim" onClick={onCancel}>
      <div className="picker-card" onClick={(e) => e.stopPropagation()}>
        <div className="picker-head">add pane</div>
        {panes.map((p) => (
          <button key={p.kind} className="picker-item" onClick={() => onPick(p.kind)}>
            {p.icon && <span className="pane-icon picker-icon">{p.icon}</span>}
            <span className="picker-labels">
              <span>{p.title}</span>
              {p.desc && <span className="picker-desc">{p.desc}</span>}
            </span>
          </button>
        ))}
      </div>
    </div>
  );
});

// ── Main ──────────────────────────────────────────────────────────────────────

interface PaneDragState {
  sourceLeafId: string;
  startX: number;
  startY: number;
  ghostX: number;
  ghostY: number;
  active: boolean;
  hover: { leafId: string; zone: "center" | DropZone } | null;
}

export default function TilingWorkspace({
  ws,
  panes,
  shakeLeaf,
}: {
  ws: WorkspaceLayout;
  panes: PaneDef[];
  /** Leaf id to shake briefly (plan-chip focus affordance); "" = none. */
  shakeLeaf?: string;
}) {
  const { root, focus, maximized, setFocus, setMaximized } = ws;
  const [picker, setPicker] = useState<{ leafId: string; dir: Dir } | null>(null);
  const [paneDrag, setPaneDrag] = useState<PaneDragState | null>(null);
  const containerRef = useRef<HTMLDivElement>(null);

  const geo = useMemo(() => computeGeometry(root, 0, 0, 1, 1), [root]);
  const geoRef = useRef(geo);
  useEffect(() => {
    geoRef.current = geo;
  }, [geo]);
  const rootRef = useRef(root);
  useEffect(() => {
    rootRef.current = root;
  }, [root]);

  // ── Resize drag (rAF-coalesced pointermove) ─────────────────────────────────

  const dragRef = useRef<HandleRect | null>(null);
  const pendingMove = useRef<PointerEvent | null>(null);
  const rafRef = useRef(0);

  useEffect(() => {
    const flush = () => {
      rafRef.current = 0;
      const e = pendingMove.current;
      pendingMove.current = null;
      const d = dragRef.current;
      if (!e || !d || !containerRef.current) return;
      const rect = containerRef.current.getBoundingClientRect();
      const posFrac =
        d.dir === "h"
          ? (e.clientX - rect.left) / rect.width
          : (e.clientY - rect.top) / rect.height;
      let ratio = (posFrac - d.axisStart) / d.axisSize;
      // Pixel floor: neither side of the dragged split may shrink below
      // MIN_PANE_PX (ratio itself clamps [0.1,0.9] in updateRatio).
      const axisPx = d.axisSize * (d.dir === "h" ? rect.width : rect.height);
      const minRatio = Math.min(0.45, MIN_PANE_PX / Math.max(axisPx, 1));
      ratio = Math.max(minRatio, Math.min(1 - minRatio, ratio));
      ws.updateRatio(d.splitId, ratio);
    };
    const onMove = (e: PointerEvent) => {
      pendingMove.current = e;
      if (!rafRef.current) rafRef.current = requestAnimationFrame(flush);
    };
    const onUp = () => {
      dragRef.current = null;
      if (rafRef.current) {
        cancelAnimationFrame(rafRef.current);
        rafRef.current = 0;
      }
    };
    document.addEventListener("pointermove", onMove);
    document.addEventListener("pointerup", onUp);
    return () => {
      document.removeEventListener("pointermove", onMove);
      document.removeEventListener("pointerup", onUp);
      if (rafRef.current) cancelAnimationFrame(rafRef.current);
    };
  }, [ws]);

  // ── Pane title-bar drag → move / swap / dock ────────────────────────────────

  const handlePaneDragStart = useCallback(
    (leafId: string, e: React.PointerEvent) => {
      if (e.button !== 0) return;
      if (geoRef.current.leaves.length < 2) return;
      if (maximized) return;
      setPaneDrag({
        sourceLeafId: leafId,
        startX: e.clientX,
        startY: e.clientY,
        ghostX: e.clientX,
        ghostY: e.clientY,
        active: false,
        hover: null,
      });
    },
    [maximized],
  );

  const isDraggingPane = paneDrag !== null;
  useEffect(() => {
    if (!isDraggingPane) return;
    const THRESHOLD = 5; // clicks on the title must not start a drag
    const EDGE = 0.22; // fraction of a pane that reads as a dock zone

    const onMove = (e: PointerEvent) => {
      setPaneDrag((d) => {
        if (!d) return d;
        const dx = Math.abs(e.clientX - d.startX);
        const dy = Math.abs(e.clientY - d.startY);
        const active = d.active || dx > THRESHOLD || dy > THRESHOLD;
        let hover: PaneDragState["hover"] = null;
        const container = containerRef.current;
        if (active && container) {
          const rect = container.getBoundingClientRect();
          const x = (e.clientX - rect.left) / rect.width;
          const y = (e.clientY - rect.top) / rect.height;
          for (const leaf of geoRef.current.leaves) {
            if (
              x >= leaf.left && x < leaf.left + leaf.width &&
              y >= leaf.top && y < leaf.top + leaf.height
            ) {
              const lx = (x - leaf.left) / leaf.width;
              const ly = (y - leaf.top) / leaf.height;
              const fromTop = ly;
              const fromBottom = 1 - ly;
              const fromLeft = lx;
              const fromRight = 1 - lx;
              const minDist = Math.min(fromTop, fromBottom, fromLeft, fromRight);
              let zone: "center" | DropZone = "center";
              if (minDist < EDGE) {
                if (minDist === fromTop) zone = "top";
                else if (minDist === fromBottom) zone = "bottom";
                else if (minDist === fromLeft) zone = "left";
                else zone = "right";
              }
              hover = { leafId: leaf.nodeId, zone };
              break;
            }
          }
        }
        return { ...d, ghostX: e.clientX, ghostY: e.clientY, active, hover };
      });
    };

    const finalize = () => {
      setPaneDrag((d) => {
        if (d && d.active && d.hover && d.hover.leafId !== d.sourceLeafId) {
          const { sourceLeafId } = d;
          const { leafId: targetId, zone } = d.hover;
          if (zone === "center") ws.swapLeaves(sourceLeafId, targetId);
          else ws.moveLeaf(sourceLeafId, targetId, zone);
        }
        return null;
      });
    };

    document.addEventListener("pointermove", onMove);
    document.addEventListener("pointerup", finalize);
    document.addEventListener("pointercancel", finalize);
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setPaneDrag(null);
    };
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("pointermove", onMove);
      document.removeEventListener("pointerup", finalize);
      document.removeEventListener("pointercancel", finalize);
      document.removeEventListener("keydown", onKey);
    };
  }, [isDraggingPane, ws]);

  // While dragging, block iframes/canvas from stealing the pointer stream.
  useEffect(() => {
    if (!paneDrag?.active) return;
    const prev = document.body.style.cursor;
    document.body.style.cursor = "grabbing";
    return () => {
      document.body.style.cursor = prev;
    };
  }, [paneDrag?.active]);

  const paneCount = geo.leaves.length;

  return (
    <div ref={containerRef} className="tile-root">
      {geo.leaves.map((leaf) => {
        const def = panes.find((p) => p.kind === leaf.pane);
        const isMax = maximized === leaf.nodeId;
        const isDragSource = paneDrag?.active && paneDrag.sourceLeafId === leaf.nodeId;
        return (
          <div
            key={leaf.nodeId}
            data-leaf-id={leaf.nodeId}
            className={
              "tile-leaf" +
              (focus === leaf.nodeId ? " focused" : "") +
              (shakeLeaf === leaf.nodeId ? " shake" : "")
            }
            style={{
              position: isMax ? "fixed" : "absolute",
              left: isMax ? 0 : `${leaf.left * 100}%`,
              top: isMax ? 0 : `${leaf.top * 100}%`,
              width: isMax ? "100vw" : `${leaf.width * 100}%`,
              height: isMax ? "100vh" : `${leaf.height * 100}%`,
              zIndex: isMax ? 90 : 1,
              // geometry transition lives in .tile-leaf (token ladder + reduced-motion)
            }}
            onPointerDown={() => setFocus(leaf.nodeId)}
          >
            <div className={"tile-card" + (isDragSource ? " drag-source" : "")}>
              <PaneHeader
                def={def}
                focused={focus === leaf.nodeId}
                isMaximized={isMax}
                canClose={paneCount > 1}
                isDragSource={!!isDragSource}
                onSplitH={() => setPicker({ leafId: leaf.nodeId, dir: "h" })}
                onSplitV={() => setPicker({ leafId: leaf.nodeId, dir: "v" })}
                onAddPanel={() => setPicker({ leafId: leaf.nodeId, dir: "h" })}
                onMaximize={() => setMaximized(leaf.nodeId)}
                onRestore={() => setMaximized(null)}
                onClose={() => ws.closeLeaf(leaf.nodeId)}
                onDragStart={(e) => handlePaneDragStart(leaf.nodeId, e)}
              />
              <div
                className="tile-body"
                style={{ pointerEvents: paneDrag?.active ? "none" : undefined }}
              >
                {def?.render({ leafId: leaf.nodeId })}
              </div>
            </div>
          </div>
        );
      })}

      {/* Dock-zone preview while dragging */}
      {paneDrag?.active && paneDrag.hover &&
        paneDrag.hover.leafId !== paneDrag.sourceLeafId &&
        (() => {
          const target = geo.leaves.find((l) => l.nodeId === paneDrag.hover!.leafId);
          if (!target) return null;
          const z = paneDrag.hover.zone;
          let { left, top, width, height } = target;
          if (z === "top") height = target.height / 2;
          if (z === "bottom") {
            top = target.top + target.height / 2;
            height = target.height / 2;
          }
          if (z === "left") width = target.width / 2;
          if (z === "right") {
            left = target.left + target.width / 2;
            width = target.width / 2;
          }
          return (
            <div
              className="tile-drop"
              style={{
                left: `${left * 100}%`,
                top: `${top * 100}%`,
                width: `${width * 100}%`,
                height: `${height * 100}%`,
              }}
            />
          );
        })()}

      {/* Drag ghost chip */}
      {paneDrag?.active &&
        (() => {
          const sourceLeaf = geo.leaves.find((l) => l.nodeId === paneDrag.sourceLeafId);
          const def = sourceLeaf && panes.find((p) => p.kind === sourceLeaf.pane);
          return (
            <div
              className="tile-ghost"
              style={{ left: paneDrag.ghostX + 14, top: paneDrag.ghostY + 14 }}
            >
              {def?.icon && <span className="pane-icon">{def.icon}</span>}
              <span>{def?.title ?? "pane"}</span>
            </div>
          );
        })()}

      {/* Resize handles */}
      {!maximized &&
        geo.handles.map((h) => (
          <div
            key={h.splitId}
            role="separator"
            aria-orientation={h.dir === "h" ? "vertical" : "horizontal"}
            className="tile-sep"
            style={
              h.dir === "h"
                ? {
                    left: `calc(${h.pos * 100}% - 3px)`,
                    top: `${h.crossStart * 100}%`,
                    width: "6px",
                    height: `${(h.crossEnd - h.crossStart) * 100}%`,
                    cursor: "col-resize",
                  }
                : {
                    left: `${h.crossStart * 100}%`,
                    top: `calc(${h.pos * 100}% - 3px)`,
                    width: `${(h.crossEnd - h.crossStart) * 100}%`,
                    height: "6px",
                    cursor: "row-resize",
                  }
            }
            onPointerDown={(e) => {
              e.preventDefault();
              (e.target as HTMLElement).setPointerCapture(e.pointerId);
              dragRef.current = h;
            }}
          />
        ))}

      {picker && (
        <PanePicker
          panes={panes}
          onPick={(kind) => {
            ws.splitLeafAt(picker.leafId, picker.dir, kind);
            setPicker(null);
          }}
          onCancel={() => setPicker(null)}
        />
      )}
    </div>
  );
}
