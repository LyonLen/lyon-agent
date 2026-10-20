import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import type { Components, Options } from "react-markdown";
import { mdComponents } from "./mdkit";

export { makeMdComponents, mdComponents, resolveRel } from "./mdkit";

// Markdown module (Phase F): react-markdown + remark-gfm live behind a
// dynamic import (loaded via React.lazy at both call sites) so the chat
// pane first paint doesn't carry the parser (portal/CodeEditor precedent:
// plugins hoisted, no syntax highlighter).

// MarkdownView is the lazy boundary: callers wrap it in Suspense with a
// plain-text fallback, so the parser chunk arrives without blanking.
// rehypePlugins is optional and stays with the caller (the editor passes
// rehype-slug for GitHub-style heading anchors; chat doesn't need it).
export default function MarkdownView({
  children,
  components,
  rehypePlugins,
}: {
  children: string;
  components?: Components;
  rehypePlugins?: Options["rehypePlugins"];
}) {
  return (
    <ReactMarkdown
      remarkPlugins={[remarkGfm]}
      rehypePlugins={rehypePlugins}
      components={components ?? mdComponents}
    >
      {children}
    </ReactMarkdown>
  );
}
