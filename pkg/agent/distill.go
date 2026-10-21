package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"lyon-agent/pkg/hooks"
	"lyon-agent/pkg/memory"
)

// Session self-evolution: distill durable facts from the conversation
// into the layered memory store using the cheap subagent tier. Two
// triggers: /remember (explicit, result shown) and exit — both gated by
// LYON_DISTILL=off. The call deliberately does NOT emit on the session's
// event channel: at exit the TUI pump may already be gone, and a blocked
// emit would hang the process. Audit goes straight through hooks.Fire
// (non-blocking by design); the user-visible channel is the one-line
// echo: stdout for the exit and plain-REPL paths (Distill), the command
// panel for the TUI (DistillWithReport → pushCmd). See DistillReport.
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
	// distillLockWait caps how long Distill waits for the history mutex.
	// The exit path runs right after an Interrupt: if the drive is still
	// settling, wait a moment then skip — a memory pass must never hold
	// process exit hostage (that felt like "ctrl+c hangs").
	distillLockWait = 3 * time.Second
)

// lockHistForDistill acquires histMu within timeout (polling TryLock so
// no abandoned goroutine can leak a held mutex). Returns false on timeout.
func (s *Session) lockHistForDistill(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for !s.histMu.TryLock() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return true
}

// distillKey fingerprints the rendered history so an unchanged
// conversation skips the extractor call entirely.
func distillKey(rendered string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(rendered))
	return h.Sum64()
}

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

// DistillReport summarizes one distill pass for the visible echo: how
// many facts were written, their names, and which of them entered the
// resident hot tier this pass. A fact that was already hot is not
// re-promoted — Promoted only records tiers that actually changed.
type DistillReport struct {
	Saved    int      // facts written this pass
	Names    []string // their names, in save order
	Promoted []string // names that entered the hot tier this pass
}

// EchoLine renders the one-line English summary, e.g.
// "memory: +2 facts (a, b) · 1 promoted to hot: a". Empty when nothing
// was saved — callers keep their own "nothing durable" wording.
func (r DistillReport) EchoLine() string { return r.echoLine(false) }

// EchoLineZH is the Chinese variant: the TUI picks it through tr(), the
// exit echo through LYON_LANG (distillEchoLine).
func (r DistillReport) EchoLineZH() string { return r.echoLine(true) }

func (r DistillReport) echoLine(zh bool) string {
	if r.Saved <= 0 {
		return ""
	}
	names := strings.Join(r.Names, ", ")
	if zh {
		line := fmt.Sprintf("memory: +%d 条事实（%s）", r.Saved, names)
		if len(r.Promoted) > 0 {
			line += fmt.Sprintf(" · %d 条晋升热层：%s", len(r.Promoted), strings.Join(r.Promoted, ", "))
		}
		return line
	}
	line := fmt.Sprintf("memory: +%d facts (%s)", r.Saved, names)
	if len(r.Promoted) > 0 {
		line += fmt.Sprintf(" · %d promoted to hot: %s", len(r.Promoted), strings.Join(r.Promoted, ", "))
	}
	return line
}

// distillEchoLine picks the exit echo's language variant. The stdout
// echo never passes through cmd/lyon's tr(), so it reads LYON_LANG the
// way lang.go's initLang does at startup — a runtime /lang switch does
// not reach pkg/agent, and the exit line follows the startup language.
func distillEchoLine(r DistillReport) string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LYON_LANG"))) {
	case "zh", "cn", "zh-cn", "zh_cn", "zh-hans", "chinese":
		return r.EchoLineZH()
	default:
		return r.EchoLine()
	}
}

// Distill extracts durable facts from this session's history and saves
// them into the layered memory store. Returns how many facts were
// written. Both in-tree callers discard the report (the exit defer and
// the plain REPL's /remember), so Distill prints the one-line summary
// itself: at exit the TUI is gone and stdout is the only visible
// channel that still reaches the user. The TUI's /remember must NOT use
// this variant — raw stdout mid-alt-screen garbles the view; it calls
// DistillWithReport and echoes through its own command panel.
// ctx bounds the model call; the history snapshot is taken under
// histMu — but only briefly: exit runs this while a prompt may
// still be settling, and an unbounded lock wait would hang the process
// (see lockHistForDistill).
func (s *Session) Distill(ctx context.Context) (int, error) {
	rep, err := s.DistillWithReport(ctx)
	if err == nil && rep.Saved > 0 {
		fmt.Println(distillEchoLine(rep))
	}
	return rep.Saved, err
}

// DistillWithReport is the structured twin of Distill: same pass, same
// gates, but the caller receives fact names and hot-tier promotions for
// its own echo and stdout stays clean (UI layers route the report
// through their channels instead of a raw write).
func (s *Session) DistillWithReport(ctx context.Context) (DistillReport, error) {
	if strings.EqualFold(os.Getenv("LYON_DISTILL"), "off") {
		return DistillReport{}, nil
	}
	if !s.lockHistForDistill(distillLockWait) {
		return DistillReport{}, nil // drive still busy — skip this round, never hang exit
	}
	msgs := s.seed
	if s.runner != nil {
		msgs = s.runner.Messages()
	}
	s.histMu.Unlock()

	rendered := renderHistory(msgs)
	if r := []rune(rendered); len(r) < distillMinHistory {
		return DistillReport{}, nil
	} else if len(r) > distillHistoryCap {
		rendered = "(earlier history elided)\n" + string(r[len(r)-distillHistoryCap:])
	}

	// Same history as the last successful pass → nothing to extract.
	// /remember followed by quit (the common exit path) becomes free.
	key := distillKey(rendered)
	s.mu.Lock()
	prev := s.distillHash
	s.mu.Unlock()
	if key == prev {
		return DistillReport{}, nil
	}

	params := anthropic.BetaMessageNewParams{
		Model:     s.eng.subagentModel(),
		MaxTokens: distillMaxTokens,
		System:    []anthropic.BetaTextBlockParam{{Text: distillSystemPrompt}},
		Messages: []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(
			anthropic.NewBetaTextBlock("Conversation history (role-tagged):\n" + rendered))},
	}
	resp, err := s.eng.client.Beta.Messages.New(ctx, params)
	if err != nil {
		return DistillReport{}, err
	}
	s.mu.Lock()
	s.distillHash = key // extraction ran clean — next identical pass is free
	s.mu.Unlock()
	var parts []string
	for _, b := range resp.Content {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, b.Text)
		}
	}
	facts := parseDistillFacts(strings.Join(parts, "\n"))
	if len(facts) == 0 {
		return DistillReport{}, nil
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

// applyDistill persists parsed facts and audits each write, returning
// the report the visible echo renders (names + hot-tier promotions).
// Idempotent: re-distilling an unchanged conversation saves nothing
// (exit runs this every time). Facts that already exist and have proven
// useful (hits >= distillPromoteHits) graduate to the hot tier.
func (s *Session) applyDistill(st *memory.Store, facts []distillFact) DistillReport {
	var rep DistillReport
	for _, f := range facts {
		if rep.Saved >= distillMaxFacts {
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
		promoted := false
		if hot {
			// Save carries an existing fact's hot flag through, so the
			// toggle (not the hot flag itself) marks a tier that just
			// changed — an already-hot fact is not re-promoted.
			if cur, ok := st.Get(scope, name); ok && !cur.Hot {
				if _, err := st.ToggleHot(scope, name); err == nil {
					promoted = true
				}
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
		rep.Saved++
		rep.Names = append(rep.Names, name)
		if promoted {
			rep.Promoted = append(rep.Promoted, name)
		}
	}
	return rep
}
