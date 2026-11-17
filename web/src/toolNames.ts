// Shared display names for tool cards — one label per tool everywhere
// (chat cards, approval card, Steps pane). Two rules:
//   - mapped tools get their styled label (🌐 Search / 🌐 Fetch);
//   - anything else is camel-cased: web_fetch is mapped, but run_shell →
//     RunShell, read_file → ReadFile — 首字母大写 + 驼峰, never ALL CAPS.
// The display layer owns naming; the data (blocks, events) keeps the raw
// tool name.
const TOOL_DISPLAY: Record<string, string> = {
  web_search: "🌐 Search",
  web_fetch: "🌐 Fetch",
};

function camelLabel(name: string): string {
  return name
    .split(/[_\s]+/)
    .filter(Boolean)
    .map((w) => w.charAt(0).toUpperCase() + w.slice(1))
    .join("");
}

export function toolDisplayName(name: string): string {
  return TOOL_DISPLAY[name] ?? camelLabel(name);
}
