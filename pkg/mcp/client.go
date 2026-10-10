// Package mcp connects lyon to Model Context Protocol servers — stdio
// subprocesses or streamable HTTP endpoints — and adapts their tools into
// the agent's tool registry under deterministic mcp_<server>_<tool> names.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/anthropics/anthropic-sdk-go"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"lyon-agent/pkg/tools"
)

// Config describes one MCP server.
type Config struct {
	// Name is unique per process and prefixes every tool: mcp_<name>_<tool>.
	Name string
	// Command is the stdio transport: argv[0] is the executable. Mutually
	// exclusive with URL.
	Command []string
	// URL selects the streamable HTTP transport.
	URL string
	// Env is appended to the process environment for stdio servers.
	Env []string
}

// Connection owns one MCP session. It is safe for concurrent use: calls are
// serialized per connection (servers are frequently single-threaded), while
// different connections run in parallel.
type Connection struct {
	Name      string
	session   *mcpsdk.ClientSession
	mu        sync.Mutex
	closeOnce sync.Once
	closeErr  error
}

// Connect dials the server described by cfg.
func Connect(ctx context.Context, cfg Config) (*Connection, error) {
	if strings.TrimSpace(cfg.Name) == "" {
		return nil, fmt.Errorf("mcp server name is required")
	}
	if cfg.URL != "" && len(cfg.Command) > 0 {
		return nil, fmt.Errorf("mcp %q: command and url are mutually exclusive", cfg.Name)
	}

	var transport mcpsdk.Transport
	switch {
	case cfg.URL != "":
		transport = &mcpsdk.StreamableClientTransport{Endpoint: cfg.URL}
	case len(cfg.Command) > 0:
		cmd := exec.Command(cfg.Command[0], cfg.Command[1:]...)
		cmd.Env = append(os.Environ(), cfg.Env...)
		transport = &mcpsdk.CommandTransport{Command: cmd}
	default:
		return nil, fmt.Errorf("mcp %q: need a command or a url", cfg.Name)
	}
	return ConnectTransport(ctx, cfg.Name, transport)
}

// ConnectTransport dials an already-constructed transport. Exported so tests
// (in-memory transports) and custom wiring can bypass Config.
func ConnectTransport(ctx context.Context, name string, transport mcpsdk.Transport) (*Connection, error) {
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "lyon-agent", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("connect mcp %q: %w", name, err)
	}
	return &Connection{Name: name, session: session}, nil
}

// Tools lists the server's tools and adapts each to the agent Tool interface.
func (c *Connection) Tools(ctx context.Context) ([]tools.Tool, error) {
	var all []*mcpsdk.Tool
	cursor := ""
	for {
		params := &mcpsdk.ListToolsParams{}
		if cursor != "" {
			params.Cursor = cursor
		}
		res, err := c.session.ListTools(ctx, params)
		if err != nil {
			return nil, fmt.Errorf("list tools from mcp %q: %w", c.Name, err)
		}
		all = append(all, res.Tools...)
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	out := make([]tools.Tool, 0, len(all))
	for _, t := range all {
		out = append(out, newAdapter(c, t))
	}
	return out, nil
}

// Call invokes one server tool. A result with isError (or a protocol error)
// comes back as a Go error, which the agent loop reports as an is_error
// tool_result so the model can adapt.
func (c *Connection) Call(ctx context.Context, name string, args json.RawMessage) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var arguments any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &arguments); err != nil {
			return "", fmt.Errorf("invalid arguments for %s.%s: %w", c.Name, name, err)
		}
	}
	res, err := c.session.CallTool(ctx, &mcpsdk.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		return "", fmt.Errorf("mcp call %s.%s: %w", c.Name, name, err)
	}

	var text strings.Builder
	for _, part := range res.Content {
		if t, ok := part.(*mcpsdk.TextContent); ok {
			if text.Len() > 0 {
				text.WriteByte('\n')
			}
			text.WriteString(t.Text)
			continue
		}
		fmt.Fprintf(&text, "[%T]", part)
	}
	if res.IsError {
		if text.Len() == 0 {
			return "", fmt.Errorf("mcp tool %s.%s returned an error", c.Name, name)
		}
		return "", fmt.Errorf("mcp tool %s.%s: %s", c.Name, name, text.String())
	}
	return text.String(), nil
}

// Close terminates the session (idempotent).
func (c *Connection) Close() error {
	c.closeOnce.Do(func() { c.closeErr = c.session.Close() })
	return c.closeErr
}

// Manager tracks every connection of one process for unified listing and
// shutdown.
type Manager struct {
	mu    sync.Mutex
	conns []*Connection
}

// NewManager creates an empty manager.
func NewManager() *Manager { return &Manager{} }

// Connect dials cfg and records the connection on success.
func (m *Manager) Connect(ctx context.Context, cfg Config) (*Connection, error) {
	conn, err := Connect(ctx, cfg)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.conns = append(m.conns, conn)
	m.mu.Unlock()
	return conn, nil
}

// Tools lists tools from every connection.
func (m *Manager) Tools(ctx context.Context) ([]tools.Tool, error) {
	m.mu.Lock()
	conns := append([]*Connection(nil), m.conns...)
	m.mu.Unlock()
	var out []tools.Tool
	for _, c := range conns {
		ts, err := c.Tools(ctx)
		if err != nil {
			return out, err // partial: already-collected tools stay usable
		}
		out = append(out, ts...)
	}
	return out, nil
}

// Names returns the connected server names in connection order.
func (m *Manager) Names() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.conns))
	for _, c := range m.conns {
		out = append(out, c.Name)
	}
	return out
}

// Close shuts down every connection (reverse order), collecting the first
// error but always closing everything.
func (m *Manager) Close() error {
	m.mu.Lock()
	conns := m.conns
	m.conns = nil
	m.mu.Unlock()
	var first error
	for i := len(conns) - 1; i >= 0; i-- {
		if err := conns[i].Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// anthropicSchema converts an MCP JSON schema into the Anthropic tool input
// schema via the same apijson path the SDK uses for its own schemas.
func anthropicSchema(raw any) anthropic.BetaToolInputSchemaParam {
	var out anthropic.BetaToolInputSchemaParam
	if raw != nil {
		data, err := json.Marshal(raw)
		if err == nil {
			_ = json.Unmarshal(data, &out)
		}
	}
	if out.Properties == nil {
		out.Properties = map[string]any{}
	}
	return out
}
