// DocxPreview — .docx → HTML via mammoth, rendered inline (ported from
// the sibling product's DocxPreview; lyon only mounts trusted workspace files, so
// the sandboxed-iframe mode of the share page is dropped). The caller
// fetches the bytes; this component owns conversion + the reading card.
//
// The white card + scoped DOCX_CONTENT_CSS come straight from the
// the sibling product: mammoth's bare HTML ships no borders/spacing, and the card
// gives it a paper to read on.

import { useEffect, useState, type CSSProperties } from "react";

export type DocxPreviewProps = {
  /** Binary content of the .docx file. */
  content: Blob | ArrayBuffer;
  /** Used for the legacy .doc vs .docx branch (we reject .doc up front). */
  filename: string;
  /** Max width of the content card in px. */
  maxWidth?: number;
};

// Mammoth output is "bare HTML" — tables without borders, headings
// without indent, lists without bullet spacing — rendered against the
// browser's user-agent defaults. This scopes the minimal readable set of
// styles to .docx-preview-content, nothing leaks outward.
const DOCX_CONTENT_CSS = `
.docx-preview-content table { border-collapse: collapse; margin: 1em 0; width: 100%; }
.docx-preview-content th,
.docx-preview-content td { border: 1px solid #cbd5e1; padding: 6px 10px; text-align: left; vertical-align: top; }
.docx-preview-content th { background: #f1f5f9; font-weight: 600; }
.docx-preview-content h1 { font-size: 1.8em; font-weight: 600; margin: 0.8em 0 0.4em; }
.docx-preview-content h2 { font-size: 1.5em; font-weight: 600; margin: 0.8em 0 0.4em; }
.docx-preview-content h3 { font-size: 1.25em; font-weight: 600; margin: 0.8em 0 0.4em; }
.docx-preview-content h4,
.docx-preview-content h5,
.docx-preview-content h6 { font-size: 1.1em; font-weight: 600; margin: 0.8em 0 0.4em; }
.docx-preview-content ul,
.docx-preview-content ol { margin: 0.8em 0; padding-left: 1.6em; }
.docx-preview-content li { margin: 0.2em 0; }
.docx-preview-content blockquote { margin: 0.8em 0; padding: 0.4em 0.8em; border-left: 3px solid #cbd5e1; color: #475569; }
.docx-preview-content img { max-width: 100%; height: auto; }
.docx-preview-content a { color: #6bb6ff; text-decoration: underline; }
.docx-preview-content code { background: #f1f5f9; padding: 0.1em 0.3em; border-radius: 3px; font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 0.9em; }
`;

async function toArrayBuffer(content: Blob | ArrayBuffer): Promise<ArrayBuffer> {
  return content instanceof Blob ? await content.arrayBuffer() : content;
}

export default function DocxPreview({
  content,
  filename,
  maxWidth = 800,
}: DocxPreviewProps) {
  const [html, setHtml] = useState("");
  const [warnings, setWarnings] = useState<string[]>([]);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setErr(null);
    setHtml("");
    setWarnings([]);
    void (async () => {
      try {
        // Legacy binary .doc isn't a zip — mammoth can't read it. Reject
        // up front with a friendlier message than the parser exception.
        if (filename.toLowerCase().endsWith(".doc") && !filename.toLowerCase().endsWith(".docx")) {
          if (cancelled) return;
          setErr("legacy .doc is not supported — save as .docx and retry");
          setLoading(false);
          return;
        }
        const arrayBuffer = await toArrayBuffer(content);
        if (cancelled) return;
        // Dynamic import keeps mammoth out of the initial bundle.
        // @ts-expect-error mammoth has no bundled types
        const mammothAny = await import("mammoth/mammoth.browser");
        const mammoth = mammothAny as {
          convertToHtml: (i: { arrayBuffer: ArrayBuffer }) => Promise<{
            value: string;
            messages?: Array<{ message?: string }>;
          }>;
        };
        const result = await mammoth.convertToHtml({ arrayBuffer });
        if (cancelled) return;
        const htmlValue = result.value || '<p style="color:#94a3b8">(empty document)</p>';
        const msgs = (result.messages || [])
          .map((m) => m?.message)
          .filter((s): s is string => typeof s === "string");
        setHtml(htmlValue);
        setWarnings(msgs);
        setLoading(false);
      } catch (e: unknown) {
        if (cancelled) return;
        const ae = e as { name?: string; message?: string };
        if (ae?.name === "AbortError") return;
        setErr(ae?.message || "parse failed — not a valid .docx file?");
        setLoading(false);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [content, filename]);

  if (loading) {
    return (
      <div className="pv-center">
        <div className="spin" style={{ width: 22, height: 22 }} />
      </div>
    );
  }
  if (err) {
    return (
      <div className="pv-center">
        <span className="err" style={{ textAlign: "center" }}>{err}</span>
      </div>
    );
  }

  const cardStyle: CSSProperties = {
    maxWidth,
    margin: "0 auto",
    background: "#fff",
    color: "#1a1a1a",
    padding: 40,
    boxShadow: "0 1px 4px rgba(0,0,0,0.25)",
    fontFamily: '"Microsoft YaHei", "PingFang SC", "Helvetica Neue", Arial, sans-serif',
    fontSize: 14,
    lineHeight: 1.7,
    wordBreak: "break-word",
  };

  return (
    <div className="pv-scroll">
      <style>{DOCX_CONTENT_CSS}</style>
      <div
        className="docx-preview-content"
        style={cardStyle}
        dangerouslySetInnerHTML={{ __html: html }}
      />
      {warnings.length > 0 && (
        <div className="docx-warn">
          {warnings.length} conversion note{warnings.length > 1 ? "s" : ""} (some styles may not be fully restored)
        </div>
      )}
    </div>
  );
}
