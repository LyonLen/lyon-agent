// ZoomToolbar — floating bottom-center zoom control (ported from the
// sibling product's zoom bar, self-drawn buttons instead of antd). 1 = each
// preview type's native behavior; click the % to reset to 1. The bar
// fades out after the pointer sits still (so it never blocks a slide or
// a full-bleed image) and returns on the next move or hover.

import { useEffect, useRef, useState } from "react";

export const ZOOM_MIN = 0.5;
export const ZOOM_MAX = 3;
const STEP = 0.25;
const IDLE_HIDE_MS = 2500;

export default function ZoomToolbar({ zoom, setZoom, disabled }: {
  zoom: number;
  setZoom: (z: number) => void;
  disabled?: boolean;
}) {
  const [idle, setIdle] = useState(false);
  const hoveringRef = useRef(false);
  const timerRef = useRef<number | null>(null);
  const armHide = () => {
    setIdle(false);
    if (timerRef.current != null) window.clearTimeout(timerRef.current);
    timerRef.current = window.setTimeout(() => {
      if (!hoveringRef.current) setIdle(true);
    }, IDLE_HIDE_MS);
  };
  useEffect(() => {
    armHide();
    window.addEventListener("pointermove", armHide);
    return () => {
      window.removeEventListener("pointermove", armHide);
      if (timerRef.current != null) window.clearTimeout(timerRef.current);
    };
  }, []);
  const step = (dir: 1 | -1) => {
    const next = Math.min(ZOOM_MAX, Math.max(ZOOM_MIN, +(zoom + dir * STEP).toFixed(2)));
    setZoom(next);
  };
  return (
    <div
      className="zoom-toolbar"
      onMouseEnter={() => { hoveringRef.current = true; armHide(); }}
      onMouseLeave={() => { hoveringRef.current = false; armHide(); }}
      style={{
        opacity: idle ? 0 : 1,
        pointerEvents: idle ? "none" : "auto",
      }}
    >
      <button className="ghost" disabled={zoom <= ZOOM_MIN || disabled}
        onClick={() => step(-1)} aria-label="zoom out" title="zoom out">
        −
      </button>
      <button className="ghost zoom-pct" disabled={zoom === 1} onClick={() => setZoom(1)}
        title="reset zoom">
        {Math.round(zoom * 100)}%
      </button>
      <button className="ghost" disabled={zoom >= ZOOM_MAX || disabled}
        onClick={() => step(1)} aria-label="zoom in" title="zoom in">
        +
      </button>
    </div>
  );
}
