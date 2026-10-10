package openaix

import (
	"encoding/json"
	"strings"
)

// anthropicRequest is the subset of the Anthropic Messages request body the
// translation needs; everything else (cache_control, betas, output_config,
// fallbacks, metadata) is gateway-specific noise and is dropped.
type anthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int64              `json:"max_tokens"`
	System        json.RawMessage    `json:"system"`
	Messages      []anthropicMessage `json:"messages"`
	Tools         []anthropicTool    `json:"tools"`
	ToolChoice    json.RawMessage    `json:"tool_choice"`
	StopSequences []string           `json:"stop_sequences"`
	Temperature   *float64           `json:"temperature"`
	TopP          *float64           `json:"top_p"`
	Stream        bool               `json:"stream"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// anthropicBlock is one content block. Only the fields the translator reads
// are declared; thinking, cache_control, citations, etc. decode into the
// void.
type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
	Source    json.RawMessage `json:"source"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// oaiMessage doubles as the OpenAI request message we build and the
// response message we decode: content is a string, a part array, or null.
type oaiMessage struct {
	Role       string        `json:"role"`
	Content    any           `json:"content"`
	ToolCalls  []oaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
}

type oaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaiPart struct {
	Type     string       `json:"type"`
	Text     string       `json:"text,omitempty"`
	ImageURL *oaiImageURL `json:"image_url,omitempty"`
}

type oaiImageURL struct {
	URL string `json:"url"`
}

type oaiRequest struct {
	Model         string       `json:"model"`
	Messages      []oaiMessage `json:"messages"`
	MaxTokens     int64        `json:"max_tokens"`
	Stream        bool         `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
	Tools       []oaiTool `json:"tools,omitempty"`
	ToolChoice  any       `json:"tool_choice,omitempty"`
	Stop        []string  `json:"stop,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
	TopP        *float64  `json:"top_p,omitempty"`
}

type oaiTool struct {
	Type     string `json:"type"` // "function"
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// translateRequest converts a decoded Anthropic request into the OpenAI
// chat/completions shape.
func translateRequest(a *anthropicRequest) *oaiRequest {
	o := &oaiRequest{
		Model:       a.Model,
		MaxTokens:   a.MaxTokens,
		Stream:      a.Stream,
		Temperature: a.Temperature,
		TopP:        a.TopP,
	}
	if len(a.StopSequences) > 0 {
		o.Stop = a.StopSequences
	}
	if a.Stream {
		o.StreamOptions = &struct {
			IncludeUsage bool `json:"include_usage"`
		}{IncludeUsage: true}
	}

	// Frozen system prompt (block array or plain string) becomes the first
	// system message.
	if sys := systemText(a.System); sys != "" {
		o.Messages = append(o.Messages, oaiMessage{Role: "system", Content: sys})
	}
	for _, m := range a.Messages {
		o.Messages = append(o.Messages, expandMessage(m)...)
	}
	o.Messages = mergeMessages(o.Messages)

	for _, tl := range a.Tools {
		if tl.Name == "" || len(tl.InputSchema) == 0 {
			continue // server-side tool variants have no function shape
		}
		tool := oaiTool{Type: "function"}
		tool.Function.Name = tl.Name
		tool.Function.Description = tl.Description
		tool.Function.Parameters = tl.InputSchema
		o.Tools = append(o.Tools, tool)
	}
	if tc := mapToolChoice(a.ToolChoice, len(o.Tools) > 0); tc != nil {
		o.ToolChoice = tc
	}
	return o
}

// systemText flattens the top-level system field (string or block array).
func systemText(raw json.RawMessage) string {
	s, blocks := parseContent(raw)
	if len(blocks) == 0 {
		return s
	}
	parts := make([]string, 0, len(blocks)+1)
	if s != "" {
		parts = append(parts, s)
	}
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// parseContent accepts both wire shapes Anthropic uses: a bare string and a
// block array.
func parseContent(raw json.RawMessage) (string, []anthropicBlock) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		return "", blocks
	}
	return "", nil
}

// expandMessage maps one Anthropic message to the OpenAI sequence that
// replaces it: tool results become standalone role:"tool" messages (they
// must directly follow the assistant tool_calls), everything else stays in
// its role.
func expandMessage(m anthropicMessage) []oaiMessage {
	s, blocks := parseContent(m.Content)
	switch m.Role {
	case "system":
		// System notes queued mid-conversation (PushSystemNote) pass
		// through as system messages where they stand.
		if s == "" && len(blocks) > 0 {
			s = systemText(m.Content)
		}
		return []oaiMessage{{Role: "system", Content: s}}

	case "assistant":
		text := s
		var calls []oaiToolCall
		for _, b := range blocks {
			switch b.Type {
			case "text":
				text = joinText(text, b.Text)
			case "tool_use":
				call := oaiToolCall{ID: b.ID, Type: "function"}
				call.Function.Name = b.Name
				call.Function.Arguments = argsString(b.Input)
				calls = append(calls, call)
			default:
				// thinking / redacted_thinking / server_tool_use: no
				// OpenAI equivalent, drop.
			}
		}
		if text == "" && len(calls) == 0 {
			return nil // empty assistant message would be rejected upstream
		}
		out := oaiMessage{Role: "assistant", ToolCalls: calls}
		if text != "" {
			out.Content = text
		}
		return []oaiMessage{out}

	case "user":
		if len(blocks) == 0 {
			return []oaiMessage{{Role: "user", Content: s}}
		}
		var out []oaiMessage
		var parts []oaiPart
		for _, b := range blocks {
			switch b.Type {
			case "text":
				parts = append(parts, oaiPart{Type: "text", Text: b.Text})
			case "image":
				if p, ok := imagePart(b.Source); ok {
					parts = append(parts, p)
				}
			case "tool_result":
				out = append(out, oaiMessage{
					Role:       "tool",
					ToolCallID: b.ToolUseID,
					Content:    toolResultText(b),
				})
			default:
				// documents / unknown blocks: keep a breadcrumb rather
				// than silently vanishing.
				parts = append(parts, oaiPart{Type: "text", Text: "[unsupported block: " + b.Type + "]"})
			}
		}
		if len(parts) > 0 {
			out = append(out, oaiMessage{Role: "user", Content: partsOrString(parts)})
		}
		return out

	default:
		if s == "" && len(blocks) > 0 {
			s = systemText(m.Content)
		}
		return []oaiMessage{{Role: m.Role, Content: s}}
	}
}

// toolResultText renders a tool_result payload as the plain string the
// OpenAI tool role requires, with error and image markers preserved.
func toolResultText(tr anthropicBlock) string {
	s, blocks := parseContent(tr.Content)
	pieces := make([]string, 0, len(blocks)+1)
	if s != "" {
		pieces = append(pieces, s)
	}
	for _, bl := range blocks {
		switch bl.Type {
		case "text":
			if bl.Text != "" {
				pieces = append(pieces, bl.Text)
			}
		case "image":
			pieces = append(pieces, "[image omitted]")
		}
	}
	out := strings.Join(pieces, "\n")
	if tr.IsError {
		out = "[tool error] " + out
	}
	if strings.TrimSpace(out) == "" {
		return "(empty)"
	}
	return out
}

// imagePart converts an Anthropic image source (base64 or url) into an
// OpenAI image_url part.
func imagePart(src json.RawMessage) (oaiPart, bool) {
	var s struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	}
	if len(src) == 0 || json.Unmarshal(src, &s) != nil {
		return oaiPart{}, false
	}
	var url string
	switch s.Type {
	case "base64":
		if s.Data == "" {
			return oaiPart{}, false
		}
		media := s.MediaType
		if media == "" {
			media = "image/png"
		}
		url = "data:" + media + ";base64," + s.Data
	case "url":
		url = s.URL
	default:
		return oaiPart{}, false
	}
	if url == "" {
		return oaiPart{}, false
	}
	return oaiPart{Type: "image_url", ImageURL: &oaiImageURL{URL: url}}, true
}

// argsString renders tool_use input as the JSON string OpenAI expects.
func argsString(input json.RawMessage) string {
	trimmed := strings.TrimSpace(string(input))
	if trimmed == "" || !json.Valid([]byte(trimmed)) || !strings.HasPrefix(trimmed, "{") {
		return "{}"
	}
	return trimmed
}

func partsOrString(parts []oaiPart) any {
	if len(parts) == 1 && parts[0].Type == "text" {
		return parts[0].Text
	}
	return parts
}

func joinText(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "\n\n" + b
	}
}

// mergeMessages collapses adjacent same-role user/assistant messages —
// compaction summaries, system-note turns, and interrupted-history repairs
// all produce runs that some strict gateways reject unmerged. Tool and
// system messages are left exactly where they are.
func mergeMessages(ms []oaiMessage) []oaiMessage {
	if len(ms) < 2 {
		return ms
	}
	out := make([]oaiMessage, 0, len(ms))
	for _, m := range ms {
		if n := len(out); n > 0 && m.Role == out[n-1].Role &&
			(m.Role == "user" || m.Role == "assistant") {
			out[n-1] = mergePair(out[n-1], m)
			continue
		}
		out = append(out, m)
	}
	return out
}

func mergePair(a, b oaiMessage) oaiMessage {
	a.ToolCalls = append(a.ToolCalls, b.ToolCalls...)
	a.Content = mergeContent(a.Content, b.Content)
	return a
}

func mergeContent(a, b any) any {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	as, aok := a.(string)
	bs, bok := b.(string)
	if aok && bok {
		return joinText(as, bs)
	}
	return append(contentParts(a), contentParts(b)...)
}

func contentParts(c any) []oaiPart {
	switch v := c.(type) {
	case nil:
		return nil
	case string:
		if v == "" {
			return nil
		}
		return []oaiPart{{Type: "text", Text: v}}
	case []oaiPart:
		return v
	default:
		return nil
	}
}

// mapToolChoice converts Anthropic tool_choice into the OpenAI vocabulary;
// nil means omit the field (OpenAI defaults to auto when tools exist).
func mapToolChoice(raw json.RawMessage, hasTools bool) any {
	if len(raw) == 0 || string(raw) == "null" {
		if hasTools {
			return "auto"
		}
		return nil
	}
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &tc) != nil {
		if hasTools {
			return "auto"
		}
		return nil
	}
	switch tc.Type {
	case "none":
		return "none"
	case "any":
		return "required"
	case "tool":
		if tc.Name == "" {
			return "auto"
		}
		return map[string]any{
			"type":     "function",
			"function": map[string]string{"name": tc.Name},
		}
	default: // "auto" and anything unrecognized
		if hasTools {
			return "auto"
		}
		return nil
	}
}
