package agent

import (
	"github.com/anthropics/anthropic-sdk-go"
)

// serverToolDefs returns the server-side web tools: Anthropic executes them
// inside the API request, so they cost the client no round trips. The 20260209
// variants (dynamic filtering) are the current ones for Opus 5.5-class
// models. Order is fixed — the tools array is part of the prompt-cache
// prefix, so it must be byte-stable across runner recreations.
func serverToolDefs() []anthropic.BetaToolUnionParam {
	return []anthropic.BetaToolUnionParam{
		{OfWebSearchTool20260209: &anthropic.BetaWebSearchTool20260209Param{
			MaxUses: anthropic.Int(5),
		}},
		{OfWebFetchTool20260209: &anthropic.BetaWebFetchTool20260209Param{
			MaxUses:          anthropic.Int(5),
			MaxContentTokens: anthropic.Int(8000),
		}},
	}
}

// ServerToolNames lists the server tools this engine offers (for `lyon tools`
// and logs). They appear in requests only when web tools are enabled.
func ServerToolNames() []string {
	return []string{"web_search (server-side)", "web_fetch (server-side)"}
}
