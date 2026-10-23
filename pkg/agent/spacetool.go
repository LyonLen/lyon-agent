package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/toolrunner"

	"lyon-agent/pkg/space"
	"lyon-agent/pkg/tools"
)

// spaceInput is the typed input of the space tool. Action selects the
// operation; Name/Description/Body feed the save operations; Kind+Name
// select an item for share. The acting agent is the SESSION's identity
// (SetAgent via routing) — the model never picks whose space it writes.
type spaceInput struct {
	Action      string `json:"action" jsonschema:"one of list | save_knowledge | save_skill | share"`
	Name        string `json:"name,omitempty" jsonschema:"item name (required for save_*, and for share)"`
	Description string `json:"description,omitempty" jsonschema:"one-line description for a save"`
	Body        string `json:"body,omitempty" jsonschema:"markdown body for a save (knowledge) or SKILL.md body (skill)"`
	Kind        string `json:"kind,omitempty" jsonschema:"for share: knowledge | skills"`
	Endorser    string `json:"endorser,omitempty" jsonschema:"for share: the endorsing agent id (another agent or contest:<class>)"`
}

// spaceTool is the composite unit over the agent's specialist space
// (pkg/space): list what the current agent carries, save new knowledge
// or skills into ITS OWN space, and share items for consensus
// promotion. ParallelSafe stays true — pkg/space serializes its own
// disk writes, and the tool never touches session state besides the
// read-only Agent() identity.
type spaceTool struct {
	inner anthropic.BetaTool
}

// newSpaceTool builds the space tool. Schema generation failure means
// the struct tags are malformed — a developer error, so it panics loudly.
func newSpaceTool() tools.Tool {
	desc := "Manage your specialist agent space — knowledge documents and skills that persist across sessions. " +
		"`list` shows what you carry (yours plus the shared commons). `save_knowledge` writes a markdown " +
		"document into your space; `save_skill` writes a SKILL.md playbook. `share` endorses one of your " +
		"items from another source — two distinct endorsements promote the item to the shared commons every " +
		"agent can see."
	inner, err := toolrunner.NewBetaToolFromJSONSchema[spaceInput](
		"space", desc,
		func(_ context.Context, _ spaceInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			// Unreachable: Execute below handles the raw input itself.
			return anthropic.BetaToolResultBlockParamContentUnion{}, fmt.Errorf("space: use Execute")
		},
	)
	if err != nil {
		panic(fmt.Sprintf("agent: space schema: %v", err))
	}
	return &spaceTool{inner: inner}
}

// Name implements tools.Tool.
func (t *spaceTool) Name() string { return t.inner.Name() }

// Description implements tools.Tool.
func (t *spaceTool) Description() string { return t.inner.Description() }

// InputSchema implements tools.Tool.
func (t *spaceTool) InputSchema() anthropic.BetaToolInputSchemaParam { return t.inner.InputSchema() }

// ParallelSafe is true: disk writes are serialized inside pkg/space,
// and the tool only reads the session's agent identity.
func (t *spaceTool) ParallelSafe() bool { return true }

// Execute implements tools.Tool. The acting agent comes from the
// session identity — the model cannot write into another agent's space.
func (t *spaceTool) Execute(ctx context.Context, input json.RawMessage) ([]anthropic.BetaToolResultBlockParamContentUnion, error) {
	var in spaceInput
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, fmt.Errorf("invalid input: %w", err)
	}
	agent := "self"
	if sess := SessionFromContext(ctx); sess != nil {
		agent = sess.Agent()
	}
	var out string
	var err error
	switch strings.TrimSpace(strings.ToLower(in.Action)) {
	case "list":
		out = listSpace(agent)
	case "save_knowledge":
		out, err = saveSpaceItem(agent, space.KindKnowledge, in)
	case "save_skill":
		out, err = saveSpaceItem(agent, space.KindSkills, in)
	case "share":
		out, err = shareSpaceItem(agent, in)
	case "":
		return nil, fmt.Errorf("space: action is required (list | save_knowledge | save_skill | share)")
	default:
		return nil, fmt.Errorf("space: unknown action %q (want list | save_knowledge | save_skill | share)", in.Action)
	}
	if err != nil {
		return nil, err
	}
	return tools.TextResult(out), nil
}

// listSpace renders the agent's two stores plus the shared commons.
func listSpace(agent string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "space of agent %q\n", agent)
	renderKind(&b, "knowledge", space.List(agent, space.KindKnowledge))
	renderKind(&b, "skills", space.List(agent, space.KindSkills))
	renderKind(&b, "commons knowledge", space.ListCommons(space.KindKnowledge))
	renderKind(&b, "commons skills", space.ListCommons(space.KindSkills))
	return b.String()
}

func renderKind(b *strings.Builder, label string, items []space.Item) {
	fmt.Fprintf(b, "\n%s (%d):\n", label, len(items))
	if len(items) == 0 {
		b.WriteString("  (empty)\n")
		return
	}
	for _, it := range items {
		desc := it.Description
		if desc == "" {
			desc = "(no description)"
		}
		fmt.Fprintf(b, "  - %s: %s [endorsed by: %s]\n",
			it.Name, desc, strings.Join(it.Endorsements, ", "))
	}
}

// saveSpaceItem routes a save to the right store. Skills get the same
// one-line summary back as knowledge so the model sees both stuck.
func saveSpaceItem(agent, kind string, in spaceInput) (string, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return "", fmt.Errorf("space: %s needs a name", kind)
	}
	if strings.TrimSpace(in.Body) == "" {
		return "", fmt.Errorf("space: %s needs a body", kind)
	}
	var it space.Item
	var err error
	if kind == space.KindSkills {
		it, err = space.SaveSkill(agent, name, in.Description, in.Body)
	} else {
		it, err = space.SaveKnowledge(agent, name, in.Description, in.Body)
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("saved %s %q in your space (endorsed by: %s). Two distinct endorsements promote it to the shared commons.",
		kind, it.Name, strings.Join(it.Endorsements, ", ")), nil
}

// shareSpaceItem endorses one of the agent's items from another source.
// Real endorsements only: the endorser must be a distinct agent id or a
// "contest:<class>" election result — never the owner themself.
func shareSpaceItem(agent string, in spaceInput) (string, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return "", fmt.Errorf("space: share needs an item name")
	}
	kind := strings.TrimSpace(in.Kind)
	if kind == "" {
		kind = space.KindKnowledge
	}
	if kind != space.KindKnowledge && kind != space.KindSkills {
		return "", fmt.Errorf("space: share kind must be knowledge or skills, got %q", in.Kind)
	}
	endorser := strings.TrimSpace(in.Endorser)
	if endorser == "" {
		return "", fmt.Errorf("space: share needs an endorser (another agent id or contest:<class>)")
	}
	if endorser == agent {
		return "", fmt.Errorf("space: an item is already endorsed by its owner — a share needs a second source")
	}
	promoted, err := space.Share(agent, kind, name, endorser)
	if err != nil {
		return "", err
	}
	if promoted {
		return fmt.Sprintf("shared %s %q — consensus reached (2+ endorsements), promoted to the commons.", kind, name), nil
	}
	return fmt.Sprintf("endorsed %s %q by %s — waiting for consensus (2+ distinct endorsements promote to commons).",
		kind, name, endorser), nil
}
