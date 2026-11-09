// §7.3 selection-ask wire format — ONE source of truth shared by the
// mindmap pane's float bar (MindMapCard.askSel) and the main composer
// (ChatView.send). Image #64: a selection question typed in the main
// composer went out unmarked, so the model saw no selection at all —
// both paths must build the same wire string, and display must strip
// exactly what they built (Image #65: the discipline suffix is wire,
// not feed text).

/** System discipline suffix — the rules for how a selection ask may
 *  land on the map (§7.3: one map_ops fence or plain prose, never
 *  prose-shaped edit instructions). Wire-only: never rendered. */
export const SEL_DISCIPLINE =
  "【系统 · 选区提问纪律】你在回答一个「基于导图选区」的提问，选区节点 id 在上一行方括号里。规则：\n" +
  "1. 需要改图时：只输出一个 ```map_ops 代码块，内容是 JSON 数组（每项带 op 字段，可用 rewrite/set_parent/add/park/delete/confirm/merge），代码块之外不写任何改动指令。\n" +
  "2. 不需要改图时：直接散文回答，不要输出 map_ops 代码块。\n" +
  "3. 改动落在选区上：rewrite/confirm/park/delete/merge/set_parent 只针对方括号里的选区 id；add 的新节点挂在选区节点下、提案态。\n" +
  "4. 选区外的既有节点一律不动；已确认/已固定节点不要改（服务器会整批拒绝）。\n" +
  "5. 节点只承载主题内容：流程旁注、机制/上限发现、操作记录（回滚、墓碑、rev、parent_id）、确认/停靠状态日志、归档路径、日期播报都不入图——要说明就写在散文回复里。";

/** 期望落图 flag — rides only verbs that must mutate (改写): ChatView's
 *  reply parser uses it to decide whether a prose-only answer earns the
 *  A4 notice; 解释/矛盾吗 answering in prose is correct and stays quiet. */
export const SEL_EXPECT_MAP = "\n【期望落图】本次请求必须以 map_ops 围栏落图。";

/** The full wire prompt: `[导图选区 N 个节点|id1,id2] <question>` +
 *  discipline (+ optional expect-map flag). The ids ride the text
 *  itself — no RPC carries them (the marker IS the wire format). */
export function buildSelAsk(ids: string[], question: string, expectMap = false): string {
  const q = question.trim() || "这些节点讲了什么？彼此是什么关系？";
  return `[导图选区 ${ids.length} 个节点|${ids.join(",")}] ${q}\n${SEL_DISCIPLINE}${
    expectMap ? SEL_EXPECT_MAP : ""
  }`;
}

/** Matches the leading marker (used to arm the reply collector and to
 *  detect an already-marked line so ↑ recall is never wrapped twice). */
export const SEL_MARKER_RE = /^\[导图选区 (\d+) 个节点\|([^\]]+)\]\s*/;

/** Display-side strip of everything wire-only: the 【系统 ·…】 suffix
 *  runs to the end of the message (discipline + optional 期望落图), the
 *  marker leads it. Ids survive for chip rendering; body is the human
 *  text the feed should show. */
export function stripWireForDisplay(text: string): {
  marked: boolean;
  ids: string[];
  body: string;
} {
  const m = SEL_MARKER_RE.exec(text);
  // Unmarked text is a plain human line — never touch it (a message that
  // merely contains 【系统 ·…】 must survive verbatim).
  if (!m) return { marked: false, ids: [], body: text };
  // Marked: the discipline (and 期望落图) suffix runs to the end.
  const body = text.replace(/\n【系统 ·[\s\S]*$/, "").slice(m[0].length);
  return { marked: true, ids: m[2].split(",").filter(Boolean), body };
}
