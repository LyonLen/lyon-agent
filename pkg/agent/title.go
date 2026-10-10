package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
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

// Title returns the current session title ("" until first recognized).
func (s *Session) Title() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.title
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

	resp, err := s.eng.client.Beta.Messages.New(ctx, anthropic.BetaMessageNewParams{
		Model:     s.eng.cfg.SubagentModel,
		MaxTokens: titleMaxTokens,
		System:    []anthropic.BetaTextBlockParam{{Text: titleSystemPrompt}},
		Messages:  []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(user))},
	})
	if err != nil {
		return cur, false, err
	}
	var parts []string
	for _, b := range resp.Content {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, b.Text)
		}
	}
	changed, title := parseTitleReply(strings.Join(parts, "\n"))
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
