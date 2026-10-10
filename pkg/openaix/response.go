package openaix

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// oaiResponse is a non-streaming OpenAI chat.completion (and, with no
// choices, also the shape of a streaming chunk's envelope).
type oaiResponse struct {
	ID      string        `json:"id"`
	Model   string        `json:"model"`
	Choices []oaiChoice   `json:"choices"`
	Usage   *oaiUsage     `json:"usage"`
	Error   *oaiErrorBody `json:"error"`
}

type oaiChoice struct {
	Index        int        `json:"index"`
	Message      oaiMessage `json:"message"`
	FinishReason *string    `json:"finish_reason"`
}

// oaiChunk is one streaming chunk: delta instead of message, plus optional
// trailing usage.
type oaiChunk struct {
	ID      string           `json:"id"`
	Model   string           `json:"model"`
	Choices []oaiChunkChoice `json:"choices"`
	Usage   *oaiUsage        `json:"usage"`
	Error   *oaiErrorBody    `json:"error"`
}

type oaiChunkChoice struct {
	Index        int      `json:"index"`
	Delta        oaiDelta `json:"delta"`
	FinishReason *string  `json:"finish_reason"`
}

type oaiDelta struct {
	Role      string            `json:"role"`
	Content   *string           `json:"content"`
	ToolCalls []oaiChunkToolSeq `json:"tool_calls"`
}

type oaiChunkToolSeq struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaiUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

type oaiErrorBody struct {
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// anthropicUsage mirrors the Anthropic usage object. All four fields are
// always present (zero included): the SDK's accumulator treats a *present*
// field in message_delta as authoritative and overwrites message_start's.
type anthropicUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

func (u *oaiUsage) anthropic() anthropicUsage {
	var out anthropicUsage
	if u == nil {
		return out
	}
	out.InputTokens = int64(u.PromptTokens)
	out.OutputTokens = int64(u.CompletionTokens)
	if u.PromptTokensDetails != nil {
		out.CacheReadInputTokens = int64(u.PromptTokensDetails.CachedTokens)
	}
	return out
}

// anthropicOutBlock is one block of the translated response message. Text
// and tool shapes are separate structs so empty text still marshals with an
// explicit "" and tool blocks never carry a stray text field.
type anthropicTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicToolBlock struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// anthropicResponse is the non-streaming message document (also the shape
// carried inside message_start, built inline there).
type anthropicResponse struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"` // "message"
	Role         string         `json:"role"` // "assistant"
	Model        string         `json:"model"`
	Content      []any          `json:"content"`
	StopReason   string         `json:"stop_reason"`
	StopSequence *string        `json:"stop_sequence"`
	Usage        anthropicUsage `json:"usage"`
}

// mapStopReason folds OpenAI's finish_reason vocabulary onto Anthropic's.
// Unknown reasons degrade to end_turn — the SDK and our runner only branch
// on tool_use / max_tokens.
func mapStopReason(r string) string {
	switch r {
	case "tool_calls", "function_call":
		return "tool_use"
	case "length":
		return "max_tokens"
	default: // stop, stop_sequence, content_filter, "", unknown
		return "end_turn"
	}
}

// msgID normalizes the response id into Anthropic's msg_ prefix namespace.
func msgID(s string) string {
	switch {
	case s == "":
		return "msg_" + time.Now().Format("150405.000000")
	case strings.HasPrefix(s, "msg_"):
		return s
	case strings.HasPrefix(s, "chatcmpl-"):
		return "msg_" + strings.TrimPrefix(s, "chatcmpl-")
	default:
		return "msg_" + s
	}
}

// translateError renders any non-2xx body as an Anthropic error document
// while keeping the original HTTP status so SDK retries and error typing
// behave. Bodies that are already Anthropic-shaped pass through.
func translateError(status int, body []byte) []byte {
	return mustMarshal(errorPayload(status, body))
}

// errorPayload is the map form of translateError, for SSE error frames.
func errorPayload(status int, body []byte) any {
	var probe struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(body, &probe) == nil && probe.Type == "error" {
		var m map[string]any
		if json.Unmarshal(body, &m) == nil {
			return m
		}
	}
	var oe oaiErrorBody
	msg := ""
	if json.Unmarshal(body, &oe) == nil && oe.Error != nil {
		msg = oe.Error.Message
	}
	if strings.TrimSpace(msg) == "" {
		msg = strings.TrimSpace(string(body))
		if len(msg) > 2048 {
			msg = msg[:2048]
		}
		if msg == "" {
			msg = http.StatusText(status)
		}
	}
	return map[string]any{
		"type":  "error",
		"error": map[string]string{"type": errorTypeFor(status), "message": msg},
	}
}

// errorTypeFor maps HTTP status onto Anthropic's error type vocabulary.
func errorTypeFor(status int) string {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case 529:
		return "overloaded_error"
	default:
		if status >= 500 {
			return "api_error"
		}
		return "invalid_request_error"
	}
}
