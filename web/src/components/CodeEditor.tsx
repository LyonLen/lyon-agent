// CodeEditor — CodeMirror 6 editing surface for text/code/markdown
// previews (ported from the sibling product's CodeEditor, stripped to what lyon
// needs: no tabs cache — PreviewPane remounts per path via key —, no AI
// ghost text, no share/quote toolbar; save goes through FileService
// WriteFile).
//
// What came along because it is the whole point of the port:
//   - PyCharm-style diff gutter: Diff.diffLines(baseline, doc) → per-line
//     markers (added / modified / deleted-below), clicking one opens a
//     popup with the red/green hunk.
//   - Markdown rendered/source toggle: the CM host stays mounted
//     (visibility swap, not unmount) so the cursor/undo survive; the
//     rendered layer re-renders the live doc (remark-gfm + rehype-slug).
//   - Mod-s saves; dirty state is measured against the baseline field,
//     never assumed.
//
// Unsaved drafts survive tab switches in a module-level cache keyed by
// path; a successful save (or an external content reload) resets the
// baseline and drops the draft.

import {
  useCallback,
  useEffect,
  useRef,
  useState,
  Suspense,
} from "react";
import type { Components } from "react-markdown";
import {
  EditorState,
  StateField,
  StateEffect,
  RangeSet,
  type Range,
} from "@codemirror/state";
import {
  EditorView,
  keymap,
  gutter,
  GutterMarker,
  drawSelection,
  highlightActiveLine,
  highlightActiveLineGutter,
  lineNumbers,
} from "@codemirror/view";
import {
  defaultKeymap,
  history,
  historyKeymap,
  indentWithTab,
} from "@codemirror/commands";
import {
  indentUnit,
  syntaxHighlighting,
  defaultHighlightStyle,
  bracketMatching,
  foldGutter,
  foldKeymap,
  indentOnInput,
  type LanguageSupport,
} from "@codemirror/language";
import {
  autocompletion,
  completionKeymap,
  closeBrackets,
  closeBracketsKeymap,
} from "@codemirror/autocomplete";
import { oneDark } from "@codemirror/theme-one-dark";
import { javascript } from "@codemirror/lang-javascript";
import { python } from "@codemirror/lang-python";
import { json } from "@codemirror/lang-json";
import { yaml } from "@codemirror/lang-yaml";
import { markdown } from "@codemirror/lang-markdown";
import { html } from "@codemirror/lang-html";
import { css } from "@codemirror/lang-css";
import rehypeSlug from "rehype-slug";
import * as Diff from "diff";
import { errText, fileClient } from "../api";
import MarkdownView from "./Markdown";

// Module-level: a new array each render would make react-markdown treat
// the plugins as "changed" and re-run the mdast parse every keystroke.
const REHYPE_PLUGINS = [rehypeSlug];

// Per-file rendered/source preference, remembered across reloads
// (mdModeByPath pattern). Default: rendered.
const MD_MODE_KEY = "lyon_md_mode_v1";

function loadMdMode(path: string): "raw" | "rendered" {
  try {
    const rec = JSON.parse(localStorage.getItem(MD_MODE_KEY) ?? "{}") as Record<string, string>;
    return rec[path] === "raw" ? "raw" : "rendered";
  } catch {
    return "rendered";
  }
}

function storeMdMode(path: string, mode: "raw" | "rendered") {
  try {
    const rec = JSON.parse(localStorage.getItem(MD_MODE_KEY) ?? "{}") as Record<string, string>;
    rec[path] = mode;
    localStorage.setItem(MD_MODE_KEY, JSON.stringify(rec));
  } catch {
    // preference just won't persist
  }
}

// Unsaved drafts keyed by path — a tab switch remounts the editor, the
// draft keeps the in-flight edits (and the baseline they diff against).
const draftCache = new Map<string, { baseline: string; doc: string }>();

// CM chrome (non-syntax): gutter/active-line tints on lyon tokens, dark
// only (the web UI has no light theme). Syntax colors ride oneDark.
function buildCmChrome() {
  const accent = "77, 163, 255"; // --accent #4da3ff
  return EditorView.theme({
    "&": { height: "100%", fontSize: "12px" },
    ".cm-scroller": {
      fontFamily: "'Cascadia Code', 'Fira Code', 'JetBrains Mono', Consolas, monospace",
      lineHeight: "1.6",
      overflow: "auto",
    },
    ".cm-content": { padding: "6px 0", minHeight: "100%" },
    ".cm-gutters": {
      backgroundColor: "var(--bg-sunken)",
      color: `rgba(${accent}, 0.45)`,
      border: "none",
    },
    ".cm-gutter": { backgroundColor: "var(--bg-sunken)" },
    ".cm-lineNumbers .cm-gutterElement": { color: `rgba(${accent}, 0.45)` },
    ".cm-foldGutter .cm-gutterElement": { color: `rgba(${accent}, 0.55)` },
    ".cm-activeLine": { backgroundColor: `rgba(${accent}, 0.05)` },
    ".cm-activeLineGutter": {
      backgroundColor: `rgba(${accent}, 0.08)`,
      color: `rgba(${accent}, 0.95)`,
    },
  });
}

function pickLanguage(filename: string): LanguageSupport | null {
  const lower = filename.toLowerCase();
  if (lower === "package.json" || lower.endsWith(".json") || lower === "tsconfig.json") return json();
  if (lower.endsWith(".ts") || lower.endsWith(".tsx")) return javascript({ typescript: true, jsx: lower.endsWith(".tsx") });
  if (lower.endsWith(".js") || lower.endsWith(".jsx") || lower.endsWith(".mjs") || lower.endsWith(".cjs"))
    return javascript({ jsx: lower.endsWith(".jsx") });
  if (lower.endsWith(".py")) return python();
  if (lower.endsWith(".yaml") || lower.endsWith(".yml")) return yaml();
  if (lower.endsWith(".md") || lower.endsWith(".markdown")) return markdown();
  if (lower.endsWith(".html") || lower.endsWith(".htm") || lower.endsWith(".xml")) return html();
  if (lower.endsWith(".css") || lower.endsWith(".scss") || lower.endsWith(".less")) return css();
  return null;
}

/**
 * Diff.diffLines → the minimal set of non-overlapping ChangeSpec writes.
 * Unchanged runs are untouched, so a cursor sitting in them survives an
 * external rewrite (the naive prefix/suffix double-pointer collapses to
 * a whole-document replace when both ends differ — cursor falls to 0).
 * Returns null when the texts are identical (caller should skip).
 */
function computeMinimalChanges(oldText: string, newText: string):
  | { changes: { from: number; to: number; insert: string }[] }
  | null {
  if (oldText === newText) return null;
  const parts = Diff.diffLines(oldText, newText, { newlineIsToken: false });
  const changes: { from: number; to: number; insert: string }[] = [];
  let pos = 0; // offset into oldText
  for (let i = 0; i < parts.length; i++) {
    const p = parts[i];
    if (p.removed) {
      const next = parts[i + 1];
      if (next && next.added) {
        // removed immediately followed by added → one replacement write
        changes.push({ from: pos, to: pos + p.value.length, insert: next.value });
        pos += p.value.length;
        i++;
      } else {
        changes.push({ from: pos, to: pos + p.value.length, insert: "" });
        pos += p.value.length;
      }
    } else if (p.added) {
      changes.push({ from: pos, to: pos, insert: p.value });
      // pos unchanged: added spans consume no oldText
    } else {
      pos += p.value.length;
    }
  }
  if (changes.length === 0) return null;
  return { changes };
}

// ── Diff gutter (PyCharm-style change tracking) ─────────────────────────────

type DiffKind = "added" | "modified" | "deleted-below";

interface DiffHunk {
  kind: DiffKind;
  baselineLines: string[];
  currentLines: string[];
}

type DiffClickHandler = (lineIdx: number, e: MouseEvent) => void;

class DiffMarker extends GutterMarker {
  constructor(
    readonly kind: DiffKind,
    readonly lineIdx: number,
    readonly onClick: DiffClickHandler,
  ) {
    super();
  }
  override eq(other: GutterMarker) {
    return other instanceof DiffMarker && other.kind === this.kind && other.lineIdx === this.lineIdx;
  }
  override toDOM() {
    const el = document.createElement("div");
    el.className = `cm-diff-mark cm-diff-${this.kind}`;
    el.addEventListener("mousedown", (e) => e.preventDefault());
    el.addEventListener("click", (e) => {
      e.stopPropagation();
      this.onClick(this.lineIdx, e);
    });
    return el;
  }
}

const setBaselineEffect = StateEffect.define<string>();

const baselineField = StateField.define<string>({
  create: () => "",
  update(value, tr) {
    for (const e of tr.effects) if (e.is(setBaselineEffect)) return e.value;
    return value;
  },
});

function buildDiffMarkers(
  view: EditorView,
  baseline: string,
  hunks: Map<number, DiffHunk>,
  onClick: DiffClickHandler,
): RangeSet<GutterMarker> {
  hunks.clear();
  const doc = view.state.doc;
  const current = doc.toString();
  if (!baseline || current === baseline) return RangeSet.empty;

  const parts = Diff.diffLines(baseline, current, { newlineIsToken: false });
  const lineKind: ("added" | "modified" | "unchanged")[] = [];
  const deletionAfter: Map<number, DiffHunk> = new Map();

  const splitLines = (s: string): string[] =>
    s ? s.replace(/\n$/, "").split("\n") : [];

  let curLineIdx = 0;
  for (let i = 0; i < parts.length; i++) {
    const part = parts[i];
    const next = parts[i + 1];
    const lineCount = part.count ?? part.value.split("\n").length - 1;
    if (part.added) {
      const isModified = i > 0 && parts[i - 1].removed;
      const baselineText = isModified ? parts[i - 1].value : "";
      const hunk: DiffHunk = {
        kind: isModified ? "modified" : "added",
        baselineLines: splitLines(baselineText),
        currentLines: splitLines(part.value),
      };
      for (let k = 0; k < lineCount; k++) {
        lineKind.push(isModified ? "modified" : "added");
        hunks.set(curLineIdx + k, hunk);
      }
      curLineIdx += lineCount;
    } else if (part.removed) {
      if (!next || !next.added) {
        // pure deletion: anchor the marker on the surviving line above
        const anchor = curLineIdx > 0 ? curLineIdx - 1 : 0;
        const hunk: DiffHunk = {
          kind: "deleted-below",
          baselineLines: splitLines(part.value),
          currentLines: [],
        };
        deletionAfter.set(anchor, hunk);
        if (!hunks.has(anchor)) hunks.set(anchor, hunk);
      }
    } else {
      for (let k = 0; k < lineCount; k++) lineKind.push("unchanged");
      curLineIdx += lineCount;
    }
  }

  const totalLines = doc.lines;
  const ranges: Range<GutterMarker>[] = [];
  for (let i = 0; i < totalLines; i++) {
    const kind = lineKind[i];
    const line = doc.line(i + 1);
    if (kind === "added" || kind === "modified") {
      ranges.push(new DiffMarker(kind, i, onClick).range(line.from));
    }
    if (deletionAfter.has(i)) {
      ranges.push(new DiffMarker("deleted-below", i, onClick).range(line.from));
    }
  }
  return RangeSet.of(ranges, true);
}

// ── Component ───────────────────────────────────────────────────────────────

export default function CodeEditor({
  path,
  content,
  md,
  mdComponents,
}: {
  path: string;
  /** Text as last fetched — an external change re-diffs against this. */
  content: string;
  /** Markdown file: rendered/source toggle (defaults to rendered). */
  md?: boolean;
  /** Renderer components from PreviewPane (local-path link routing). */
  mdComponents?: Components;
}) {
  const bodyRef = useRef<HTMLDivElement | null>(null);
  const viewRef = useRef<EditorView | null>(null);
  const resizeObsRef = useRef<ResizeObserver | null>(null);
  const diffHunksRef = useRef(new Map<number, DiffHunk>());
  const diffClickRef = useRef<DiffClickHandler>(() => {});
  const saveRef = useRef<() => void>(() => {});
  const lastLoadedRef = useRef(content);

  const [dirty, setDirty] = useState(false);
  const [saving, setSaving] = useState(false);
  const [flash, setFlash] = useState("");
  const [mode, setMode] = useState<"raw" | "rendered">(() =>
    md ? loadMdMode(path) : "raw",
  );
  // Start from a surviving draft (tab switch remounts us) so the rendered
  // markdown and the dirty dot agree with the doc before the first edit.
  const [docText, setDocText] = useState(() => draftCache.get(path)?.doc ?? content);
  const [popup, setPopup] = useState<{ hunk: DiffHunk; top: number; left: number } | null>(null);

  // ── Markdown「本页目录」rail (sibling-product parity) ──────────────────────
  // Headings are collected from the RENDERED DOM (rehype-slug owns the ids;
  // reading the elements keeps scroll targets and TOC rows in exact
  // correspondence — no source regex to drift from the renderer).
  const [toc, setToc] = useState<{ text: string; level: number; el: HTMLElement }[]>([]);
  const [tocActive, setTocActive] = useState(0);
  // Click lock: the tail section's remaining content is shorter than the
  // viewport, so scrollIntoView clamps (zero scroll events) and the
  // at-bottom rule would immediately outshout the click — hold the clicked
  // item until the user interacts with the content themselves.
  const tocLockRef = useRef<number | null>(null);
  const mdRenderedRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (!md || mode !== "rendered") {
      setToc([]);
      return;
    }
    // One frame so ReactMarkdown's DOM has committed before collecting.
    const raf = requestAnimationFrame(() => {
      const host = mdRenderedRef.current;
      if (!host) return;
      const els = Array.from(host.querySelectorAll<HTMLElement>("h1, h2, h3, h4, h5, h6"));
      setToc(els.map((el) => ({ text: (el.textContent ?? "").trim(), level: Number(el.tagName[1]), el })));
      setTocActive(0);
      tocLockRef.current = null;
    });
    return () => cancelAnimationFrame(raf);
  }, [md, mode, docText, path]);

  // Scroll-spy: nearest heading above the 90px baseline (> the 12px
  // scroll-margin, so a jump doesn't highlight the previous section).
  // Bottom rule: a tail section's heading can never cross the baseline
  // (its content is shorter than the viewport) — activate the last one
  // when the scrollable container is actually scrollable and at the end.
  const handleTocScroll = useCallback(() => {
    setTocActive((prev) => {
      const host = mdRenderedRef.current;
      if (!host) return prev;
      if (tocLockRef.current != null) return prev;
      const atBottom =
        host.scrollHeight - host.clientHeight > 4 &&
        host.scrollTop + host.clientHeight >= host.scrollHeight - 2;
      if (atBottom) {
        const last = toc.length - 1;
        return last < 0 || last === prev ? prev : last;
      }
      const line = host.scrollTop + 90;
      let idx = 0;
      for (let i = 0; i < toc.length; i++) {
        if (toc[i].el.offsetTop <= line) idx = i;
        else break;
      }
      return idx === prev ? prev : idx;
    });
  }, [toc]);
  // Any user interaction with the content hands the highlight back to the spy.
  const releaseTocLock = useCallback(() => {
    tocLockRef.current = null;
  }, []);
  const tocJump = useCallback((i: number, el: HTMLElement) => {
    tocLockRef.current = i;
    setTocActive(i);
    el.scrollIntoView({ behavior: "smooth", block: "start" });
  }, []);

  const flashMsg = useCallback((m: string) => {
    setFlash(m);
    window.setTimeout(() => setFlash((cur) => (cur === m ? "" : cur)), 2000);
  }, []);

  // ── Save (Mod-s keymap + the bar button share this) ───────────────────────
  const save = useCallback(async () => {
    const view = viewRef.current;
    if (!view || saving) return;
    const text = view.state.doc.toString();
    setSaving(true);
    try {
      await fileClient.writeFile({ path, content: text });
      view.dispatch({ effects: setBaselineEffect.of(text) });
      draftCache.delete(path);
      setDirty(false);
      flashMsg("saved");
    } catch (e) {
      flashMsg(errText(e));
    } finally {
      setSaving(false);
    }
  }, [path, saving, flashMsg]);
  saveRef.current = () => void save();

  // ── Diff-gutter click → popup near the click, clamped to the body ─────────
  diffClickRef.current = (lineIdx: number, e: MouseEvent) => {
    const hunk = diffHunksRef.current.get(lineIdx);
    const body = bodyRef.current;
    if (!hunk || !body) return;
    const rect = body.getBoundingClientRect();
    const width = Math.min(560, Math.max(220, rect.width - 16));
    const left = Math.max(8, Math.min(e.clientX - rect.left - 12, rect.width - width - 8));
    const top = Math.min(e.clientY - rect.top + 8, Math.max(8, rect.height - 160));
    setPopup({ hunk, top, left });
  };

  // ── Mount the EditorView once (per path — the parent keys us) ─────────────
  const setHostRef = useCallback((node: HTMLDivElement | null) => {
    if (node && !viewRef.current) {
      const initial = draftCache.get(path) ?? { baseline: content, doc: content };
      const lang = pickLanguage(path);
      const view = new EditorView({
        doc: initial.doc,
        parent: node,
        extensions: [
          lineNumbers(),
          highlightActiveLineGutter(),
          history(),
          drawSelection(),
          indentOnInput(),
          bracketMatching(),
          closeBrackets(),
          autocompletion(),
          foldGutter(),
          highlightActiveLine(),
          indentUnit.of("  "),
          EditorState.tabSize.of(2),
          baselineField,
          gutter({
            class: "cm-diff-gutter",
            markers: (v) =>
              buildDiffMarkers(
                v,
                v.state.field(baselineField),
                diffHunksRef.current,
                (idx, e) => diffClickRef.current(idx, e),
              ),
          }),
          syntaxHighlighting(defaultHighlightStyle, { fallback: true }),
          oneDark,
          buildCmChrome(),
          keymap.of([
            {
              key: "Mod-s",
              preventDefault: true,
              run: () => {
                saveRef.current();
                return true;
              },
            },
          ]),
          keymap.of([
            ...defaultKeymap,
            ...historyKeymap,
            ...closeBracketsKeymap,
            ...completionKeymap,
            ...foldKeymap,
            indentWithTab,
          ]),
          ...(lang ? [lang] : []),
          EditorView.lineWrapping,
          EditorView.updateListener.of((u) => {
            if (u.docChanged) {
              const text = u.state.doc.toString();
              const base = u.state.field(baselineField);
              draftCache.set(path, { baseline: base, doc: text });
              setDirty(text !== base);
              setDocText(text);
              setPopup(null); // the doc moved under the popup — stale
            }
          }),
        ],
      });
      view.dispatch({ effects: setBaselineEffect.of(initial.baseline) });
      viewRef.current = view;
      // a surviving draft may already differ from its baseline — the dirty
      // dot must be lit before the first keystroke
      if (initial.doc !== initial.baseline) setDirty(true);
      if (typeof ResizeObserver !== "undefined") {
        resizeObsRef.current = new ResizeObserver(() => view.requestMeasure());
        resizeObsRef.current.observe(node);
      }
    } else if (!node && viewRef.current) {
      if (resizeObsRef.current) {
        resizeObsRef.current.disconnect();
        resizeObsRef.current = null;
      }
      viewRef.current.destroy();
      viewRef.current = null;
    }
    // path/content are mount-time inputs; later changes arrive via the
    // external-reload effect below
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [path]);

  // ── External content change (retry / panel reload): minimal-write the doc
  // so cursors in unchanged runs survive, then re-baseline.
  useEffect(() => {
    if (content === lastLoadedRef.current) return;
    lastLoadedRef.current = content;
    const view = viewRef.current;
    if (!view) return;
    const cur = view.state.doc.toString();
    const mc = computeMinimalChanges(cur, content);
    if (mc) {
      view.dispatch({ changes: mc.changes, effects: setBaselineEffect.of(content) });
    } else {
      view.dispatch({ effects: setBaselineEffect.of(content) });
    }
    draftCache.delete(path);
    setDirty(false);
    setDocText(content);
    flashMsg("reloaded");
  }, [content, path, flashMsg]);

  // Close the popup when focus/click wanders away from it.
  useEffect(() => {
    if (!popup) return;
    const onDown = (e: PointerEvent) => {
      const el = e.target as HTMLElement | null;
      if (el?.closest(".diff-popup") || el?.closest(".cm-diff-mark")) return;
      setPopup(null);
    };
    document.addEventListener("pointerdown", onDown);
    return () => document.removeEventListener("pointerdown", onDown);
  }, [popup]);

  const switchMode = (m: "raw" | "rendered") => {
    if (m === mode) return;
    setMode(m);
    storeMdMode(path, m);
  };

  const baseName = path.split("/").pop() ?? path;

  return (
    <div className="ce">
      <div className="ce-bar">
        <span className="ce-path" title={path}>
          {baseName}
        </span>
        {dirty && <span className="ce-dirty" title="unsaved changes">●</span>}
        <span className="ce-spacer" />
        {flash && <span className={"ce-flash" + (flash === "saved" ? " ok" : "")}>{flash}</span>}
        {dirty && (
          <button className="ghost ce-save" disabled={saving} onClick={() => void save()}>
            {saving ? "saving…" : "save"}
          </button>
        )}
        {md && (
          <span className="ce-seg">
            <button className={mode === "rendered" ? "active" : ""} onClick={() => switchMode("rendered")}>
              preview
            </button>
            <button className={mode === "raw" ? "active" : ""} onClick={() => switchMode("raw")}>
              source
            </button>
          </span>
        )}
      </div>
      <div className="ce-body" ref={bodyRef}>
        <div
          ref={setHostRef}
          className="cm-host"
          style={{
            position: "absolute",
            inset: 0,
            // The CM host never unmounts (cursor/undo survive mode flips);
            // the rendered layer simply covers it. opacity 1 always — a
            // dim flash would be visible on every toggle.
            visibility: md && mode === "rendered" ? "hidden" : "visible",
          }}
        />
        {md && mode === "rendered" && (
          <div className="ce-md">
            <div
              className="ce-rendered md"
              ref={mdRenderedRef}
              onScroll={handleTocScroll}
              onWheel={releaseTocLock}
              onTouchStart={releaseTocLock}
              onMouseDown={releaseTocLock}
            >
              <Suspense fallback={<pre className="pv-pre">{docText}</pre>}>
                <MarkdownView components={mdComponents} rehypePlugins={REHYPE_PLUGINS}>
                  {docText}
                </MarkdownView>
              </Suspense>
            </div>
            {/* 本页目录: a sibling product's right rail — hidden under 3 headings (a
                two-item rail is noise, matching that threshold) */}
            {toc.length >= 3 && (
              <div className="md-toc-rail">
                <div className="md-toc-title">本页目录</div>
                {toc.map((h, i) => (
                  <button
                    key={i}
                    className={"md-toc-item" + (i === tocActive ? " active" : "") + (h.level >= 3 ? " deep" : "")}
                    title={h.text}
                    onClick={() => tocJump(i, h.el)}
                  >
                    {h.text}
                  </button>
                ))}
              </div>
            )}
          </div>
        )}
        {popup && (
          <div
            className="diff-popup"
            style={{ top: popup.top, left: popup.left, width: Math.min(560, (bodyRef.current?.clientWidth ?? 560) - 16) }}
          >
            <div className="diff-popup-head">
              <span className={"diff-popup-title " + popup.hunk.kind}>
                {popup.hunk.kind === "added" && `+ added ${popup.hunk.currentLines.length} line(s)`}
                {popup.hunk.kind === "modified" && `~ modified (-${popup.hunk.baselineLines.length} +${popup.hunk.currentLines.length})`}
                {popup.hunk.kind === "deleted-below" && `- deleted ${popup.hunk.baselineLines.length} line(s)`}
              </span>
              <button className="diff-popup-x" onClick={() => setPopup(null)} aria-label="close">
                ×
              </button>
            </div>
            <div className="diff-popup-body">
              {popup.hunk.baselineLines.length > 0 && (
                <div className="diff-side">
                  {popup.hunk.baselineLines.map((line, idx) => (
                    <div key={`b${idx}`} className="diff-line del">
                      <span className="diff-sign">-</span>{line || " "}
                    </div>
                  ))}
                </div>
              )}
              {popup.hunk.currentLines.length > 0 && (
                <div className="diff-side">
                  {popup.hunk.currentLines.map((line, idx) => (
                    <div key={`c${idx}`} className="diff-line add">
                      <span className="diff-sign">+</span>{line || " "}
                    </div>
                  ))}
                </div>
              )}
            </div>
          </div>
        )}
      </div>
    </div>
  );
}
