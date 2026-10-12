package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// TrajectoryCap bounds what of the failed run is shown to the diagnoser.
const TrajectoryCap = 8000

// Diagnosis is the structured post-mortem of a failed attempt (design
// §1.3): which checkable conditions were violated, plus a patch — a
// short constraint to append to the candidate's instructions before the
// retry.
type Diagnosis struct {
	FailedConditions []string `json:"failed_conditions"`
	Patch            string   `json:"patch"`
}

// Asker is one model call (system + user → text). Wired to a cheap
// subagent-tier model in production; tests pass a fake.
type Asker func(ctx context.Context, system, user string) (string, error)

// DiagnoseSystem is the summarizer prompt: actionable rules, JSON only.
const DiagnoseSystem = `You post-mortem a failed agent attempt on a task with a known correct answer. Look at the trajectory and the failure, then reply with JSON only:
{"failed_conditions":["<checkable condition that was violated — what must be done/verified next time>"],"patch":"<one short constraint sentence to append to the agent's instructions>"}
Conditions must be rules someone could verify, not narration. One to five items. No prose outside the JSON.`

// SummarizeFailure feeds the failed trajectory to the diagnoser and
// parses its reply. A malformed reply yields an empty Diagnosis, never
// an error — a bad post-mortem must not block the retry it feeds.
func SummarizeFailure(ctx context.Context, ask Asker, trajectory string) (Diagnosis, error) {
	reply, err := ask(ctx, DiagnoseSystem, clipRunes(trajectory, TrajectoryCap))
	if err != nil {
		return Diagnosis{}, err
	}
	return ParseDiagnosis(reply), nil
}

// ParseDiagnosis extracts {failed_conditions, patch} from a model reply,
// tolerating fences and stray prose.
func ParseDiagnosis(s string) Diagnosis {
	body := extractJSON(s, '{', '}')
	if body == "" {
		return Diagnosis{}
	}
	var d Diagnosis
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		return Diagnosis{}
	}
	out := d.FailedConditions[:0]
	for _, c := range d.FailedConditions {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	d.FailedConditions = out
	d.Patch = strings.TrimSpace(d.Patch)
	return d
}

// Constraints renders the diagnosis as the block appended to a retried
// node prompt (empty when the diagnosis found nothing usable).
func (d Diagnosis) Constraints() string {
	if len(d.FailedConditions) == 0 && d.Patch == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nLessons from the previous failed attempt (must hold this time):")
	for _, c := range d.FailedConditions {
		b.WriteString("\n- ")
		b.WriteString(c)
	}
	if d.Patch != "" {
		b.WriteString("\n- ")
		b.WriteString(d.Patch)
	}
	return b.String()
}

// String renders a one-line summary for logs/cmdOut.
func (d Diagnosis) String() string {
	return fmt.Sprintf("%d condition(s), patch=%q", len(d.FailedConditions), d.Patch)
}
