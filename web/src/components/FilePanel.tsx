import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { DirEntry } from "../gen/lyon/v1/file_pb";
import { FileKind } from "../gen/lyon/v1/file_pb";
import { errText, fileClient } from "../api";
import { joinPath } from "../filePreview";
import SkeletonRows from "./Skeleton";
import ScmView from "./ScmView";

// Dotfile visibility, remembered per browser (sibling-product convention):
// default hidden, like `ls` without -a.
const HIDE_DOTFILES_KEY = "lyon_files_hide_dot_v1";

// FilePanel is the workspace browser shell (plan Phase B): navigate,
// CRUD, upload, move. The embedded 45% preview moved out to standalone
// PreviewPane instances — clicking a file routes through onOpenFile,
// which App turns into a preview tab in the most fitting preview pane.

// One destructive ask. actions replace the sheet's buttons — delete gets
// a single danger confirm, an upload collision gets the usual trio
// (skip / overwrite / cancel), each entry fires its own run (sibling-product
// resolveConflicts: decisions are made before the RPCs, not per-file
// mid-flight).
type Confirm = {
  title: string;
  detail: string;
  actions: Array<{ label: string; danger?: boolean; run: () => Promise<void> | void }>;
};

export default function FilePanel({
  onOpenFile,
  nav,
}: {
  /** A workspace file was clicked; App opens it in a preview pane. */
  onOpenFile: (path: string, kind: FileKind) => void;
  /** External navigation request (markdown links, slash routing): load
   *  this directory. `seq` bumps per request so replaying the same path
   *  still re-reads it. */
  nav?: { path: string; seq: number } | null;
}) {
  const [path, setPath] = useState("");
  // browse = the file tree; scm = the source-control section (changes /
  // commit / sync / graph) — same panel, VS Code's sidebar toggle.
  const [mode, setMode] = useState<"browse" | "scm">("browse");
  const [entries, setEntries] = useState<DirEntry[]>([]);
  const [err, setErr] = useState("");
  const [loading, setLoading] = useState(true);
  // write-op UI state
  const [newKind, setNewKind] = useState<null | "file" | "dir">(null);
  const [newName, setNewName] = useState("");
  const [renaming, setRenaming] = useState(""); // entry name with an inline field open
  const [renameTo, setRenameTo] = useState("");
  const [confirm, setConfirm] = useState<Confirm | null>(null);
  const [moving, setMoving] = useState<null | { name: string; from: string }>(null);
  const [dragOver, setDragOver] = useState(false);
  // Context menu (sibling FileExplorer parity): right-click a row or the list
  // background. `entry` null = background (new file/folder only); `copied`
  // briefly flips a menu label to confirm a clipboard write.
  const [menu, setMenu] = useState<{
    x: number;
    y: number;
    entry: DirEntry | null;
    copied?: "rel" | "abs";
  } | null>(null);
  // Workspace absolute path (for copy-absolute-path) rides /healthz.
  const wsRoot = useRef("");
  useEffect(() => {
    let live = true;
    fetch("/healthz")
      .then((r) => r.json())
      .then((j: { workspace?: string }) => {
        if (live && j.workspace) wsRoot.current = j.workspace;
      })
      .catch(() => {});
    return () => {
      live = false;
    };
  }, []);
  const absOf = (rel: string) =>
    wsRoot.current ? `${wsRoot.current}\\${rel.split("/").join("\\")}` : rel;
  useEffect(() => {
    if (!menu) return;
    const close = () => setMenu(null);
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setMenu(null);
    };
    document.addEventListener("mousedown", close);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", close);
      document.removeEventListener("keydown", onKey);
    };
  }, [menu]);
  const copyTo = (text: string, which: "rel" | "abs") => {
    void navigator.clipboard.writeText(text).then(() => {
      setMenu((m) => (m ? { ...m, copied: which } : m));
      window.setTimeout(
        () =>
          setMenu((m) =>
            m && m.copied === which ? { ...m, copied: undefined } : m,
          ),
        900,
      );
    });
  };
  const startRename = (e: DirEntry) => {
    setRenaming(e.name);
    setRenameTo(e.name);
    setNewKind(null);
    setMenu(null);
  };
  const dragDepth = useRef(0); // enter/leave fires on child boundaries too
  const [hideDot, setHideDot] = useState<boolean>(() => {
    try {
      const v = localStorage.getItem(HIDE_DOTFILES_KEY);
      return v == null ? true : v === "1";
    } catch {
      return true;
    }
  });
  const toggleHideDot = () => {
    setHideDot((h) => {
      const next = !h;
      try {
        localStorage.setItem(HIDE_DOTFILES_KEY, next ? "1" : "0");
      } catch {
        // storage blocked: the toggle just won't survive reloads
      }
      return next;
    });
  };

  // What the list shows: dotfiles filtered (when the eye is off), then
  // folders above files, name ascending within each group — the classic
  // browser order; the server's raw order only feeds the rename/move/create
  // logic, never the pixels.
  const visible = useMemo(() => {
    const list = entries.filter((e) => !(hideDot && e.name.startsWith(".")));
    return [...list].sort((a, b) =>
      a.isDir === b.isDir
        ? a.name.localeCompare(b.name, undefined, { numeric: true, sensitivity: "base" })
        : a.isDir
          ? -1
          : 1,
    );
  }, [entries, hideDot]);

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

  // External nav requests (md links routing through App): load the dir
  // once per seq bump.
  const lastNavSeq = useRef(-1);
  useEffect(() => {
    if (!nav || nav.seq === lastNavSeq.current) return;
    lastNavSeq.current = nav.seq;
    void load(nav.path);
  }, [nav, load]);

  // Live workspace: the agent edits files while the panel watches — poll
  // the open directory every 5s and only re-render on a real diff
  // (file-events rule: root + open dir, diff name/size/mtime).
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

  const openFile = (e: DirEntry) =>
    onOpenFile(joinPath(path, e.name), e.kind ?? FileKind.TEXT);

  // ---- write ops (each: RPC → refresh; destructive ones pass through
  // the confirm sheet first) --------------------------------------------
  const afterWrite = async (run: () => Promise<void>) => {
    setErr("");
    try {
      await run();
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

  // Uploads check the target dir first (the sibling product's transfer): one
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

  const gotoDir = (p: string) => {
    void load(p);
  };

  const crumbs = path ? path.split("/") : [];

  return (
    <div className="files">
      <div className="files-head">
        <button className="ghost" onClick={() => gotoDir("")}>
          workspace
        </button>
        {crumbs.map((seg, i) => (
          <button
            key={i}
            className="ghost"
            onClick={() => gotoDir(crumbs.slice(0, i + 1).join("/"))}
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
        <button
          className="ghost"
          title={hideDot ? "show dotfiles (. prefix)" : "hide dotfiles (. prefix)"}
          onClick={toggleHideDot}
        >
          {hideDot ? "show ." : "hide ."}
        </button>
        <button
          className={"ghost" + (mode === "scm" ? " on" : "")}
          title="source control — changes, commit, sync, graph"
          onClick={() => setMode((m) => (m === "scm" ? "browse" : "scm"))}
        >
          git
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
      {mode === "scm" ? (
        <ScmView onOpenFile={onOpenFile} />
      ) : (
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
          // subtree has been exited (drag-counter rule, minus capture).
          dragDepth.current = Math.max(0, dragDepth.current - 1);
          if (dragDepth.current === 0) setDragOver(false);
        }}
        onDrop={(e) => {
          e.preventDefault();
          dragDepth.current = 0;
          setDragOver(false);
          void uploadFiles(e.dataTransfer.files);
        }}
        onContextMenu={(e) => {
          e.preventDefault();
          setMenu({ x: e.clientX, y: e.clientY, entry: null });
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
        ) : visible.length === 0 && path === "" ? null : (
          <>
            {path !== "" && (
              <button
                className="file-entry dir"
                onClick={() => gotoDir(crumbs.slice(0, -1).join("/"))}
              >
                ..
              </button>
            )}
            {visible.length === 0 && (
              <div className="placeholder file-empty">目录为空</div>
            )}
            {visible.map((e) => (
              <div
                key={e.name}
                className={"file-row" + (moving?.name === e.name && moving.from === path ? " moving" : "")}
                onContextMenu={(ev) => {
                  ev.preventDefault();
                  ev.stopPropagation();
                  setMenu({ x: ev.clientX, y: ev.clientY, entry: e });
                }}
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
                      onClick={() => (e.isDir ? gotoDir(joinPath(path, e.name)) : openFile(e))}
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
      )}
      {menu && (
        <div
          className="ctx-menu"
          style={{ left: menu.x, top: menu.y }}
          onMouseDown={(e) => e.stopPropagation()}
          onClick={(e) => e.stopPropagation()}
        >
          {menu.entry ? (
            <>
              <button
                onClick={() => {
                  const en = menu.entry!;
                  setMenu(null);
                  if (en.isDir) gotoDir(joinPath(path, en.name));
                  else openFile(en);
                }}
              >
                {menu.entry.isDir ? "打开目录" : "打开"}
              </button>
              {menu.entry.isDir && (
                <>
                  <button
                    onClick={() => {
                      setMenu(null);
                      setNewKind("file");
                      setNewName("");
                    }}
                  >
                    新建文件
                  </button>
                  <button
                    onClick={() => {
                      setMenu(null);
                      setNewKind("dir");
                      setNewName("");
                    }}
                  >
                    新建文件夹
                  </button>
                </>
              )}
              <div className="ctx-sep" />
              <button onClick={() => startRename(menu.entry!)}>重命名</button>
              <button
                onClick={() =>
                  copyTo(joinPath(path, menu.entry!.name), "rel")
                }
              >
                {menu.copied === "rel" ? "✓ 已复制路径" : "复制路径"}
              </button>
              <button
                onClick={() =>
                  copyTo(absOf(joinPath(path, menu.entry!.name)), "abs")
                }
              >
                {menu.copied === "abs" ? "✓ 已复制绝对路径" : "复制绝对路径"}
              </button>
              <div className="ctx-sep" />
              <button
                className="danger"
                onClick={() => {
                  const en = menu.entry!;
                  setMenu(null);
                  askDelete(en);
                }}
              >
                删除
              </button>
            </>
          ) : (
            <>
              <button
                onClick={() => {
                  setMenu(null);
                  setNewKind("file");
                  setNewName("");
                }}
              >
                新建文件
              </button>
              <button
                onClick={() => {
                  setMenu(null);
                  setNewKind("dir");
                  setNewName("");
                }}
              >
                新建文件夹
              </button>
            </>
          )}
        </div>
      )}
      {confirm && (
        <ConfirmSheet confirm={confirm} onCancel={() => setConfirm(null)} />
      )}
    </div>
  );
}

// ConfirmSheet: in-panel modal — backdrop click / Esc cancels, every
// decision is an explicit button (danger first, the sibling product's ok-button).
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

function formatSize(n: bigint | number): string {
  const v = typeof n === "bigint" ? Number(n) : n;
  if (v < 1024) return `${v}B`;
  if (v < 1024 * 1024) return `${(v / 1024).toFixed(1)}K`;
  return `${(v / 1024 / 1024).toFixed(1)}M`;
}
