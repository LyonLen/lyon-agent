// PptxPreview — pure pptx render into a host element (ported from the
// portal's PptxPreview; lyon has no sandboxed share page, so the split is
// simpler: this component owns the host ref + initPptxPreview lifecycle,
// PptxDeck owns scale/zoom/page geometry around it).
//
// pptx-preview is a DOM-rendering library (init() writes into a host
// element rather than returning HTML), so:
//   - onReady({ slideCount, slides }) fires once per content change after
//     the render; the deck uses slideCount for natural content height
//     (pptx-preview positions slides absolutely, host scrollHeight is 0)
//     and the slide elements to clone thumbnails.
//   - The host's inner overflow:hidden containers (set inline by the
//     library) are released to overflow:visible so list mode shows all
//     slides; the persistent suppression is the .pptx-host !important
//     CSS rule (the library keeps re-writing the inline styles).

import { useEffect, useRef, useState } from "react";

export type PptxPreviewProps = {
  content: Blob | ArrayBuffer;
  filename: string;
  /** Stage width in px — pptx-preview renders each slide at this width. */
  width?: number;
  /** Stage height in px (540 = 16:9 at 960 wide). */
  height?: number;
  onReady?: (info: { slideCount: number; slides: HTMLElement[] }) => void;
  onLoadingChange?: (loading: boolean) => void;
  onError?: (err: Error) => void;
};

export function PptxPreview({
  content,
  filename,
  width = 960,
  height = 540,
  onReady,
  onLoadingChange,
  onError,
}: PptxPreviewProps) {
  const hostRef = useRef<HTMLDivElement | null>(null);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    let previewer: {
      preview: (ab: ArrayBuffer) => Promise<void>;
      destroy: () => void;
      slideCount?: number;
    } | null = null;

    setLoading(true);
    setErr(null);
    onLoadingChange?.(true);

    void (async () => {
      try {
        if (filename.toLowerCase().endsWith(".ppt") && !filename.toLowerCase().endsWith(".pptx")) {
          throw new Error("legacy .ppt is not supported — save as .pptx and retry");
        }
        const arrayBuffer = content instanceof Blob ? await content.arrayBuffer() : content;
        if (cancelled) return;

        const host = hostRef.current;
        if (!host) return;

        // Clear-out: the effect can re-run on parent re-render (unstable
        // callback deps), and pptx-preview's destroy() does not guarantee
        // it removes the DOM it wrote — without this, renders stack.
        host.innerHTML = "";

        // Dynamic import — pptx-preview is ~500KB, only needed for pptx.
        const { init } = await import("pptx-preview");
        const p = init(host, { width, height, mode: "list" }) as unknown as {
          preview: (ab: ArrayBuffer) => Promise<void>;
          destroy: () => void;
          slideCount?: number;
        };
        previewer = p;
        await p.preview(arrayBuffer);
        if (cancelled) return;

        // The library's viewPort container ships overflow:hidden + fixed
        // height, which clips the absolutely-positioned multi-page list to
        // one page. First frame: release the inline styles immediately so
        // nothing flashes clipped; the .pptx-host !important CSS keeps
        // holding the line afterwards (the library re-writes inline).
        host.querySelectorAll<HTMLElement>("*").forEach((el) => {
          if (el.style.overflow === "hidden") el.style.overflow = "visible";
        });

        setLoading(false);
        onLoadingChange?.(false);
        // Hand the per-page roots out: the deck clones them for the
        // thumbnail bar. The library classes are pptx-preview-slide-wrapper-{i}.
        const slides = Array.from(
          host.querySelectorAll<HTMLElement>('[class*="pptx-preview-slide-wrapper"]'),
        );
        onReady?.({ slideCount: p.slideCount ?? 0, slides });
      } catch (e: unknown) {
        if (cancelled) return;
        const ae = e as { message?: string };
        const errObj = e instanceof Error ? e : new Error(ae?.message || "parse failed — not a valid .pptx file?");
        setErr(errObj.message);
        setLoading(false);
        onLoadingChange?.(false);
        onError?.(errObj);
      }
    })();

    return () => {
      cancelled = true;
      // Belt and braces: destroy() plus a manual host clear, so no
      // residue survives until the next effect run.
      try { previewer?.destroy(); } catch { /* ignore */ }
      previewer = null;
      const host = hostRef.current;
      if (host) host.innerHTML = "";
    };
  }, [content, filename, width, height, onReady, onLoadingChange, onError]);

  return (
    <div style={{ position: "relative", minHeight: loading ? 360 : undefined }}>
      <div
        ref={hostRef}
        className="pptx-host"
        style={{ width, minHeight: height }}
      />
      {loading && !err && (
        <div className="pv-mask">
          <div className="spin" style={{ width: 22, height: 22 }} />
        </div>
      )}
      {err && (
        <div className="pv-mask">
          <span className="err" style={{ textAlign: "center" }}>{err}</span>
        </div>
      )}
    </div>
  );
}
