import { useEffect, useState } from "react";
import type { Components } from "react-markdown"; // type-only: erased, keeps the parser out of this chunk
import { fileClient } from "../api";

// mdkit (Phase F): the shared renderer pieces that do NOT pull in the
// react-markdown parser — FilePanel imports this statically while both
// markdown consumers load Markdown.tsx (the parser) behind React.lazy.

export type MdLinkOpts = {
  onLocalPath?: (path: string) => void;
  basePath?: string;
};
export function makeMdComponents(opts?: MdLinkOpts): Components {
  return {
    code({ className, children }) {
      const text = String(children ?? "");
      const match = /language-([\w-]+)/.exec(className || "");
      if (!match) return <code className="md-inline">{text}</code>;
      return <code className={`md-fence ${className}`}>{text}</code>;
    },
    a({ href, children }) {
      const h = href ?? "";
      if (/^(https?:|mailto:)/i.test(h) || h.startsWith("#")) {
        return (
          <a href={h} target="_blank" rel="noreferrer">
            {children}
          </a>
        );
      }
      if (opts?.onLocalPath) {
        return (
          <a
            href="#"
            onClick={(e) => {
              e.preventDefault();
              opts.onLocalPath?.(h);
            }}
          >
            {children}
          </a>
        );
      }
      return (
        <a href={h} target="_blank" rel="noreferrer">
          {children}
        </a>
      );
    },
    img: opts?.basePath ? (props) => <MdImage {...props} basePath={opts.basePath!} /> : undefined,
  };
}

export const mdComponents: Components = makeMdComponents();

// Resolve a markdown-relative href against the preview's directory,
// collapsing "." / ".." lexically — the server's Confine re-validates on
// fetch, so this is URL bookkeeping, not a security decision.
export function resolveRel(dir: string, href: string): string {
  if (href.startsWith("/")) return href.slice(1);
  const segs = (dir ? dir.split("/") : []).concat(href.split("/"));
  const out: string[] = [];
  for (const s of segs) {
    if (s === "" || s === ".") continue;
    if (s === "..") out.pop();
    else out.push(s);
  }
  return out.join("/");
}

// MdImage: relative srcs go through ReadBinaryFile → object URL so md
// previews can show workspace images; failures degrade to alt text
// (portal MdImage behavior), never a broken-image glyph.
function MdImage({
  src,
  alt,
  basePath,
}: {
  src?: string | Blob;
  alt?: string;
  basePath: string;
}) {
  const raw = typeof src === "string" ? src : "";
  const [url, setUrl] = useState<string | null>(null);
  const [state, setState] = useState<"idle" | "load" | "fail">("idle");
  useEffect(() => {
    let live = true;
    let created = "";
    if (!raw || /^(https?:|data:)/i.test(raw)) {
      setUrl(raw || null);
      setState("idle");
      return () => {
        live = false;
      };
    }
    setState("load");
    void (async () => {
      try {
        const res = await fileClient.readBinaryFile({ path: resolveRel(basePath, raw) });
        if (!live) return; // unmounted mid-fetch: don't mint an orphan URL
        const blob = new Blob([new Uint8Array(res.data)], { type: res.contentType });
        created = URL.createObjectURL(blob);
        setUrl(created);
        setState("idle");
      } catch {
        if (live) setState("fail");
      }
    })();
    return () => {
      live = false;
      if (created) URL.revokeObjectURL(created);
    };
  }, [raw, basePath]);
  if (state === "fail") return <>[图片加载失败: {alt ?? raw}]</>;
  if (state === "load") return <>[加载中: {alt ?? raw}]</>;
  if (!url) return <img src={raw} alt={alt} />;
  return <img src={url} alt={alt} />;
}
