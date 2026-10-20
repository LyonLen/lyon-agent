// File preview fetching, extracted from FilePanel when the embedded 45%
// preview became standalone PreviewPane instances (plan Phase B). Pure
// logic — no React, no state: given a path, produce a renderable Preview
// or throw errText-able errors. The four text/media/unknown branches are
// the ones that used to live in FilePanel.openPath.

import { FileKind } from "./gen/lyon/v1/file_pb";
import { fileClient } from "./api";

export type OfficeKind = "pptx" | "docx" | "xlsx";

export type Preview =
  | { kind: "text"; path: string; content: string; truncated: boolean; md: boolean }
  | { kind: "media"; path: string; url: string; contentType: string }
  | { kind: "office"; path: string; office: OfficeKind; data: ArrayBuffer; truncated: boolean };

export function joinPath(dir: string, name: string): string {
  return dir ? `${dir}/${name}` : name;
}

export function dirnameOf(p: string): string {
  const i = p.lastIndexOf("/");
  return i < 0 ? "" : p.slice(0, i);
}

/**
 * Read a file into a renderable Preview. kindHint comes from the DirEntry
 * when clicked from a listing; unknown/OTHER sniffs text first (the
 * server-classified kind) and falls back to a binary download stub.
 * Media URLs are object URLs — the caller revokes them on unmount/tab
 * close (see usePreviewResource).
 */
export async function fetchPreview(p: string, kindHint?: FileKind): Promise<Preview> {
  const kind = kindHint ?? FileKind.TEXT;
  if (kind === FileKind.PPTX || kind === FileKind.DOCX || kind === FileKind.XLSX) {
    const res = await fileClient.readBinaryFile({ path: p });
    return {
      kind: "office",
      path: p,
      office: kind === FileKind.PPTX ? "pptx" : kind === FileKind.DOCX ? "docx" : "xlsx",
      // fresh copy: the proto field is typed ArrayBufferLike (could be
      // SharedArrayBuffer in principle) — viewers want a plain buffer
      data: new Uint8Array(res.data).buffer,
      truncated: res.truncated,
    };
  }
  if (kind === FileKind.IMAGE || kind === FileKind.PDF) {
    const res = await fileClient.readBinaryFile({ path: p });
    // fresh copy: Blob wants Uint8Array<ArrayBuffer>, the proto field is
    // typed ArrayBufferLike (could be SharedArrayBuffer in principle).
    const blob = new Blob([new Uint8Array(res.data)], {
      type: res.contentType || "application/octet-stream",
    });
    return {
      kind: "media",
      path: p,
      url: URL.createObjectURL(blob),
      contentType: res.contentType,
    };
  }
  if (kind === FileKind.OTHER) {
    try {
      const res = await fileClient.readTextFile({ path: p });
      return { kind: "text", path: p, content: res.content, truncated: res.truncated, md: false };
    } catch {
      const res = await fileClient.readBinaryFile({ path: p });
      const blob = new Blob([new Uint8Array(res.data)], { type: res.contentType });
      return {
        kind: "media",
        path: p,
        url: URL.createObjectURL(blob),
        contentType: res.contentType || "application/octet-stream",
      };
    }
  }
  const res = await fileClient.readTextFile({ path: p });
  return {
    kind: "text",
    path: p,
    content: res.content,
    truncated: res.truncated,
    md: kind === FileKind.MARKDOWN,
  };
}

/** Revoke an object-URL-backed preview (no-op for text). */
export function revokePreview(p: Preview | null): void {
  if (p?.kind === "media") URL.revokeObjectURL(p.url);
}

/** True when the path looks like a previewable file (not a directory hop). */
export function isPreviewPath(_path: string, kindHint?: FileKind): boolean {
  return kindHint !== FileKind.DIR;
}
