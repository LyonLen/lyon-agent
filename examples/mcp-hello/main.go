// Command mcp-hello is a minimal MCP stdio server exposing one greet tool.
// Run it through the lyon CLI:
//
//	lyon run --mcp "hello=go run ./examples/mcp-hello" "greet Ada"
package main

import (
	"context"
	"fmt"
	"os"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type greetArgs struct {
	Name string `json:"name" jsonschema:"the person to greet"`
}

func main() {
	server := mcp.NewServer(&mcp.Implementation{Name: "hello", Version: "1.0.0"}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "greet",
		Description: "Say hello to someone by name.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args greetArgs) (*mcp.CallToolResult, any, error) {
		if args.Name == "" {
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: "name is required"}},
			}, nil, nil
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Hi %s, welcome to lyon-agent!", args.Name)}},
		}, nil, nil
	})

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "mcp-hello:", err)
		os.Exit(1)
	}
}
