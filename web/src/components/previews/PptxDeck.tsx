// PptxDeck — the pptx presentation shell (ported from the sibling product's
// PptxDeck; antd controls replaced with self-drawn ones): continuous
// (list) / paged (slide) dual mode + a collapsible left thumbnail bar +
// zoom + wheel/keyboard page flipping.
//
// Layout contract: the root is position:absolute inset:0 — the parent
// must have a definite height (lyon mounts it in .pv-office).
//
// Key design: both modes share ONE list render — paging just translates
// the viewport to page N, never re-inits pptx-preview, so mode switches
// are free and thumbnails stay available. Preferences (localStorage)
// survive across files: reopening a PPT keeps the last-used habits.

import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import ZoomToolbar from "./ZoomToolbar";
import { PptxPreview } from "./PptxPreview";

const NATIVE_W = 960;
const NATIVE_H = 540;
const PADDING_X = 16;
// The scroll container pads 32px at the bottom; the "scrolled to end"
// line is "visual bottom reached content bottom", not "padding eaten" —
// otherwise a single just-fitting page swallows one wheel notch on the
// fake 32px space.
const PAD_BOTTOM_TOLERANCE = 33;
// Top+bottom padding (16+32) + 4px slack: only when the stage is taller
// than (viewport - padding) does the scroll-then-flip logic engage.
const AVAIL_H_SLACK = 52;

// Double-chevron SVGs (a sibling product used antd's DoubleLeft/DoubleRightOutlined).
const ChevLIcon = () => (
  <svg viewBox="0 0 14 14" width="12" height="12" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
    <path d="M8.5 3L5 7l3.5 4M11.5 3L8 7l3.5 4" />
  </svg>
);
const ChevRIcon = () => (
  <svg viewBox="0 0 14 14" width="12" height="12" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
    <path d="M5.5 3L9 7l-3.5 4M2.5 3L6 7l-3.5 4" />
  </svg>
);
const ArrowLIcon = () => (
  <svg viewBox="0 0 14 14" width="14" height="14" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
    <path d="M9 2.5L4.5 7 9 11.5" />
  </svg>
);
const ArrowRIcon = () => (
  <svg viewBox="0 0 14 14" width="14" height="14" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" aria-hidden>
    <path d="M5 2.5L9.5 7 5 11.5" />
  </svg>
);

export default function PptxDeck({ content, filename, onLoadingChange }: {
  content: Blob | ArrayBuffer | null;
  filename: string;
  onLoadingChange?: (loading: boolean) => void;
}) {
  const scrollRef = useRef<HTMLDivElement | null>(null);
  const [loading, setLoading] = useState(true);
  const [containerSize, setContainerSize] = useState({ w: 0, h: 0 });
  const [zoom, setZoom] = useState(1);
  const [slideCount, setSlideCount] = useState(0);
  const [slides, setSlides] = useState<HTMLElement[]>([]);

  const [viewMode, setViewMode] = useState<"list" | "slide">(() =>
    localStorage.getItem("lyon_pptx_view_mode") === "slide" ? "slide" : "list",
  );
  const [barOpen, setBarOpen] = useState(() => {
    // Narrow viewports always start collapsed (sibling-product decision): the
    // 148px bar eats ~40% of a 390px phone. Desktop keeps the stored
    // preference; no preference means open.
    if (typeof window.matchMedia === "function" &&
      window.matchMedia("(max-width: 720px), (max-height: 500px)").matches) {
      return false;
    }
    return localStorage.getItem("lyon_pptx_bar_open") !== "0";
  });
  const [page, setPage] = useState(0);          // current page in slide mode (0-based)
  const [listPage, setListPage] = useState(0);  // current page in list mode (scroll-tracked, for bar highlight)
  const barListRef = useRef<HTMLDivElement | null>(null);
  // Measured per-page offsets in the list flow (unscaled px, relative to
  // the first page). The library writes margin-bottom:10px per slide, so
  // "page i top = i×540" drifts progressively — flips would reveal the
  // previous page's bottom and jumps would land off. Always trust the
  // measurement.
  const [pageTops, setPageTops] = useState<number[]>([]);

  useEffect(() => {
    if (!slides.length) { setPageTops([]); return; }
    const tops = slides.map((el) => el.offsetTop);
    const base = tops[0] || 0;
    setPageTops(tops.map((t) => t - base));
  }, [slides]);

  const topOf = (i: number) => pageTops[i] ?? i * NATIVE_H;

  // Note the content dep: before content arrives the scroll container is
  // not in the DOM. Mounting the RO once on an absent element would leave
  // containerW at 0 forever → scale floored at 480 → half-size PPT.
  useEffect(() => {
    const el = scrollRef.current;
    if (!el) return;
    const ro = new ResizeObserver(() => setContainerSize({ w: el.clientWidth, h: el.clientHeight }));
    ro.observe(el);
    return () => ro.disconnect();
  }, [content]);
  // Measure synchronously once: avoids the first-paint flash where
  // containerW=0 → availW=480 → PPT renders at half size.
  useLayoutEffect(() => {
    const el = scrollRef.current;
    if (!el) return;
    setContainerSize({ w: el.clientWidth, h: el.clientHeight });
  }, [content]);

  // These callbacks MUST be reference-stable: PptxPreview's useEffect
  // takes them as deps, and an inline arrow (new ref each parent render)
  // would re-run destroy/init in a loop, stacking multiple renders.
  const onPptxReady = useCallback(
    ({ slideCount: sc, slides: sl }: { slideCount: number; slides: HTMLElement[] }) => {
      setSlideCount(sc);
      setSlides(sl);
      setPage(0);
      setListPage(0);
    },
    [],
  );
  const onPptxLoading = useCallback((l: boolean) => {
    setLoading(l);
    onLoadingChange?.(l);
  }, [onLoadingChange]);

  // Base scale: list mode fit-width (it scrolls); slide mode contain —
  // min of width/height ratios so the whole page is always in view.
  // Width-only scaling would grow unbounded on wide windows and clip the
  // page bottom in slide mode (guaranteed when maximized). No 480 floor:
  // it overflows narrow panes; the first-frame flash is already handled
  // by the synchronous layout measure above.
  const availW = Math.max(1, containerSize.w - PADDING_X * 2);
  const availH = Math.max(1, containerSize.h - 48); // top+bottom padding 16+32
  const baseScale = viewMode === "slide"
    ? Math.min(availW / NATIVE_W, availH / NATIVE_H)
    : availW / NATIVE_W;
  const scale = baseScale * zoom;
  // List-mode scroll math (track current page / jump / return-to-list)
  // must use the same algorithm as the rendered scale.
  const listScaleOf = (el: HTMLDivElement) =>
    (Math.max(1, el.clientWidth - PADDING_X * 2) / NATIVE_W) * zoom;
  // Total height from measured page positions (last page top + page
  // height); equal-split fallback until pageTops land.
  const innerH = pageTops.length
    ? pageTops[pageTops.length - 1] + NATIVE_H
    : Math.max(1, slideCount) * NATIVE_H;
  const visualW = NATIVE_W * scale;
  const visualH = innerH * scale;

  const curPage = viewMode === "slide" ? page : listPage;

  // List mode: track the scroll position → current page (drives the
  // thumbnail-bar highlight). "Nearest page top to the scroll offset"
  // stays correct even with uneven page gaps.
  useEffect(() => {
    if (viewMode !== "list") return;
    const el = scrollRef.current;
    if (!el) return;
    let raf = 0;
    const onScroll = () => {
      if (raf) return;
      raf = requestAnimationFrame(() => {
        raf = 0;
        const s = listScaleOf(el);
        let best = 0;
        for (let i = 1; i < slideCount; i++) {
          if (Math.abs(topOf(i) * s - el.scrollTop) < Math.abs(topOf(best) * s - el.scrollTop)) best = i;
        }
        setListPage(best);
      });
    };
    onScroll();
    el.addEventListener("scroll", onScroll, { passive: true });
    return () => { el.removeEventListener("scroll", onScroll); if (raf) cancelAnimationFrame(raf); };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [viewMode, slideCount, zoom, pageTops]);

  // Single entry for page changes in slide mode: also resets the scroll
  // to the page top — after zooming past one page, keeping the old
  // offset would park the new page mid-view.
  const flipTo = (i: number) => {
    setPage(Math.max(0, Math.min(i, Math.max(0, slideCount - 1))));
    const el = scrollRef.current;
    if (el) { el.scrollTop = 0; el.scrollLeft = 0; }
  };

  // Slide mode: ←/→/PgUp/PgDn flip. Keys are left alone when focus sits
  // in an input/textarea/contenteditable (those keys have day jobs).
  useEffect(() => {
    if (viewMode !== "slide") return;
    const onKey = (e: KeyboardEvent) => {
      const t = e.target as HTMLElement | null;
      if (t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.isContentEditable)) return;
      if (e.key === "ArrowRight" || e.key === "PageDown") flipTo(page + 1);
      else if (e.key === "ArrowLeft" || e.key === "PageUp") flipTo(page - 1);
      else return;
      e.preventDefault();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [viewMode, slideCount, page]);

  // Slide mode: wheel-to-flip (touchpad horizontal sweeps count too).
  // When zoom makes a page taller/wider than the viewport, scroll the
  // content first and flip only at the edges — PDF-reader semantics;
  // 400ms inertia lock stops multi-flips.
  const wheelLockRef = useRef(0);
  useEffect(() => {
    if (viewMode !== "slide" || slideCount <= 0) return;
    const el = scrollRef.current;
    if (!el) return;
    const onWheel = (e: WheelEvent) => {
      const { deltaX: dx, deltaY: dy } = e;
      const d = Math.abs(dx) > Math.abs(dy) ? dx : dy;
      if (!d) return;
      const stageEl = el.firstElementChild as HTMLElement | null;
      const canY = stageEl
        ? stageEl.offsetHeight > el.clientHeight - AVAIL_H_SLACK
        : el.scrollHeight > el.clientHeight + 1;
      const canX = stageEl
        ? stageEl.offsetWidth > el.clientWidth - PADDING_X * 2 - 4
        : el.scrollWidth > el.clientWidth + 1;
      if (Math.abs(dx) > Math.abs(dy)) {
        if (canX) return; // zoomed wide: let the sweep scroll content
      } else if (canY) {
        const atTop = el.scrollTop <= 0;
        const atBottom = el.scrollTop + el.clientHeight >= el.scrollHeight - PAD_BOTTOM_TOLERANCE;
        if (d > 0 && !atBottom) return; // not at bottom: native scroll sees the lower half
        if (d < 0 && !atTop) return;
      }
      e.preventDefault();
      const now = Date.now();
      if (now - wheelLockRef.current < 400) return;
      wheelLockRef.current = now;
      flipTo(d > 0 ? page + 1 : page - 1);
    };
    el.addEventListener("wheel", onWheel, { passive: false });
    return () => el.removeEventListener("wheel", onWheel);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [viewMode, slideCount, page]);

  // ── Drag-to-pan when zoomed (image-viewer convention): only when the
  // stage overflows horizontally (= zoomed). At fit scale, list mode's
  // vertical overflow is normal reading scroll — stealing the drag would
  // kill text selection (userSelect:text is deliberately kept). Activation
  // needs 5px of travel; while active, cancel any started selection and
  // swallow the trailing click.
  useEffect(() => {
    const el = scrollRef.current;
    if (!el) return;
    let startX = 0, startY = 0, startSL = 0, startST = 0;
    let armed = false, active = false, suppressClick = false;
    const pannable = () => el.scrollWidth > el.clientWidth + 1;

    const onMouseDown = (e: MouseEvent) => {
      if (e.button !== 0 || !pannable()) return;
      armed = true;
      active = false;
      suppressClick = false; // a previous drag's suppression must not outlive this press
      startX = e.clientX; startY = e.clientY;
      startSL = el.scrollLeft; startST = el.scrollTop;
    };
    const onMouseMove = (e: MouseEvent) => {
      if (!armed) return;
      const dx = e.clientX - startX;
      const dy = e.clientY - startY;
      if (!active) {
        if (Math.abs(dx) < 5 && Math.abs(dy) < 5) return;
        active = true;
        suppressClick = true;
        el.classList.add("pptx-panning");
        window.getSelection()?.removeAllRanges(); // drag takes over — undo any started selection
      }
      el.scrollLeft = startSL - dx;
      el.scrollTop = startST - dy;
      e.preventDefault(); // once active, don't extend the selection
    };
    const onMouseUp = () => {
      armed = false;
      active = false;
      el.classList.remove("pptx-panning");
    };
    // The click after a drag (its target is the mousedown/mouseup common
    // ancestor) is swallowed in the capture phase so in-slide links don't
    // fire; the flag lives only until the next mousedown resets it.
    const onClickCapture = (e: MouseEvent) => {
      if (!suppressClick) return;
      suppressClick = false;
      e.preventDefault();
      e.stopPropagation();
    };

    el.addEventListener("mousedown", onMouseDown);
    window.addEventListener("mousemove", onMouseMove);
    window.addEventListener("mouseup", onMouseUp);
    el.addEventListener("click", onClickCapture, true);
    return () => {
      el.removeEventListener("mousedown", onMouseDown);
      window.removeEventListener("mousemove", onMouseMove);
      window.removeEventListener("mouseup", onMouseUp);
      el.removeEventListener("click", onClickCapture, true);
    };
    // content dep: before content arrives the scroll container is not in
    // the DOM (same reasoning as the ResizeObserver above)
  }, [content]);

  // grab cursor only while horizontally overflowable (pannable); the
  // layout shifts with zoom/viewport so re-measure on each.
  useEffect(() => {
    const el = scrollRef.current;
    if (!el) return;
    const raf = requestAnimationFrame(() => {
      el.classList.toggle("pptx-pannable", el.scrollWidth > el.clientWidth + 1);
    });
    return () => cancelAnimationFrame(raf);
  }, [zoom, containerSize, viewMode, content, slideCount]);

  const switchViewMode = (m: "list" | "slide") => {
    if (m === viewMode) return;
    setViewMode(m);
    localStorage.setItem("lyon_pptx_view_mode", m);
    if (m === "slide") {
      // Continue from whatever page list mode was showing; scroll to top.
      setPage(listPage);
      requestAnimationFrame(() => {
        const el = scrollRef.current;
        if (el) { el.scrollTop = 0; el.scrollLeft = 0; }
      });
    } else {
      // Back to list: scroll to the page slide mode parked on.
      requestAnimationFrame(() => {
        const el = scrollRef.current;
        if (!el) return;
        const s = listScaleOf(el);
        el.scrollTop = topOf(page) * s;
      });
    }
  };

  const toggleBar = () => {
    setBarOpen((v) => {
      // A narrow-viewport expansion lives for this session only: the
      // stored preference is the desktop's.
      if (!(typeof window.matchMedia === "function" &&
        window.matchMedia("(max-width: 720px), (max-height: 500px)").matches)) {
        localStorage.setItem("lyon_pptx_bar_open", v ? "0" : "1");
      }
      return !v;
    });
  };

  // Bar thumbnail click: slide mode flips, list mode smooth-scrolls.
  const gotoPage = (i: number) => {
    if (viewMode === "slide") {
      flipTo(i);
      return;
    }
    const el = scrollRef.current;
    if (!el) return;
    const s = listScaleOf(el);
    el.scrollTo({ top: topOf(i) * s, behavior: "smooth" });
  };

  // ── Left bar thumbnails: clone each page's DOM (imperative — React does
  // not participate inside). Canvas (chart) clones need a toDataURL → img
  // swap once mounted. Giant decks (>THUMB_MAX pages) degrade to a plain
  // page-number grid (JSX below) to avoid clone jank.
  const THUMB_MAX = 60;
  useEffect(() => {
    const bar = barListRef.current;
    if (!bar) return;
    bar.innerHTML = "";
    if (!slides.length || slides.length > THUMB_MAX) return;
    const THUMB_W = 120;
    slides.forEach((src, i) => {
      const card = document.createElement("div");
      card.className = "pptx-thumb";
      card.dataset.page = String(i);
      const clip = document.createElement("div");
      clip.className = "pptx-thumb-clip";
      const inner = document.createElement("div");
      inner.style.cssText = `width:${NATIVE_W}px;height:${NATIVE_H}px;transform:scale(${THUMB_W / NATIVE_W});transform-origin:0 0;position:relative;pointer-events:none;`;
      const clone = src.cloneNode(true) as HTMLElement;
      // Source pages flow sequentially in the list; the clone needs to be
      // pinned to the origin of the fixed 960×540 thumb canvas.
      clone.style.position = "absolute";
      clone.style.top = "0";
      clone.style.left = "0";
      clone.querySelectorAll<HTMLElement>("*").forEach((el) => {
        if (el.style.overflow === "hidden") el.style.overflow = "visible";
      });
      inner.appendChild(clone);
      clip.appendChild(inner);
      const num = document.createElement("div");
      num.className = "pptx-thumb-num";
      num.textContent = String(i + 1);
      card.appendChild(clip);
      card.appendChild(num);
      bar.appendChild(card);
    });
    // canvas → img (clones are in the document now, so offsetLeft/Top is
    // measurable; container geometry matches the source)
    bar.querySelectorAll<HTMLCanvasElement>(".pptx-thumb-clip canvas").forEach((cv) => {
      try {
        const img = document.createElement("img");
        img.src = cv.toDataURL();
        img.style.cssText = `position:absolute;left:${cv.offsetLeft}px;top:${cv.offsetTop}px;width:${cv.offsetWidth}px;height:${cv.offsetHeight}px;`;
        cv.replaceWith(img);
      } catch { /* tainted canvas etc: leave the thumb blank — main preview unaffected */ }
    });
  }, [slides, barOpen]);

  // Current-page highlight + scroll-into-view (class toggles only —
  // clones are not rebuilt, scrolling stays smooth).
  useEffect(() => {
    const bar = barListRef.current;
    if (!bar) return;
    bar.querySelectorAll<HTMLElement>(".pptx-thumb").forEach((el) => {
      const on = el.dataset.page === String(curPage);
      el.classList.toggle("active", on);
      if (on) el.scrollIntoView({ block: "nearest" });
    });
  }, [curPage]);

  if (!content) {
    return (
      <div className="pptx-loading">
        <span>loading…</span>
      </div>
    );
  }

  return (
    <div className="pptx-deck-stage">
      {/* Left page bar: collapsible (26px rail ⇔ 148px thumbnail list) */}
      <div className={"pptx-bar" + (barOpen ? "" : " collapsed")}>
        {barOpen ? (
          <>
            <div className="pptx-bar-head">
              <span className="pptx-bar-title">pages</span>
              <button className="pptx-bar-btn" onClick={toggleBar} title="collapse page bar" aria-label="collapse page bar">
                <ChevLIcon />
              </button>
            </div>
            <div
              className="pptx-bar-list"
              ref={barListRef}
              onClick={(e) => {
                const card = (e.target as HTMLElement).closest?.(".pptx-thumb") as HTMLElement | null;
                if (card?.dataset.page != null) gotoPage(Number(card.dataset.page));
              }}
            >
              {/* thumb cards are cloned in by the effect above */}
            </div>
            {slides.length > THUMB_MAX && (
              <div className="pptx-bar-pages">
                {slides.map((_, i) => (
                  <button key={i} className={i === curPage ? "active" : ""} onClick={() => gotoPage(i)}>{i + 1}</button>
                ))}
              </div>
            )}
            {/* Page indicator pinned at the bar bottom: floating over the
                preview would cover slide footers; hidden with the bar. */}
            {slideCount > 0 && (
              <div className="pptx-bar-page-indicator">{curPage + 1} / {slideCount}</div>
            )}
          </>
        ) : (
          <button className="pptx-bar-btn pptx-bar-expand" onClick={toggleBar} title="expand page bar" aria-label="expand page bar">
            <ChevRIcon />
          </button>
        )}
      </div>

      <div
        ref={scrollRef}
        className="pptx-scroll"
        style={{
          position: "absolute",
          inset: 0,
          left: barOpen ? 148 : 26,
          transition: "left var(--dur-2) var(--ease)",
          overflow: "auto",
          // Slide mode: the page centers vertically. Use the stage's
          // margin:auto, NOT the container's justify-content:center —
          // when zoom overflows the viewport, flex centering makes the
          // top unreachable (classic flex overflow trap); margin:auto
          // degrades to top-aligned and stays scrollable.
          display: viewMode === "slide" ? "flex" : undefined,
          flexDirection: "column",
          padding: `16px ${PADDING_X}px 32px ${PADDING_X}px`,
          userSelect: "text",
          WebkitUserSelect: "text",
        }}
      >
        {/*
          Shell uses the visual size, the inner layer the actual size +
          transform: scale.
          - the shell's width/height anchor margin:auto centering
          - overflow:hidden clips the inner overflow when scale<1
          List mode: shell = full deck height, natural scroll.
          Slide mode: shell shows one page; the inner layer
          translateY(-page×540) flips — underneath it is the SAME list
          render, so the two modes switch instantly and thumbnails always
          work. When zoomed past the viewport, the container's overflow
          takes over: the wheel scrolls content first, flips at the edge.
        */}
        <div
          style={{
            width: visualW,
            height: viewMode === "slide" ? NATIVE_H * scale : visualH,
            margin: viewMode === "slide" ? "auto" : "0 auto",
            overflow: "hidden",
            position: "relative",
            flexShrink: 0,
          }}
        >
          <div
            style={{
              width: NATIVE_W,
              height: innerH,
              transform: viewMode === "slide" ? `scale(${scale}) translateY(${-topOf(page)}px)` : `scale(${scale})`,
              transformOrigin: "0 0",
            }}
          >
            <PptxPreview
              content={content}
              filename={filename}
              width={NATIVE_W}
              height={NATIVE_H}
              onReady={onPptxReady}
              onLoadingChange={onPptxLoading}
            />
          </div>
        </div>
      </div>

      {/* Slide mode: floating prev/next buttons (hidden at the ends).
          When zoomed, the stage overflows horizontally → drag/wheel/keys
          all work and the arrows only block the view — don't render them;
          at fit scale keep the classic behavior. */}
      {viewMode === "slide" && slideCount > 0 && zoom <= 1 && (
        <div
          style={{
            position: "absolute",
            top: 0,
            bottom: 0,
            left: barOpen ? 148 : 26,
            right: 0,
            pointerEvents: "none",
            transition: "left var(--dur-2) var(--ease)",
          }}
        >
          {page > 0 && (
            <button className="pptx-flip-btn" style={{ left: 14 }} onClick={() => flipTo(page - 1)} title="previous page (←)">
              <ArrowLIcon />
            </button>
          )}
          {page < slideCount - 1 && (
            <button className="pptx-flip-btn" style={{ right: 14 }} onClick={() => flipTo(page + 1)} title="next page (→)">
              <ArrowRIcon />
            </button>
          )}
        </div>
      )}
      {/* Mode switch: top-right; both presentations coexist, flip anytime */}
      <div className="pptx-mode-switch">
        <button className={viewMode === "list" ? "active" : ""} onClick={() => switchViewMode("list")}>
          list
        </button>
        <button className={viewMode === "slide" ? "active" : ""} onClick={() => switchViewMode("slide")}>
          slide
        </button>
      </div>
      <ZoomToolbar zoom={zoom} setZoom={setZoom} disabled={loading} />
    </div>
  );
}
