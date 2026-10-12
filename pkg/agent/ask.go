package agent

import (
	"context"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// Ask runs one standalone completion on the subagent tier — the sidecar
// channel for cheap single-shot calls: title intent checks, failure
// post-mortems, coverage quizzes. It never touches session history or the
// tool loop, and it never touches s.mu — safe to call from a goroutine
// beside a live drive. The reply is the model's text (joined blocks),
// verbatim; callers do their own parsing.
func (s *Session) Ask(ctx context.Context, system, user string, maxTokens int64) (string, error) {
	resp, err := s.eng.client.Beta.Messages.New(ctx, anthropic.BetaMessageNewParams{
		Model:     s.eng.cfg.SubagentModel,
		MaxTokens: maxTokens,
		System:    []anthropic.BetaTextBlockParam{{Text: system}},
		Messages:  []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(user))},
	})
	if err != nil {
		return "", err
	}
	var parts []string
	for _, b := range resp.Content {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n"), nil
}
