// PreviewPane — a standalone file-preview pane with its own tab stack
// (plan Phase B; replaces FilePanel's embedded 45% preview). One instance
// per preview leaf; the tab record lives in App's useFileTabs so the pane
// body survives layout moves and two preview panes stay independent.

import { lazy, Suspense, useEffect, useRef, useState } from "react";
import { errText } from "../api";
import { dirnameOf, fetchPreview, revokePreview, type Preview } from "../filePreview";
import type { PVFile } from "../useFileTabs";
import { makeMdComponents, resolveRel } from "./mdkit";
import SkeletonRows from "./Skeleton";

const MarkdownView = lazy(() => import("./Markdown"));
// Editor rides the same lazy path as the md parser — CodeMirror only
// shows up once a text file opens.
const CodeEditor = lazy(() => import("./CodeEditor"));
// Office viewers stay out of the main bundle: pptx-preview (~500KB),
// mammoth, and SheetJS are each reachable only from their own pane.
const PptxDeck = lazy(() => import("./previews/PptxDeck"));
const DocxPreview = lazy(() => import("./previews/DocxPreview"));
const XlsxSheet = lazy(() => import("./previews/XlsxSheet"));

// XlsxView — the fetch-shell half of a sibling product's viewer split:
// XLSX.read here (dynamic import keeps SheetJS lazy), grid rendering in
// XlsxSheet. XLSX.read sniffs the CSV text variant on its own, so .csv
// rides the same path as .xlsx.
function XlsxView({ data, filename }: { data: ArrayBuffer; filename: string }) {
  const [workbook, setWorkbook] = useState<import("xlsx").WorkBook | null>(null);
  const [err, setErr] = useState<string | null>(null);
  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const XLSX = await import("xlsx");
        const book = XLSX.read(new Uint8Array(data), { type: "array" });
        if (!cancelled) setWorkbook(book);
      } catch (e) {
        if (!cancelled) setErr(errText(e));
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [data]);
  return <XlsxSheet workbook={workbook} filename={filename} err={err} />;
}

// ZoomImage: Ctrl+wheel zooms about the cursor (the content point under
// the pointer stays put — the sibling product's image anchor rule, algebraic
// version for the transform path), drag pans, double-click resets. The
// wheel listener must be native + non-passive — React attaches wheel
// passively, so preventDefault there is ignored and the page behind
// would zoom instead.
function ZoomImage({ url, alt }: { url: string; alt: string }) {
  const [scale, setScale] = useState(1);
  const [off, setOff] = useState({ x: 0, y: 0 });
  const boxRef = useRef<HTMLDivElement>(null);
  const imgRef = useRef<HTMLImageElement>(null);
  const pan = useRef<{ x: number; y: number; ox: number; oy: number } | null>(null);
  const view = useRef({ scale: 1, off: { x: 0, y: 0 } });
  view.current = { scale, off };

  useEffect(() => {
    const el = imgRef.current;
    if (!el) return;
    const onWheel = (e: WheelEvent) => {
      if (!e.ctrlKey && !e.metaKey) return;
      e.preventDefault();
      const box = boxRef.current?.getBoundingClientRect();
      if (!box) return;
      const { scale: s1, off } = view.current;
      const s2 = Math.min(8, Math.max(0.2, s1 * Math.exp(-e.deltaY * 0.002)));
      // Anchor algebra: content under the cursor is (r-off)/s1 with r the
      // cursor relative to the box center (the flex-centered img's origin);
      // off2 puts that same content back under the cursor at s2.
      const r = { x: e.clientX - box.left - box.width / 2, y: e.clientY - box.top - box.height / 2 };
      setOff({
        x: r.x - ((r.x - off.x) * s2) / s1,
        y: r.y - ((r.y - off.y) * s2) / s1,
      });
      setScale(s2);
    };
    el.addEventListener("wheel", onWheel, { passive: false });
    return () => el.removeEventListener("wheel", onWheel);
  }, []);

  return (
    <div
      ref={boxRef}
      className="img-zoom pane-zoom"
      onPointerDown={(e) => {
        pan.current = { x: e.clientX, y: e.clientY, ox: off.x, oy: off.y };
        (e.target as HTMLElement).setPointerCapture(e.pointerId);
      }}
      onPointerMove={(e) => {
        const p = pan.current;
        if (!p) return;
        setOff({ x: p.ox + (e.clientX - p.x), y: p.oy + (e.clientY - p.y) });
      }}
      onPointerUp={() => (pan.current = null)}
      onDoubleClick={() => {
        setScale(1);
        setOff({ x: 0, y: 0 });
      }}
    >
      <img
        ref={imgRef}
        className="files-preview-img zoomable"
        src={url}
        alt={alt}
        style={{ transform: `translate(${off.x}px, ${off.y}px) scale(${scale})` }}
      />
      <span className="img-zoom-hint">
        {Math.round(scale * 100)}% · ctrl+wheel zoom · drag pan · dbl-click reset
      </span>
    </div>
  );
}

function baseName(p: string): string {
  return p.split("/").pop() ?? p;
}

export default function PreviewPane(props: {
  files: PVFile[];
  active: number;
  onActivate: (index: number) => void;
  onCloseTab: (index: number) => void;
  /** Route a workspace path (markdown link): directories navigate the
   *  files pane, files open a preview tab. */
  onOpenPath: (path: string) => void;
}) {
  const cur: PVFile | undefined = props.files[props.active];
  const [preview, setPreview] = useState<Preview | null>(null);
  const [err, setErr] = useState("");
  const [loading, setLoading] = useState(false);
  // Race guard: rapid tab switches must not let a slow fetch overwrite
  // the active tab's content.
  const seq = useRef(0);
  // Retry bumps this — path/kind are unchanged on a failed fetch, so the
  // effect needs a second dep to re-run.
  const [retryTick, setRetryTick] = useState(0);

  useEffect(() => {
    if (!cur) {
      setPreview(null);
      setErr("");
      setLoading(false);
      return;
    }
    const my = ++seq.current;
    setLoading(true);
    setErr("");
    // Drop the previous object URL up front so a long browsing session
    // doesn't pin every preview's bytes in memory.
    setPreview((p) => {
      revokePreview(p);
      return null;
    });
    fetchPreview(cur.path, cur.kind)
      .then((p) => {
        if (seq.current !== my) {
          // stale fetch — a newer tab won; don't leak its URL either
          revokePreview(p);
          return;
        }
        setPreview(p);
        setLoading(false);
      })
      .catch((e) => {
        if (seq.current !== my) return;
        setErr(errText(e));
        setLoading(false);
      });
    return () => {
      // unmount or tab switch: nothing more to revoke here — the next
      // effect run (or the final cleanup below) handles the live URL
    };
  }, [cur?.path, cur?.kind, retryTick]);

  // Final cleanup on unmount: revoke whatever media URL is live.
  useEffect(() => {
    return () => {
      seq.current++; // cancel any in-flight fetch
      setPreview((p) => {
        revokePreview(p);
        return null;
      });
    };
  }, []);

  const openPath = (p: string) => props.onOpenPath(p);

  const mdLinks = makeMdComponents({
    onLocalPath: (href) => void openPath(resolveRel(dirnameOf(cur?.path ?? ""), href)),
    basePath: dirnameOf(cur?.path ?? ""),
  });

  if (!cur) {
    return (
      <div className="pv-empty placeholder">
        pick a file in the files pane — it opens here in a tab
      </div>
    );
  }

  return (
    <div className="pv">
      {props.files.length > 1 && (
        <div className="pv-tabs">
          {props.files.map((f, i) => (
            <span
              key={f.path}
              className={"pv-tab" + (i === props.active ? " active" : "")}
            >
              <button
                className="pv-tab-name"
                title={f.path}
                onClick={() => props.onActivate(i)}
              >
                {baseName(f.path)}
              </button>
              <button
                className="pv-tab-close"
                aria-label={`close ${f.path}`}
                onClick={() => props.onCloseTab(i)}
              >
                ×
              </button>
            </span>
          ))}
        </div>
      )}
      {err && (
        <div className="file-err">
          <span className="err">{err}</span>
          <button className="ghost" onClick={() => setRetryTick((t) => t + 1)}>
            retry
          </button>
        </div>
      )}
      {loading && (
        <div className="pv-body">
          <SkeletonRows n={4} />
        </div>
      )}
      {!loading && preview?.kind === "office" && (
        <div className="pv-office">
          {preview.truncated ? (
            <div className="pv-center">
              <span className="err" style={{ textAlign: "center" }}>
                {preview.path} is larger than the preview cap — download it instead
              </span>
            </div>
          ) : (
            <Suspense
              fallback={
                <div className="pv-center">
                  <div className="spin" style={{ width: 22, height: 22 }} />
                </div>
              }
            >
              {preview.office === "pptx" ? (
                <PptxDeck content={preview.data} filename={baseName(preview.path)} />
              ) : preview.office === "docx" ? (
                <DocxPreview content={preview.data} filename={baseName(preview.path)} />
              ) : (
                <XlsxView data={preview.data} filename={baseName(preview.path)} />
              )}
            </Suspense>
          )}
        </div>
      )}
      {!loading && preview && preview.kind !== "office" && (
        <div
          className={
            // an editable text pane needs a column layout (head + editor
            // filling the rest, editor scrolls itself); truncated text and
            // media keep the plain scrollable body
            preview.kind === "text" && !preview.truncated ? "pv-body pv-edit" : "pv-body"
          }
        >
        {preview.kind === "text" && (
          <>
            {/* The editable path shows its name once, in CodeEditor's own
                ce-bar (the bar carries preview/source + save); a static
                head above it stacked two identical filename rows — one
                is enough. Truncated text has no editor, so it keeps the
                head (with its · truncated flag). */}
            {preview.truncated && (
              <div className="files-preview-head">
                {preview.path} · truncated
              </div>
            )}
            {preview.truncated ? (
              // The content is a cut copy — editable/saveable would
              // silently destroy the tail of the file on the next save.
              preview.md ? (
                <div className="md files-md">
                  <Suspense fallback={<pre>{preview.content}</pre>}>
                    <MarkdownView components={mdLinks}>{preview.content}</MarkdownView>
                  </Suspense>
                </div>
              ) : (
                <pre className="pv-pre">{preview.content}</pre>
              )
            ) : (
              <Suspense fallback={<pre className="pv-pre">{preview.content}</pre>}>
                <CodeEditor
                  key={preview.path}
                  path={preview.path}
                  content={preview.content}
                  md={preview.md}
                  mdComponents={mdLinks}
                />
              </Suspense>
            )}
          </>
        )}
        {preview.kind === "media" &&
          (preview.contentType === "application/pdf" ? (
            <iframe className="files-preview-frame pane-frame" src={preview.url} title={preview.path} />
          ) : preview.contentType.startsWith("image/") ? (
            <ZoomImage url={preview.url} alt={preview.path} />
          ) : (
            <>
              <div className="files-preview-head">
                {preview.path} · {preview.contentType}
              </div>
              <a className="file-dl" href={preview.url} download={baseName(preview.path)}>
                download
              </a>
            </>
          ))}
        </div>
      )}
    </div>
  );
}
