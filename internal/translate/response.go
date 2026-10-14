package translate

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// ReasoningSignature marks thinking blocks rollet built from a provider's
// reasoning_content. Claude Code stores the block and sends it back, which is
// how the reasoning survives to the next request.
const ReasoningSignature = "rollet-reasoning"

type openAIResponse struct {
	Choices []struct {
		Message struct {
			Content *string `json:"content"`
			Reasoning
			ToolCalls []openAIToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *openAIUsage `json:"usage"`
}

// Reasoning covers both spellings in the wild: reasoning_content (DeepSeek,
// Qwen, GLM, Kimi, MiMo) and reasoning (OpenRouter).
type Reasoning struct {
	ReasoningContent *string `json:"reasoning_content"`
	ReasoningAlt     *string `json:"reasoning"`
}

func (r Reasoning) Text() string {
	if r.ReasoningContent != nil && *r.ReasoningContent != "" {
		return *r.ReasoningContent
	}
	if r.ReasoningAlt != nil {
		return *r.ReasoningAlt
	}
	return ""
}

type openAIToolCall struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// FromOpenAI converts a non-streaming completion. model is what Claude Code
// asked for: echoing it back keeps the experiment blind.
func FromOpenAI(raw []byte, model string) (map[string]any, error) {
	var r openAIResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("upstream returned invalid JSON: %w", err)
	}
	if len(r.Choices) == 0 {
		return nil, fmt.Errorf("upstream returned no choices")
	}
	ch := r.Choices[0]
	content := []map[string]any{}
	if r := ch.Message.Text(); r != "" {
		content = append(content, map[string]any{"type": "thinking", "thinking": r, "signature": ReasoningSignature})
	}
	if ch.Message.Content != nil && *ch.Message.Content != "" {
		content = append(content, map[string]any{"type": "text", "text": *ch.Message.Content})
	}
	for _, tc := range ch.Message.ToolCalls {
		content = append(content, map[string]any{
			"type":  "tool_use",
			"id":    toolID(tc.ID),
			"name":  tc.Function.Name,
			"input": parseArgs(tc.Function.Arguments),
		})
	}
	usage := map[string]any{"input_tokens": 0, "output_tokens": 0}
	if r.Usage != nil {
		usage["input_tokens"] = r.Usage.PromptTokens
		usage["output_tokens"] = r.Usage.CompletionTokens
	}
	return map[string]any{
		"id":            NewID("msg_"),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   StopReason(ch.FinishReason, len(ch.Message.ToolCalls) > 0),
		"stop_sequence": nil,
		"usage":         usage,
	}, nil
}

// StopReason maps finish_reason. Some providers say "stop" even when they
// emitted tool calls; Claude Code only runs tools on tool_use, so the calls win.
func StopReason(finish string, hadTools bool) string {
	if hadTools {
		return "tool_use"
	}
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	default:
		return "end_turn"
	}
}

// parseArgs never fails: a model that emits broken JSON still gets its call
// through, and the tool reports the bad input the way it would for Claude.
func parseArgs(s string) any {
	s = strings.TrimSpace(s)
	if s == "" {
		return map[string]any{}
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return map[string]any{"_raw_arguments": s}
	}
	if _, ok := v.(map[string]any); !ok {
		return map[string]any{"_raw_arguments": s}
	}
	return v
}

func toolID(id string) string {
	if id == "" {
		return NewID("toolu_")
	}
	return id
}

func NewID(prefix string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// ErrorBody builds an Anthropic-shaped error so Claude Code shows it (and
// retries 429/5xx) like it would with the real API.
func ErrorBody(status int, msg string) map[string]any {
	return map[string]any{
		"type":  "error",
		"error": map[string]any{"type": ErrorType(status), "message": msg},
	}
}

func ErrorType(status int) string {
	switch status {
	case 400:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 413:
		return "request_too_large"
	case 429:
		return "rate_limit_error"
	case 503, 529:
		return "overloaded_error"
	default:
		return "api_error"
	}
}
