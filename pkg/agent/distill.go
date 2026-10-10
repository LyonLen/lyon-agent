package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"

	"lyon-agent/pkg/hooks"
	"lyon-agent/pkg/memory"
)

// Session self-evolution: distill durable facts from the conversation
// into the layered memory store using the cheap subagent tier. Two
// triggers: /remember (explicit, result shown) and exit (once, silent) —
// both gated by LYON_DISTILL=off. The call deliberately does NOT emit on
// the session's event channel: at exit the TUI pump may already be gone,
// and a blocked emit would hang the process. Audit goes straight through
// hooks.Fire (non-blocking by design); /remember reports via its return.
const (
	// distillMaxTokens bounds one extraction pass: facts are short.
	distillMaxTokens = 1500
	// distillHistoryCap bounds the rendered history handed to the
	// extractor (runes). The RECENT tail carries the new learnings.
	distillHistoryCap = 48000
	// distillMinHistory skips extraction for sessions with nothing to say.
	distillMinHistory = 400
	// distillMaxFacts caps writes per pass — a hurried model must not
	// flood the store.
	distillMaxFacts = 6
	// distillBodyCap clips one fact body.
	distillBodyCap = 4096
	// distillPromoteHits: a fact the model re-affirms after being read
	// this often graduates into the resident hot tier.
	distillPromoteHits = 2
)

// distillSystemPrompt instructs the extractor. JSON-only output keeps
// parsing trivial and the model honest.
const distillSystemPrompt = `You distill durable memories from a conversation between a user and an AI agent, rendered as role-tagged text. Reply with a JSON array only — no prose, no code fences — and [] when nothing is durable.

Each element: {"name":"kebab-case-id","description":"one-line summary","body":"the fact in 1-3 sentences","scope":"project|personal|team","hot":false}

Save only non-obvious facts worth keeping for FUTURE sessions: decisions and their reasons, user preferences, conventions, environment gotchas, durable commands. Never save transient task state, files being edited right now, or anything obvious from the repo itself.
- scope: "project" for this codebase; "personal" for the user's standing preferences (language, workflow, tooling); "team" for org-wide conventions shared across projects.
- hot: true ONLY for standing preferences or hard constraints the next session must know without asking — the resident tier stays tiny, so default false.
- At most 6 items. Shorter is better; [] is a perfectly good answer.`

// distillFact is one candidate fact as the extractor returns it.
type distillFact struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`
	Scope       string `json:"scope"`
	Hot         bool   `json:"hot"`
}

// Distill extracts durable facts from this session's history and saves
// them into the layered memory store. Returns how many facts were
// written. ctx bounds the model call; the history snapshot is taken
// under histMu (briefly blocks against an in-flight drive).
func (s *Session) Distill(ctx context.Context) (int, error) {
	if strings.EqualFold(os.Getenv("LYON_DISTILL"), "off") {
		return 0, nil
	}
	s.histMu.Lock()
	msgs := s.seed
	if s.runner != nil {
		msgs = s.runner.Messages()
	}
	s.histMu.Unlock()

	rendered := renderHistory(msgs)
	if r := []rune(rendered); len(r) < distillMinHistory {
		return 0, nil
	} else if len(r) > distillHistoryCap {
		rendered = "(earlier history elided)\n" + string(r[len(r)-distillHistoryCap:])
	}

	params := anthropic.BetaMessageNewParams{
		Model:     s.eng.cfg.SubagentModel,
		MaxTokens: distillMaxTokens,
		System:    []anthropic.BetaTextBlockParam{{Text: distillSystemPrompt}},
		Messages: []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(
			anthropic.NewBetaTextBlock("Conversation history (role-tagged):\n" + rendered))},
	}
	resp, err := s.eng.client.Beta.Messages.New(ctx, params)
	if err != nil {
		return 0, err
	}
	var parts []string
	for _, b := range resp.Content {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, b.Text)
		}
	}
	facts := parseDistillFacts(strings.Join(parts, "\n"))
	if len(facts) == 0 {
		return 0, nil
	}
	return s.applyDistill(memory.Open(s.eng.cfg.Workspace), facts), nil
}

// parseDistillFacts pulls the JSON array out of the model's reply,
// tolerating code fences and stray prose around it. Unparseable output
// yields no facts — distillation is best-effort, never an error the user
// must see at exit.
func parseDistillFacts(text string) []distillFact {
	s := strings.TrimSpace(text)
	s = strings.ReplaceAll(s, "```json", "")
	s = strings.ReplaceAll(s, "```", "")
	start := strings.IndexByte(s, '[')
	end := strings.LastIndexByte(s, ']')
	if start < 0 || end <= start {
		return nil
	}
	var facts []distillFact
	if err := json.Unmarshal([]byte(s[start:end+1]), &facts); err != nil {
		return nil
	}
	return facts
}

// applyDistill persists parsed facts and audits each write. Idempotent:
// re-distilling an unchanged conversation saves nothing (exit runs this
// every time). Facts that already exist and have proven useful
// (hits >= distillPromoteHits) graduate to the hot tier.
func (s *Session) applyDistill(st *memory.Store, facts []distillFact) int {
	saved := 0
	for _, f := range facts {
		if saved >= distillMaxFacts {
			break
		}
		body := strings.TrimSpace(f.Body)
		if body == "" {
			continue
		}
		name := memory.Slug(strings.TrimSpace(f.Name))
		if name == "" {
			name = memory.Slug(f.Description)
		}
		if name == "" {
			continue
		}
		scope := f.Scope
		if scope != memory.ScopeTeam && scope != memory.ScopePersonal && scope != memory.ScopeProject {
			scope = memory.ScopeProject
		}
		desc := strings.TrimSpace(f.Description)
		if r := []rune(desc); len(r) > 200 {
			desc = string(r[:200])
		}
		old, existed := st.Get(scope, name)
		if existed && old.Body == body && (desc == "" || old.Description == desc) {
			continue // nothing new — exit distills must be idempotent
		}
		if _, err := st.Save(memory.Fact{
			Name:        name,
			Description: desc,
			Scope:       scope,
			Body:        clipRunes(body, distillBodyCap),
		}); err != nil {
			continue
		}
		hot := f.Hot
		if !hot && existed && old.Hits >= distillPromoteHits {
			hot = true // proven useful: graduate to the resident tier
		}
		if hot {
			if cur, ok := st.Get(scope, name); ok && !cur.Hot {
				st.ToggleHot(scope, name)
			}
		}
		hotNote := ""
		if hot {
			hotNote = ", hot"
		}
		s.eng.hooks.Fire(hooks.Record{
			Event:  hooks.EvMemorySave,
			Tool:   name,
			Detail: fmt.Sprintf("%s scope%s", scope, hotNote),
		})
		saved++
	}
	return saved
}
