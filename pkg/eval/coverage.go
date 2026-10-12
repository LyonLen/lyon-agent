package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// CoverageThreshold is the pre-work gate (design §2.2): below this the
// run must first inject the missing knowledge before it starts.
const CoverageThreshold = 0.6

// checklistGenCap bounds the task text sent to the checklist generator.
const checklistGenCap = 6000

// Item is one knowledge probe: a question a prepared agent should answer
// from memory before touching the task, plus its ground truth.
type Item struct {
	Q        string `json:"q"`
	Expected string `json:"expected"`
}

// ChecklistGen turns a task into a knowledge checklist, usually by
// reading the project memory index — the question bank grows with the
// memory (design §2.1). It returns JSON: [{"q":…,"expected":…}].
type ChecklistGen func(ctx context.Context, task string) (string, error)

// Book answers every checklist question in ONE closed-book call,
// returning JSON {"<question>": "<answer>"} (design §2.1 stage two —
// "what did your head pass over before you started").
type Book func(ctx context.Context, questions []string) (string, error)

// Coverage is the grading of one closed-book sweep.
type Coverage struct {
	Items  int     `json:"items"`
	Hits   int     `json:"hits"`
	Score  float64 `json:"score"`
	Misses []Item  `json:"misses,omitempty"`
}

// Pass compares against an explicit threshold.
func (c Coverage) Pass(th float64) bool { return c.Score >= th }

// GatePass compares against the default gate.
func (c Coverage) GatePass() bool { return c.Pass(CoverageThreshold) }

// GenSystem is the checklist generator prompt: derive checkable Q/A pairs
// from the task plus whatever the caller prepends (memory excerpts).
const GenSystem = `You build a knowledge checklist for a coding agent about to start a task. Produce the concrete facts the agent must already know (files involved, conventions, commands, pitfalls) as JSON only:
[{"q":"<question>","expected":"<short ground-truth answer>"}]
3 to 8 items. Checkable facts only — no generic advice, no narration.`

// BookSystem is the closed-book answerer prompt.
const BookSystem = ClosedBookSystem + ` Answer every question. Reply with JSON only, mapping each question verbatim to its answer: {"<question>":"<answer>"}.`

// ParseChecklist extracts the item array from a generator reply,
// tolerating code fences and surrounding prose.
func ParseChecklist(s string) ([]Item, error) {
	body := extractJSON(s, '[', ']')
	if body == "" {
		return nil, fmt.Errorf("no checklist array found")
	}
	var items []Item
	if err := json.Unmarshal([]byte(body), &items); err != nil {
		return nil, fmt.Errorf("parse checklist: %w", err)
	}
	out := items[:0]
	for _, it := range items {
		if strings.TrimSpace(it.Q) != "" {
			out = append(out, it)
		}
	}
	return out, nil
}

// ParseBook extracts the {question: answer} map from a book reply.
func ParseBook(s string) (map[string]string, error) {
	body := extractJSON(s, '{', '}')
	if body == "" {
		return nil, fmt.Errorf("no answer object found")
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return nil, fmt.Errorf("parse answers: %w", err)
	}
	return m, nil
}

// extractJSON returns the first balanced [...] / {...} slice of s with
// fences and prose stripped (same tolerance as the title/diagnosis
// parsers: a messy reply must not crash the gate).
func extractJSON(s string, open, close byte) string {
	s = strings.ReplaceAll(s, "```json", "")
	s = strings.ReplaceAll(s, "```", "")
	start := strings.IndexByte(s, open)
	if start < 0 {
		return ""
	}
	depth := 0
	for i := start; i < len(s); i++ {
		switch s[i] {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

// RunCoverage runs the two-stage check (design §2.1): generate the
// checklist from the task, answer it closed-book, grade against ground
// truth. The returned injection lines are ready-to-append context for the
// gate's miss path ("Q → expected"), one per gap.
func RunCoverage(ctx context.Context, gen ChecklistGen, book Book, task string) (Coverage, []string, error) {
	raw, err := gen(ctx, clipRunes(task, checklistGenCap))
	if err != nil {
		return Coverage{}, nil, fmt.Errorf("checklist: %w", err)
	}
	items, err := ParseChecklist(raw)
	if err != nil {
		return Coverage{}, nil, err
	}
	c := Coverage{Items: len(items)}
	if len(items) == 0 {
		c.Score = 1 // nothing to prove — an empty checklist never blocks work
		return c, nil, nil
	}
	qs := make([]string, len(items))
	for i, it := range items {
		qs[i] = it.Q
	}
	replies, err := book(ctx, qs)
	if err != nil {
		return Coverage{}, nil, fmt.Errorf("closed-book answers: %w", err)
	}
	answers, err := ParseBook(replies)
	if err != nil {
		return Coverage{}, nil, err
	}
	var inject []string
	for _, it := range items {
		if checkAnswer(it.asQuestion(), answers[it.Q]) {
			c.Hits++
			continue
		}
		c.Misses = append(c.Misses, it)
		inject = append(inject, fmt.Sprintf("%s → %s",
			clipRunes(it.Q, 120), clipRunes(it.Expected, 120)))
	}
	c.Score = float64(c.Hits) / float64(len(items))
	return c, inject, nil
}

// asQuestion adapts an Item to the grader's Question shape (Expected
// substring, no keywords — coverage grading is exact-fact).
func (it Item) asQuestion() Question {
	return Question{Q: it.Q, Expected: it.Expected}
}
