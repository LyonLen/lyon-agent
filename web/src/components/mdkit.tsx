import { useEffect, useRef, useState } from "react";
import type { ReactNode } from "react";
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
    // Every fenced/indented block gets the corner copy button (the
    // transcript's code boxes, the preview's, the editor's — one seam).
    pre({ children, className }) {
      return <MdPreBox className={className}>{children}</MdPreBox>;
    },
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
    // img ALWAYS maps to MdImage: "" basePath (root-level files, the
    // dirnameOf an unnested path) is a valid resolveRel input, and a
    // components:{img: undefined} entry crashes react-markdown's render
    // ("Element type is invalid" on the badge-image link of any root README).
    img: (props) => <MdImage {...props} basePath={opts?.basePath ?? ""} />,
  };
}

export const mdComponents: Components = makeMdComponents();

// legacyCopy — the textarea + execCommand belt for the clipboard API's
// failure modes (non-secure origin, denied permission). Returns whether
// the copy actually happened; the node is always removed.
function legacyCopy(text: string): boolean {
  const ta = document.createElement("textarea");
  ta.value = text;
  ta.style.position = "fixed";
  ta.style.opacity = "0";
  document.body.appendChild(ta);
  ta.select();
  let ok = false;
  try {
    ok = document.execCommand("copy");
  } catch {
    ok = false;
  } finally {
    ta.remove();
  }
  return ok;
}

// MdPreBox — one fenced block plus its copy button. The button is a
// SIBLING of the <pre>, so its label can never leak into the payload;
// the copy reads textContent off the box itself, i.e. exactly the text
// the reader sees (entities decoded, the button excluded).
function MdPreBox({
  children,
  className,
}: {
  children?: ReactNode;
  className?: string;
}) {
  const ref = useRef<HTMLPreElement>(null);
  const [state, setState] = useState<"idle" | "ok" | "fail">("idle");
  const copy = async () => {
    const text = ref.current?.textContent ?? "";
    let ok = false;
    try {
      if (navigator.clipboard?.writeText) {
        await navigator.clipboard.writeText(text);
        ok = true;
      } else {
        ok = legacyCopy(text);
      }
    } catch {
      ok = legacyCopy(text);
    }
    setState(ok ? "ok" : "fail");
    window.setTimeout(() => setState("idle"), 1500);
  };
  return (
    <div className="md-pre">
      <pre ref={ref} className={className}>
        {children}
      </pre>
      <button
        type="button"
        className={"md-copy" + (state === "idle" ? "" : " " + state)}
        title="copy"
        onClick={() => void copy()}
      >
        {state === "ok" ? "copied ✓" : state === "fail" ? "copy failed" : "copy"}
      </button>
    </div>
  );
}

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
// (the sibling product's image behavior), never a broken-image glyph.
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
