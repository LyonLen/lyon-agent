package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Session title: a terse intent label shown in the header and persisted
// with snapshots, refreshed when the user pivots to a different topic.
// Cost discipline is the design: ONE small-model call per prompt that
// sees only the first prompt + current title + latest prompt (hundreds
// of input tokens — never the history), a 64-token reply, and a
// changed:false answer that skips the write entirely. LYON_TITLE=off
// disables. Prompt text is recorded at Submit, so this path never
// contends with the drive goroutine's histMu.

const (
	// titleMaxTokens bounds the JSON reply: changed flag + ≤8 words.
	titleMaxTokens = 64
	// titlePromptCap clips each embedded prompt (runes).
	titlePromptCap = 500
	// titleCap clips the accepted title itself.
	titleCap = 80
	// titleRecentN is how many prompts the interval summarizer sees.
	titleRecentN = 5
)

// titleSystemPrompt is the intent-recognition instruction. The
// changed:false answer is what keeps the title stable (and the token
// bill low) when the user is just continuing.
const titleSystemPrompt = `You maintain a terse session title for a chat header (intent recognition). You receive the session's first user message, the current title (may be empty), and the latest user message.

Reply with ONE JSON object only, no prose, no code fences:
- No current title yet: {"changed":true,"title":"..."}
- The latest message continues the same task: {"changed":false}
- The latest message starts a clearly different topic or task: {"changed":true,"title":"new title"}

The title is 3-8 words in the language the user writes, naming the TASK (e.g. "修复登录页布局错位"), never a transcript of the chat.`

// Title returns the current session title: the intent label once the title
// pass has written one, else the placeholder derived from the first prompt
// (#44) — a fresh seat shows its question instead of "(untitled)" while
// round one is still running, and the pass overwrites it when the summary
// lands. "" only before the session has any prompt.
func (s *Session) Title() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.title != "" {
		return s.title
	}
	return s.titleProv
}

// placeholderTitle derives the stand-in title from a prompt: its first
// non-empty line, whitespace collapsed, capped at titleCap runes. The raw
// question is deliberately used — no model call, so it is available the
// instant SubmitOn records the prompt.
func placeholderTitle(prompt string) string {
	for _, line := range strings.Split(prompt, "\n") {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			continue
		}
		if r := []rune(line); len(r) > titleCap {
			return strings.TrimRight(string(r[:titleCap-1]), " ") + "…"
		}
		return line
	}
	return ""
}

// RefreshTitle runs one cheap intent check against the latest prompt and
// updates the title when the topic changed (or when none exists yet).
// Returns the effective title, whether it changed, and any call error —
// failures leave the previous title untouched.
func (s *Session) RefreshTitle(ctx context.Context) (string, bool, error) {
	if strings.EqualFold(os.Getenv("LYON_TITLE"), "off") {
		return s.Title(), false, nil
	}
	s.mu.Lock()
	first, last, cur := s.firstPrompt, s.lastPrompt, s.title
	s.mu.Unlock()
	if last == "" {
		return cur, false, nil // no prompt yet — nothing to recognize
	}
	if first == "" {
		first = last
	}
	current := strings.TrimSpace(cur)
	if current == "" {
		current = "(none)"
	}
	user := fmt.Sprintf("First user message:\n%s\n\nCurrent title: %s\n\nLatest user message:\n%s",
		clipRunes(first, titlePromptCap), current, clipRunes(last, titlePromptCap))

	reply, err := s.Ask(ctx, titleSystemPrompt, user, titleMaxTokens)
	if err != nil {
		return cur, false, err
	}
	changed, title := parseTitleReply(reply)
	if !changed {
		return cur, false, nil
	}
	title = strings.Join(strings.Fields(title), " ")
	if title == "" {
		return cur, false, nil // changed with no title: keep the old one
	}
	if r := []rune(title); len(r) > titleCap {
		title = string(r[:titleCap])
	}
	s.mu.Lock()
	s.title = title
	s.mu.Unlock()
	// A fresh intent label belongs on disk with the conversation, so a
	// restarted lyond lists the titled session, not "(untitled)".
	s.notifyDirty()
	return title, true, nil
}

// titleSummarizeSystemPrompt is the interval pass: instead of the
// per-prompt intent check (first + latest), the model gets the recent
// prompt tail and names the session's current task.
const titleSummarizeSystemPrompt = `You maintain a terse session title for a chat header (session summary). You receive the session's current title (may be empty) and its most recent user messages (up to 5).

Reply with ONE JSON object only, no prose, no code fences:
- A title is needed, or the current one no longer fits: {"changed":true,"title":"..."}
- The current title still fits the session: {"changed":false}

The title is 3-8 words in the language the user writes, naming the TASK the session is working on (e.g. "修复登录页布局错位"), never a transcript of the chat.`

// appendPromptTail keeps the last titleRecentN non-empty prompts —
// called with s.mu held.
func appendPromptTail(tail []string, prompts ...string) []string {
	for _, p := range prompts {
		if p = strings.TrimSpace(p); p != "" {
			tail = append(tail, p)
		}
	}
	if len(tail) > titleRecentN {
		tail = tail[len(tail)-titleRecentN:]
	}
	return tail
}

// intervalTitleInput snapshots what the summarizer sees: the current
// title (trimmed, "" until first recognized) and the recent-prompt tail.
// A restored session predates the tail, so it falls back to the first
// prompt; recent is nil only when the session has no prompt at all.
func (s *Session) intervalTitleInput() (current string, recent []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current = strings.TrimSpace(s.title)
	recent = append([]string(nil), s.recentPrompts...)
	if len(recent) == 0 && strings.TrimSpace(s.firstPrompt) != "" {
		recent = []string{s.firstPrompt}
	}
	return current, recent
}

// IntervalTitle regenerates the title from the last ≤titleRecentN
// prompts plus the current title (shown to the model as "(none)" when
// absent) — the beat-based title pass (prompt counts 1, 6, 26, 46 …
// behind the server's hub tap). Same cost discipline and reply contract
// as RefreshTitle: one small-model call, 64-token reply, a bad reply or
// changed:false never clobbers the existing title. LYON_TITLE=off
// disables.
func (s *Session) IntervalTitle(ctx context.Context) (string, bool, error) {
	if strings.EqualFold(os.Getenv("LYON_TITLE"), "off") {
		return s.Title(), false, nil
	}
	before := s.Title()
	current, recent := s.intervalTitleInput()
	if len(recent) == 0 {
		return before, false, nil // no prompt yet — nothing to summarize
	}
	if current == "" {
		current = "(none)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Current title: %s\n\nRecent user messages:", current)
	for i, p := range recent {
		fmt.Fprintf(&b, "\n%d. %s", i+1, clipRunes(p, titlePromptCap))
	}

	reply, err := s.Ask(ctx, titleSummarizeSystemPrompt, b.String(), titleMaxTokens)
	if err != nil {
		return before, false, err
	}
	changed, title := parseTitleReply(reply)
	if !changed {
		return before, false, nil
	}
	title = strings.Join(strings.Fields(title), " ")
	if title == "" {
		return before, false, nil // changed with no title: keep the old one
	}
	if r := []rune(title); len(r) > titleCap {
		title = string(r[:titleCap])
	}
	s.mu.Lock()
	s.title = title
	s.mu.Unlock()
	// A fresh intent label belongs on disk with the conversation, so a
	// restarted lyond lists the titled session, not "(untitled)".
	s.notifyDirty()
	return title, true, nil
}

// parseTitleReply extracts {changed,title} from the model's reply,
// tolerating code fences and stray prose. Malformed output means "no
// change" — a bad reply never clobbers a good title.
func parseTitleReply(text string) (changed bool, title string) {
	s := strings.TrimSpace(text)
	s = strings.ReplaceAll(s, "```json", "")
	s = strings.ReplaceAll(s, "```", "")
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end <= start {
		return false, ""
	}
	var r struct {
		Changed *bool  `json:"changed"`
		Title   string `json:"title"`
	}
	if err := json.Unmarshal([]byte(s[start:end+1]), &r); err != nil {
		return false, ""
	}
	if r.Changed == nil || !*r.Changed {
		return false, ""
	}
	return true, strings.TrimSpace(r.Title)
}
