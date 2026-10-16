import { lazy, Suspense, useCallback, useEffect, useRef, useState } from "react";
import type { DirEntry } from "../gen/lyon/v1/file_pb";
import { FileKind } from "../gen/lyon/v1/file_pb";
import { errText, fileClient } from "../api";
import { makeMdComponents, resolveRel } from "./mdkit";

// Shared with the transcript: the parser chunk loads on first md preview.
const MarkdownView = lazy(() => import("./Markdown"));

type Preview =
  | { kind: "text"; path: string; content: string; truncated: boolean; md: boolean }
  | { kind: "media"; path: string; url: string; contentType: string };

function joinPath(dir: string, name: string): string {
  return dir ? `${dir}/${name}` : name;
}
function dirnameOf(p: string): string {
  const i = p.lastIndexOf("/");
  return i < 0 ? "" : p.slice(0, i);
}

// One destructive ask. actions replace the sheet's buttons — delete gets
// a single danger confirm, an upload collision gets the portal trio
// (skip / overwrite / cancel), each entry fires its own run (portal
// resolveConflicts, FileExplorer.tsx:939 — decisions are made before the
// RPCs, not per-file mid-flight).
type Confirm = {
  title: string;
  detail: string;
  actions: Array<{ label: string; danger?: boolean; run: () => Promise<void> | void }>;
};

export default function FilePanel() {
  const [path, setPath] = useState("");
  const [entries, setEntries] = useState<DirEntry[]>([]);
  const [preview, setPreview] = useState<Preview | null>(null);
  const [err, setErr] = useState("");
  const [loading, setLoading] = useState(true);
  const [previewLoading, setPreviewLoading] = useState(false);
  // write-op UI state
  const [newKind, setNewKind] = useState<null | "file" | "dir">(null);
  const [newName, setNewName] = useState("");
  const [renaming, setRenaming] = useState(""); // entry name with an inline field open
  const [renameTo, setRenameTo] = useState("");
  const [confirm, setConfirm] = useState<Confirm | null>(null);
  const [moving, setMoving] = useState<null | { name: string; from: string }>(null);
  const [dragOver, setDragOver] = useState(false);
  const dragDepth = useRef(0); // enter/leave fires on child boundaries too

  const closePreview = () => {
    // Revoke the previous object URL so a long browsing session doesn't
    // pin every preview's bytes in memory.
    setPreview((p) => {
      if (p?.kind === "media") URL.revokeObjectURL(p.url);
      return null;
    });
  };

  const load = useCallback(async (p: string) => {
    setLoading(true);
    try {
      const res = await fileClient.listDirectory({ path: p });
      setEntries(res.entries);
      setPath(p);
      setErr("");
    } catch (e) {
      setErr(errText(e));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load("");
  }, [load]);

  // Live workspace: the agent edits files while the panel watches — poll
  // the open directory every 5s and only re-render on a real diff
  // (portal FileEventsContext rule: root + open dir, diff name/size/mtime).
  useEffect(() => {
    const iv = setInterval(() => {
      void (async () => {
        try {
          const res = await fileClient.listDirectory({ path });
          setEntries((prev) => {
            const same =
              prev.length === res.entries.length &&
              prev.every((a, b) => {
                const c = res.entries[b];
                return (
                  c &&
                  a.name === c.name &&
                  a.isDir === c.isDir &&
                  a.sizeBytes === c.sizeBytes &&
                  a.mtimeUnixMs === c.mtimeUnixMs
                );
              });
            return same ? prev : res.entries;
          });
        } catch {
          // transient poll failure: keep the last listing; the next tick retries
        }
      })();
    }, 5000);
    return () => clearInterval(iv);
  }, [path]);

  // Open any workspace path from a markdown link: directory wins, file
  // falls through to the preview reader.
  const openPath = async (p: string, kindHint?: FileKind) => {
    setErr("");
    try {
      const dir = await fileClient.listDirectory({ path: p });
      closePreview();
      setEntries(dir.entries);
      setPath(p);
      return;
    } catch {
      // not a directory (or unreadable) — treat as a file below
    }
    closePreview();
    setPreviewLoading(true);
    const kind = kindHint ?? FileKind.TEXT;
    try {
      if (kind === FileKind.IMAGE || kind === FileKind.PDF) {
        const res = await fileClient.readBinaryFile({ path: p });
        // fresh copy: Blob wants Uint8Array<ArrayBuffer>, the proto field is
        // typed ArrayBufferLike (could be SharedArrayBuffer in principle).
        const blob = new Blob([new Uint8Array(res.data)], {
          type: res.contentType || "application/octet-stream",
        });
        setPreview({
          kind: "media",
          path: p,
          url: URL.createObjectURL(blob),
          contentType: res.contentType,
        });
      } else if (kind === FileKind.OTHER) {
        // Unknown payload: text first (sniff happens server-side), fall
        // back to a download stub for the true binary.
        try {
          const res = await fileClient.readTextFile({ path: p });
          setPreview({ kind: "text", path: p, content: res.content, truncated: res.truncated, md: false });
        } catch {
          const res = await fileClient.readBinaryFile({ path: p });
          const blob = new Blob([new Uint8Array(res.data)], { type: res.contentType });
          setPreview({
            kind: "media",
            path: p,
            url: URL.createObjectURL(blob),
            contentType: res.contentType || "application/octet-stream",
          });
        }
      } else {
        const res = await fileClient.readTextFile({ path: p });
        setPreview({
          kind: "text",
          path: p,
          content: res.content,
          truncated: res.truncated,
          md: kind === FileKind.MARKDOWN,
        });
      }
    } catch (e) {
      closePreview();
      setErr(errText(e));
    } finally {
      setPreviewLoading(false);
    }
  };

  const openFile = (e: DirEntry) => void openPath(joinPath(path, e.name), e.kind);

  // ---- write ops (each: RPC → refresh; destructive ones pass through
  // the confirm sheet first) --------------------------------------------
  const afterWrite = async (run: () => Promise<void>) => {
    setErr("");
    try {
      await run();
      closePreview();
      await load(path);
    } catch (e) {
      setErr(errText(e));
    }
  };

  const createEntry = () => {
    const name = newName.trim();
    if (!name || !newKind) return;
    const target = joinPath(path, name);
    void afterWrite(async () => {
      if (newKind === "file") {
        await fileClient.writeFile({ path: target, content: "", createOnly: true });
      } else {
        await fileClient.mkdir({ path: target });
      }
    });
    setNewKind(null);
    setNewName("");
  };

  const renameEntry = (e: DirEntry) => {
    const to = renameTo.trim();
    if (!to || to === e.name) {
      setRenaming("");
      return;
    }
    void afterWrite(async () => {
      await fileClient.rename({ path: joinPath(path, e.name), newName: to });
    });
    setRenaming("");
  };

  const askDelete = (e: DirEntry) => {
    const target = joinPath(path, e.name);
    setConfirm({
      title: e.isDir ? "删除目录？" : "删除文件？",
      detail: e.isDir
        ? `确定删除「${target}」吗？其中的所有内容一并删除，此操作不可恢复。`
        : `确定删除「${target}」吗？此操作不可恢复。`,
      actions: [
        {
          label: "删除",
          danger: true,
          run: async () => {
            setErr("");
            try {
              await fileClient.delete({ path: target, recursive: e.isDir });
              closePreview();
              await load(path);
            } catch (ex) {
              setErr(errText(ex));
            }
          },
        },
        { label: "取消", run: async () => {} },
      ],
    });
  };

  const dropMoving = () => {
    if (!moving) return;
    const { name, from } = moving;
    void afterWrite(async () => {
      await fileClient.move({ path: joinPath(from, name), toDir: path });
    });
    setMoving(null);
  };

  // Uploads check the target dir first (portal performTransfer): one
  // collision sheet decides skip/overwrite/cancel for the whole drop
  // instead of erroring per file. Skip keeps the colliding names out.
  const uploadFiles = async (files: FileList | null) => {
    if (!files || files.length === 0) return;
    const list = Array.from(files);
    let existing = new Set<string>();
    try {
      const res = await fileClient.listDirectory({ path });
      existing = new Set(res.entries.map((e) => e.name));
    } catch {
      // unreadable dir: let the writes surface their own errors
    }
    const clashes = list.filter((f) => existing.has(f.name));
    const put = async (f: File, overwrite: boolean) => {
      try {
        const data = new Uint8Array(await f.arrayBuffer());
        await fileClient.uploadFile({
          path: joinPath(path, f.name),
          data,
          overwrite,
        });
        return "";
      } catch (e) {
        return `${f.name}: ${errText(e)}`;
      }
    };
    const runBatch = async (overwrite: boolean, skip: string[]) => {
      const failed: string[] = [];
      for (const f of list) {
        if (skip.includes(f.name)) continue;
        const msg = await put(f, overwrite || !existing.has(f.name));
        if (msg) failed.push(msg);
      }
      if (failed.length > 0) setErr(failed.join(" · "));
      closePreview();
      await load(path);
    };
    if (clashes.length === 0) {
      await runBatch(false, []);
      return;
    }
    setConfirm({
      title: `覆盖 ${clashes.length} 个已存在文件？`,
      detail: clashes.map((f) => f.name).join("、"),
      actions: [
        {
          label: "覆盖",
          danger: true,
          run: () => runBatch(true, []),
        },
        {
          label: "跳过冲突",
          run: () => runBatch(false, clashes.map((f) => f.name)),
        },
        { label: "取消", run: async () => {} },
      ],
    });
  };

  const nav = (p: string) => {
    closePreview();
    void load(p);
  };

  const mdLinks = makeMdComponents({
    onLocalPath: (href) => void openPath(resolveRel(dirnameOf(preview?.path ?? ""), href)),
    basePath: dirnameOf(preview?.path ?? ""),
  });

  const crumbs = path ? path.split("/") : [];

  return (
    <div className="files">
      <div className="files-head">
        <button className="ghost" onClick={() => nav("")}>
          workspace
        </button>
        {crumbs.map((seg, i) => (
          <button
            key={i}
            className="ghost"
            onClick={() => nav(crumbs.slice(0, i + 1).join("/"))}
          >
            /{seg}
          </button>
        ))}
      </div>
      <div className="files-actions">
        <button
          className="ghost"
          onClick={() => {
            setNewKind("file");
            setNewName("");
            setRenaming("");
          }}
        >
          + file
        </button>
        <button
          className="ghost"
          onClick={() => {
            setNewKind("dir");
            setNewName("");
            setRenaming("");
          }}
        >
          + dir
        </button>
        {moving ? (
          <span className="move-banner">
            moving <b>{moving.name}</b> →{" "}
            <button className="ghost" onClick={dropMoving}>
              drop here
            </button>
            <button className="ghost" onClick={() => setMoving(null)}>
              cancel
            </button>
          </span>
        ) : (
          <span className="files-hint">drag files in to upload</span>
        )}
      </div>
      {err && (
        <div className="file-err">
          <span className="err">{err}</span>
          <button className="ghost" onClick={() => { setErr(""); void load(path); }}>
            retry
          </button>
        </div>
      )}
      <div
        className={"files-list" + (dragOver ? " dragover" : "")}
        onDragEnter={(e) => {
          e.preventDefault();
          dragDepth.current += 1;
          setDragOver(true);
        }}
        onDragOver={(e) => e.preventDefault()}
        onDragLeave={() => {
          // one leave per child boundary too — close only when the whole
          // subtree has been exited (portal dragCounterRef, minus capture).
          dragDepth.current = Math.max(0, dragDepth.current - 1);
          if (dragDepth.current === 0) setDragOver(false);
        }}
        onDrop={(e) => {
          e.preventDefault();
          dragDepth.current = 0;
          setDragOver(false);
          void uploadFiles(e.dataTransfer.files);
        }}
      >
        {newKind && (
          <div className="file-inline">
            <input
              autoFocus
              placeholder={newKind === "file" ? "name.txt" : "directory"}
              value={newName}
              onChange={(e) => setNewName(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") createEntry();
                if (e.key === "Escape") setNewKind(null);
              }}
            />
            <button onClick={createEntry}>create</button>
            <button className="ghost" onClick={() => setNewKind(null)}>
              cancel
            </button>
          </div>
        )}
        {loading ? (
          <SkeletonRows n={8} />
        ) : entries.length === 0 && path === "" ? null : (
          <>
            {path !== "" && (
              <button
                className="file-entry dir"
                onClick={() => nav(crumbs.slice(0, -1).join("/"))}
              >
                ..
              </button>
            )}
            {entries.length === 0 && (
              <div className="placeholder file-empty">目录为空</div>
            )}
            {entries.map((e) => (
              <div
                key={e.name}
                className={"file-row" + (moving?.name === e.name && moving.from === path ? " moving" : "")}
              >
                {renaming === e.name ? (
                  <div className="file-inline">
                    <input
                      autoFocus
                      value={renameTo}
                      onChange={(ev) => setRenameTo(ev.target.value)}
                      onKeyDown={(ev) => {
                        if (ev.key === "Enter") renameEntry(e);
                        if (ev.key === "Escape") setRenaming("");
                      }}
                    />
                    <button onClick={() => renameEntry(e)}>rename</button>
                    <button className="ghost" onClick={() => setRenaming("")}>
                      cancel
                    </button>
                  </div>
                ) : (
                  <>
                    <button
                      className={"file-entry" + (e.isDir ? " dir" : "")}
                      onClick={() => (e.isDir ? nav(joinPath(path, e.name)) : openFile(e))}
                    >
                      <span>{e.name}</span>
                      <span className="file-meta">
                        {e.isDir ? "" : formatSize(e.sizeBytes)}
                      </span>
                    </button>
                    {!moving && (
                      <span className="file-ops">
                        <button
                          className="ghost"
                          title="rename"
                          onClick={() => {
                            setRenaming(e.name);
                            setRenameTo(e.name);
                            setNewKind(null);
                          }}
                        >
                          ✎
                        </button>
                        <button
                          className="ghost"
                          title="move"
                          onClick={() => setMoving({ name: e.name, from: path })}
                        >
                          ⇄
                        </button>
                        <button
                          className="ghost danger"
                          title="delete"
                          onClick={() => askDelete(e)}
                        >
                          ✗
                        </button>
                      </span>
                    )}
                  </>
                )}
              </div>
            ))}
          </>
        )}
      </div>
      {previewLoading && (
        <div className="files-preview">
          <SkeletonRows n={4} />
        </div>
      )}
      {preview?.kind === "text" && (
        <div className="files-preview">
          <div className="files-preview-head">
            {preview.path}
            {preview.truncated && " · truncated"}
          </div>
          {preview.md ? (
            <div className="md files-md">
              <Suspense fallback={<pre>{preview.content}</pre>}>
                <MarkdownView components={mdLinks}>{preview.content}</MarkdownView>
              </Suspense>
            </div>
          ) : (
            <pre>{preview.content}</pre>
          )}
        </div>
      )}
      {preview?.kind === "media" &&
        (preview.contentType === "application/pdf" ? (
          <iframe className="files-preview-frame" src={preview.url} title={preview.path} />
        ) : preview.contentType.startsWith("image/") ? (
          <ZoomImage url={preview.url} alt={preview.path} />
        ) : (
          <div className="files-preview">
            <div className="files-preview-head">
              {preview.path} · {preview.contentType}
            </div>
            <a className="file-dl" href={preview.url} download={preview.path.split("/").pop()}>
              download
            </a>
          </div>
        ))}
      {confirm && (
        <ConfirmSheet confirm={confirm} onCancel={() => setConfirm(null)} />
      )}
    </div>
  );
}

// ConfirmSheet: in-panel modal — backdrop click / Esc cancels, every
// decision is an explicit button (danger first, portal okButtonProps).
function ConfirmSheet({ confirm, onCancel }: { confirm: Confirm; onCancel: () => void }) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && onCancel();
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [onCancel]);
  return (
    <div className="dlg-backdrop" onClick={onCancel}>
      <div className="dlg" onClick={(e) => e.stopPropagation()}>
        <div className="dlg-title">{confirm.title}</div>
        <div className="dlg-detail">{confirm.detail}</div>
        <div className="dlg-actions">
          {confirm.actions.map((a) => (
            <button
              key={a.label}
              className={a.danger ? "danger" : "ghost"}
              onClick={() => void Promise.resolve(a.run()).finally(onCancel)}
            >
              {a.label}
            </button>
          ))}
        </div>
      </div>
    </div>
  );
}

// SkeletonRows: one shimmer block per would-be row (portal precedent —
// loading reads as structure, not as an empty panel).
function SkeletonRows({ n }: { n: number }) {
  return (
    <div className="skeleton-rows" aria-hidden="true">
      {Array.from({ length: n }, (_, i) => (
        <div key={i} className="skeleton-row" style={{ width: `${55 + ((i * 17) % 40)}%` }} />
      ))}
    </div>
  );
}

// ZoomImage: Ctrl+wheel zooms about the cursor (the content point under
// the pointer stays put — portal ImageView's anchor rule, algebraic
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
      className="img-zoom"
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

function formatSize(n: bigint | number): string {
  const v = typeof n === "bigint" ? Number(n) : n;
  if (v < 1024) return `${v}B`;
  if (v < 1024 * 1024) return `${(v / 1024).toFixed(1)}K`;
  return `${(v / 1024 / 1024).toFixed(1)}M`;
}
