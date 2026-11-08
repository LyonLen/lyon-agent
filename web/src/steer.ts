// steer.ts — mid-run input classification, the TS mirror of
// pkg/agent/steer.go (keep the patterns in sync with ClassifySteer there).
//
// A line typed while the seat is busy is not just "queued": its intent
// decides the mechanism — stop fires immediately, redirect stops the run
// and runs next, supplement parks until the run ends, everything else
// folds into the running task at the next turn gap.

export type SteerIntent = "fold" | "supplement" | "redirect" | "stop";

// Whole-prompt stop imperative (particles only as a tail) — "停止条件是什么"
// or "帮我停止容器" must never halt a run.
const STOP_RE =
  /^(停|停下|停止|停一下|先停|给我停|可以停|现在停|立刻停|马上停|别弄了|别做了|别改了|别写了|别搞了|算了|不用了|取消|住手|打住|够了|stop|halt|abort|cancel|break)[\s吧啊了呀哦!.！~～。]*$/;
const STOP_MAX_RUNES = 12;

// Explicit task switch — conservative: a false redirect kills half-done work.
const REDIRECT_RE =
  /换个?(方向|任务|思路|话题|东西|事情)|换方向|换任务|改做|改去|转去做|先(做|弄|搞)这个|去做(别的|其他|新的)|做(别的|其他)(的)?(事|任务|东西)|新(的)?任务|切换到|重(新)?(做|来|开始)(个|一个)?(新)?(任务|东西|事)|别(弄|做|改|写|搞)(这个|那个)?了?\s*[，,、;；]?\s*(去|来|换|改|做|搞)/;

// Additive input: record it, finish the current task, then handle it.
// Continuations required where a bare word is also a common question.
const SUPPLEMENT_RE =
  /补充(一)?(个|条|点|下)?|另外(还|再|帮|做|加)|对了|别忘了|记得(要|把|加|更新|改|写|删|交|提|发|做|弄)|顺(便|手)(帮|再|做|加|改|更新)?|等(下|会儿?|一下)(再|还|帮)(帮|做|弄)?|待会(再|帮|做)?|回头(再|还|帮|做|看)|之后(再|还|帮|做|记得|更新|改|加)|再(帮我|加(一)?(条|个|点)|弄一?个|做一?个)|附带|还得(帮我|加|弄|做|改|写|更新)|还要(帮我|加|弄|做|改|写|更新)/;

export function classifySteer(prompt: string): SteerIntent {
  const p = prompt.trim();
  if (!p) return "fold";
  if (STOP_RE.test(p) && [...p].length <= STOP_MAX_RUNES) return "stop";
  if (REDIRECT_RE.test(p)) return "redirect";
  if (SUPPLEMENT_RE.test(p)) return "supplement";
  return "fold";
}
