// Clipboard image attachments for the web composer. The browser already
// holds the pixels, but the model only ever sees workspace paths — so a
// pasted picture is uploaded into <workspace>/.lyon/paste and handed to the
// composer as an "@path" token, exactly like the TUI's alt+v (cmd/lyon/
// paste.go → tools.PasteImage) and exactly like typing @shot.png by hand.
// PromptBlocks resolves that token into a real image block at submit time.
//
// Two server contracts shape this file:
//   - UploadFile does NOT mkdir -p, so the paste directory is created first.
//   - The model rejects images over 3.5MB or 8000px a side; UploadFile now
//     allows up to the 32MB binary cap, so an oversized clipboard bitmap
//     would upload fine and only blow up one keystroke later. It is fitted
//     here, before the upload, with the same ladder as tools/fitImage:
//     shrink to the pixel limit, then PNG, then JPEG q85, then halve again.

import { Code, ConnectError } from "@connectrpc/connect";
import { fileClient } from "./api";
import { joinPath } from "./filePreview";

const PASTE_DIR = ".lyon/paste";
/** Mirror of tools.pasteKeep: history replay re-resolves @paths, so old
 *  attachments are only surplus beyond this many, never deleted eagerly. */
const PASTE_KEEP = 32;
/** Mirror of tools.maxImageBytes / maxImageDim (the API's own limits). */
const MAX_IMAGE_BYTES = 3_500_000;
const MAX_IMAGE_DIM = 8000;

const EXT: Record<string, string> = {
  "image/png": ".png",
  "image/jpeg": ".jpg",
  "image/gif": ".gif",
  "image/webp": ".webp",
};

/** Magic-byte sniff for the case where the clipboard item has no MIME type
 *  (some Windows apps hand over an unnamed blob). */
function sniffImage(b: Uint8Array): string {
  if (b.length >= 8 && b[0] === 0x89 && b[1] === 0x50 && b[2] === 0x4e && b[3] === 0x47) {
    return "image/png";
  }
  if (b.length >= 3 && b[0] === 0xff && b[1] === 0xd8 && b[2] === 0xff) return "image/jpeg";
  if (b.length >= 6) {
    const head = String.fromCharCode(...b.slice(0, 6));
    if (head === "GIF87a" || head === "GIF89a") return "image/gif";
  }
  if (b.length >= 12) {
    const riff = String.fromCharCode(...b.slice(0, 4));
    const webp = String.fromCharCode(...b.slice(8, 12));
    if (riff === "RIFF" && webp === "WEBP") return "image/webp";
  }
  return "";
}

async function encode(
  bmp: ImageBitmap,
  w: number,
  h: number,
  type: string,
  quality?: number,
): Promise<Uint8Array> {
  const canvas = document.createElement("canvas");
  canvas.width = w;
  canvas.height = h;
  const ctx = canvas.getContext("2d");
  if (!ctx) throw new Error("canvas 2d context unavailable");
  if (type === "image/jpeg") {
    // JPEG has no alpha; without a white bed every transparent region
    // would come out black (tools.flattenWhite does the same server-side).
    ctx.fillStyle = "#fff";
    ctx.fillRect(0, 0, w, h);
  }
  ctx.drawImage(bmp, 0, 0, w, h);
  const blob = await new Promise<Blob>((resolve, reject) => {
    canvas.toBlob(
      (b) => (b ? resolve(b) : reject(new Error("image encode failed"))),
      type,
      quality,
    );
  });
  return new Uint8Array(await blob.arrayBuffer());
}

/** Bytes + extension for one clipboard file, guaranteed to fit what the
 *  model accepts. Images that already fit are passed through untouched —
 *  re-encoding a small screenshot would be a lossy round trip for nothing. */
async function fitImage(file: File): Promise<{ data: Uint8Array; ext: string }> {
  const raw = new Uint8Array(await file.arrayBuffer());
  const sniffed = EXT[file.type] ? file.type : sniffImage(raw);
  if (!sniffed) throw new Error("clipboard item is not png, jpeg, gif or webp");
  const type = sniffed;

  const bmp = await createImageBitmap(new Blob([raw], { type })).catch(() => null);
  if (!bmp) {
    // Undecodable here (no canvas/decoder for the type): the byte cap is
    // still checkable, the pixel cap is the model's call.
    if (raw.length <= MAX_IMAGE_BYTES) return { data: raw, ext: EXT[type] };
    throw new Error(
      `image is ${raw.length} bytes; at most ${MAX_IMAGE_BYTES} bytes fit in one request`,
    );
  }
  try {
    if (raw.length <= MAX_IMAGE_BYTES && bmp.width <= MAX_IMAGE_DIM && bmp.height <= MAX_IMAGE_DIM) {
      return { data: raw, ext: EXT[type] }; // already fits: pass through verbatim
    }
    let factor = 1;
    while (
      Math.ceil(bmp.width / factor) > MAX_IMAGE_DIM ||
      Math.ceil(bmp.height / factor) > MAX_IMAGE_DIM
    ) {
      factor++;
    }
    for (;;) {
      const w = Math.max(1, Math.round(bmp.width / factor));
      const h = Math.max(1, Math.round(bmp.height / factor));
      const png = await encode(bmp, w, h, "image/png");
      if (png.length <= MAX_IMAGE_BYTES) return { data: png, ext: ".png" };
      const jpeg = await encode(bmp, w, h, "image/jpeg", 0.85);
      if (jpeg.length <= MAX_IMAGE_BYTES) return { data: jpeg, ext: ".jpg" };
      if (w <= 16 || h <= 16 || factor >= 1 << 12) {
        throw new Error(
          `image is ${raw.length} bytes; even re-encoded it exceeds the ${MAX_IMAGE_BYTES} byte limit`,
        );
      }
      factor *= 2;
    }
  } finally {
    bmp.close();
  }
}

/** Local-time paste-YYYYMMDD-HHMMSS, the TUI's naming, so the two surfaces
 *  produce the same sortable filenames in one shared directory. */
function stamp(now: Date): string {
  const p = (n: number) => String(n).padStart(2, "0");
  return (
    `${now.getFullYear()}${p(now.getMonth() + 1)}${p(now.getDate())}` +
    `-${p(now.getHours())}${p(now.getMinutes())}${p(now.getSeconds())}`
  );
}

function isAlreadyExists(e: unknown): boolean {
  return ConnectError.from(e).code === Code.AlreadyExists;
}

/** Upload one clipboard file and return its workspace-relative path. */
export async function uploadPaste(file: File): Promise<string> {
  const { data, ext } = await fitImage(file);
  await fileClient.mkdir({ path: PASTE_DIR, createParents: true });
  const base = `paste-${stamp(new Date())}`;
  for (let i = 0; i < 1000; i++) {
    const rel = joinPath(PASTE_DIR, i === 0 ? `${base}${ext}` : `${base}-${i}${ext}`);
    try {
      // overwrite:false is the race-free name check — two pastes in one
      // second get a counter instead of clobbering each other. The server
      // makes the file, so a lost race is an AlreadyExists, not silence.
      await fileClient.uploadFile({ path: rel, data, overwrite: false });
      void prunePastes();
      return rel;
    } catch (e) {
      if (isAlreadyExists(e)) continue;
      throw e;
    }
  }
  throw new Error("paste directory holds too many files from the same second");
}

/** Drop the oldest attachments beyond PASTE_KEEP. Best-effort: a failure
 *  here must never fail the paste that triggered it. */
async function prunePastes(): Promise<void> {
  try {
    const res = await fileClient.listDirectory({ path: PASTE_DIR });
    const files = res.entries
      .filter((e) => !e.isDir && e.name.startsWith("paste-"))
      .sort((a, b) => Number(a.mtimeUnixMs - b.mtimeUnixMs));
    for (const e of files.slice(0, Math.max(0, files.length - PASTE_KEEP))) {
      await fileClient.delete({ path: joinPath(PASTE_DIR, e.name) });
    }
  } catch {
    // directory vanished or a file is held open — next paste retries
  }
}