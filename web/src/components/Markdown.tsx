import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import type { Components } from "react-markdown";
import { mdComponents } from "./mdkit";

export { makeMdComponents, mdComponents, resolveRel } from "./mdkit";

// Markdown module (Phase F): react-markdown + remark-gfm live behind a
// dynamic import (loaded via React.lazy at both call sites) so the chat
// pane first paint doesn't carry the parser (portal/CodeEditor precedent:
// plugins hoisted, no syntax highlighter).

// MarkdownView is the lazy boundary: callers wrap it in Suspense with a
// plain-text fallback, so the parser chunk arrives without blanking.
export default function MarkdownView({
  children,
  components,
}: {
  children: string;
  components?: Components;
}) {
  return (
    <ReactMarkdown remarkPlugins={[remarkGfm]} components={components ?? mdComponents}>
      {children}
    </ReactMarkdown>
  );
}
