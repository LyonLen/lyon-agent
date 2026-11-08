package agent

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// SteerIntent classifies a user input that arrives while a prompt is still
// running — the mid-run steering protocol. The three urgent/deferred kinds
// get a mechanical action at the submit seam (stop → Interrupt, redirect →
// Interrupt + run next, supplement → park until the run ends); everything
// else folds into the running task at the next turn gap and the
// DefaultSystemPrompt mid-run protocol tells the model how to fold it.
type SteerIntent int

const (
	// SteerFold injects the line at the next turn gap: the running task
	// folds it in (clarification, correction, nudge, chit-chat, or an
	// ordinary new question — the model decides how to weave it in
	// without dropping the task).
	SteerFold SteerIntent = iota
	// SteerSupplement parks the line until the current run finishes —
	// "先收录，把当前会话处理完然后再处理新任务". Not gap-injected: the
	// working task must not see it mid-flight.
	SteerSupplement
	// SteerRedirect stops the current run and runs the line next —
	// "停下后按照新问题处理". The previous task stays half-done: that is
	// what an explicit switch means.
	SteerRedirect
	// SteerStop halts the current run immediately ("立马停下"). The line
	// is the stop command itself and never runs as a prompt.
	SteerStop
)

// String names the intent for status lines and tests.
func (i SteerIntent) String() string {
	switch i {
	case SteerSupplement:
		return "supplement"
	case SteerRedirect:
		return "redirect"
	case SteerStop:
		return "stop"
	default:
		return "fold"
	}
}

// stopRe is a whole-prompt stop command: an imperative stop head plus only
// particles/punctuation as a tail. Anchored and tail-limited on purpose —
// "停止条件是什么" or "帮我停止容器" must never halt a run.
var stopRe = regexp.MustCompile(
	`^(停|停下|停止|停一下|先停|给我停|可以停|现在停|立刻停|马上停|别弄了|别做了|别改了|别写了|别搞了|算了|不用了|取消|住手|打住|够了|stop|halt|abort|cancel|break)[\s吧啊了呀哦!.！~～。]*$`)

// stopMaxRunes bounds the whole prompt for a stop match: a stop order is a
// few words, never a sentence with content after it.
const stopMaxRunes = 12

// redirectRe names an explicit task switch. Conservative on purpose: a
// false redirect kills half-done work, so plain new questions (no marker)
// must fall through to fold/supplement instead.
var redirectRe = regexp.MustCompile(
	`换个?(方向|任务|思路|话题|东西|事情)|换方向|换任务|改做|改去|转去做|先(做|弄|搞)这个|去做(别的|其他|新的)|做(别的|其他)(的)?(事|任务|东西)|新(的)?任务|切换到|重(新)?(做|来|开始)(个|一个)?(新)?(任务|东西|事)|别(弄|做|改|写|搞)(这个|那个)?了?\s*[，,、;；]?\s*(去|来|换|改|做|搞)`)

// supplementRe names additive input: "record this, finish what you are
// doing, then handle it". Explicit markers only — the plain-new-task case
// folds and the model-side protocol defers it. Continuations are required
// where a bare word is also a common question ("还要多久？" must fold).
var supplementRe = regexp.MustCompile(
	`补充(一)?(个|条|点|下)?|另外(还|再|帮|做|加)|对了|别忘了|记得(要|把|加|更新|改|写|删|交|提|发|做|弄)|顺(便|手)(帮|再|做|加|改|更新)?|等(下|会儿?|一下)(再|还|帮)(帮|做|弄)?|待会(再|帮|做)?|回头(再|还|帮|做|看)|之后(再|还|帮|做|记得|更新|改|加)|再(帮我|加(一)?(条|个|点)|弄一?个|做一?个)|附带|还得(帮我|加|弄|做|改|写|更新)|还要(帮我|加|弄|做|改|写|更新)`)

// ClassifySteer decides what a mid-run input means. Order matters: an
// explicit stop wins first (it is an emergency brake), then a redirect,
// then a supplement; anything else folds into the running task.
func ClassifySteer(prompt string) SteerIntent {
	p := strings.TrimSpace(prompt)
	if p == "" {
		return SteerFold
	}
	if stopRe.MatchString(p) && utf8.RuneCountInString(p) <= stopMaxRunes {
		return SteerStop
	}
	if redirectRe.MatchString(p) {
		return SteerRedirect
	}
	if supplementRe.MatchString(p) {
		return SteerSupplement
	}
	return SteerFold
}
