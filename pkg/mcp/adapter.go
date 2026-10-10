package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"lyon-agent/pkg/tools"
)

// Adapter exposes one remote MCP tool as a local agent tool. The exposed
// name is mcp_<server>_<tool> (sanitized), deterministic across runs so the
// prompt cache prefix stays stable.
type Adapter struct {
	conn   *Connection
	orig   string // the tool's name on the server
	id     string // the name the model sees
	desc   string
	schema anthropic.BetaToolInputSchemaParam
}

func newAdapter(c *Connection, t *mcpsdk.Tool) *Adapter {
	desc := t.Description
	if strings.TrimSpace(desc) == "" {
		desc = fmt.Sprintf("MCP tool %q from server %q.", t.Name, c.Name)
	}
	return &Adapter{
		conn:   c,
		orig:   t.Name,
		id:     "mcp_" + sanitize(c.Name) + "_" + sanitize(t.Name),
		desc:   desc,
		schema: anthropicSchema(t.InputSchema),
	}
}

// Name implements tools.Tool.
func (a *Adapter) Name() string { return a.id }

// Description implements tools.Tool.
func (a *Adapter) Description() string { return a.desc }

// InputSchema implements tools.Tool.
func (a *Adapter) InputSchema() anthropic.BetaToolInputSchemaParam { return a.schema }

// ParallelSafe reports true: the per-connection mutex inside Call already
// serializes calls to the same server, so the session's serial lock (which
// covers workspace mutation) should not gate remote reads/writes of other
// servers. Different servers therefore run in parallel.
func (a *Adapter) ParallelSafe() bool { return true }

// Execute implements tools.Tool.
func (a *Adapter) Execute(ctx context.Context, input json.RawMessage) ([]anthropic.BetaToolResultBlockParamContentUnion, error) {
	text, err := a.conn.Call(ctx, a.orig, input)
	if err != nil {
		return nil, err
	}
	return tools.TextResult(text), nil
}

// sanitize maps a server/tool name onto the model-friendly charset
// [a-z0-9_]: lowercased, everything else collapsed to underscores.
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '.' || r == ' ' || r == '/':
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "server"
	}
	return b.String()
}
