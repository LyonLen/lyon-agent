import { useCallback, useEffect, useState } from "react";
import type { DirEntry } from "../gen/lyon/v1/file_pb";
import { FileKind } from "../gen/lyon/v1/file_pb";
import { errText, fileClient } from "../api";

type Preview =
  | { kind: "text"; path: string; content: string; truncated: boolean }
  | { kind: "media"; path: string; url: string; contentType: string };

function joinPath(dir: string, name: string): string {
  return dir ? `${dir}/${name}` : name;
}

export default function FilePanel() {
  const [path, setPath] = useState("");
  const [entries, setEntries] = useState<DirEntry[]>([]);
  const [preview, setPreview] = useState<Preview | null>(null);
  const [err, setErr] = useState("");

  const load = useCallback(async (p: string) => {
    try {
      const res = await fileClient.listDirectory({ path: p });
      setEntries(res.entries);
      setPath(p);
      setErr("");
    } catch (e) {
      setErr(errText(e));
    }
  }, []);

  useEffect(() => {
    void load("");
  }, [load]);

  const openFile = async (e: DirEntry) => {
    const p = joinPath(path, e.name);
    setErr("");
    // Revoke the previous object URL so a long browsing session doesn't
    // pin every preview's bytes in memory.
    if (preview?.kind === "media") {
      URL.revokeObjectURL(preview.url);
    }
    try {
      if (e.kind === FileKind.IMAGE || e.kind === FileKind.PDF) {
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
      } else {
        const res = await fileClient.readTextFile({ path: p });
        setPreview({
          kind: "text",
          path: p,
          content: res.content,
          truncated: res.truncated,
        });
      }
    } catch (e) {
      setPreview(null);
      setErr(errText(e));
    }
  };

  const crumbs = path ? path.split("/") : [];

  return (
    <div className="files">
      <div className="files-head">
        <button className="ghost" onClick={() => void load("")}>
          workspace
        </button>
        {crumbs.map((seg, i) => (
          <button
            key={i}
            className="ghost"
            onClick={() => void load(crumbs.slice(0, i + 1).join("/"))}
          >
            /{seg}
          </button>
        ))}
      </div>
      {err && <div className="err">{err}</div>}
      <div className="files-list">
        {path !== "" && (
          <button
            className="file-entry dir"
            onClick={() => void load(crumbs.slice(0, -1).join("/"))}
          >
            ..
          </button>
        )}
        {entries.map((e) => (
          <button
            key={e.name}
            className={"file-entry" + (e.isDir ? " dir" : "")}
            onClick={() => (e.isDir ? void load(joinPath(path, e.name)) : void openFile(e))}
          >
            <span>{e.name}</span>
            <span className="file-meta">
              {e.isDir ? "" : formatSize(e.sizeBytes)}
            </span>
          </button>
        ))}
      </div>
      {preview?.kind === "text" && (
        <div className="files-preview">
          <div className="files-preview-head">
            {preview.path}
            {preview.truncated && " · truncated"}
          </div>
          <pre>{preview.content}</pre>
        </div>
      )}
      {preview?.kind === "media" &&
        (preview.contentType === "application/pdf" ? (
          <iframe className="files-preview-frame" src={preview.url} title={preview.path} />
        ) : (
          <img className="files-preview-img" src={preview.url} alt={preview.path} />
        ))}
    </div>
  );
}

function formatSize(n: bigint | number): string {
  const v = typeof n === "bigint" ? Number(n) : n;
  if (v < 1024) return `${v}B`;
  if (v < 1024 * 1024) return `${(v / 1024).toFixed(1)}K`;
  return `${(v / 1024 / 1024).toFixed(1)}M`;
}
