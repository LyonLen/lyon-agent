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
	return s.AskModel(ctx, "", system, user, maxTokens)
}

// AskModel is Ask with a model override — the competition's grading
// channel (#53): each candidate must answer the held-out exam on ITS OWN
// model, or the result grades the wrong entrant. "" = the subagent tier.
// Same statelessness as Ask: concurrent per-candidate calls are safe.
func (s *Session) AskModel(ctx context.Context, model, system, user string, maxTokens int64) (string, error) {
	if model == "" {
		model = s.eng.subagentModel()
	}
	resp, err := s.eng.client.Beta.Messages.New(ctx, anthropic.BetaMessageNewParams{
		Model:     model,
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
