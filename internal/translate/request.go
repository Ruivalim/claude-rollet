// Package translate converts between the Anthropic Messages API that Claude
// Code speaks and the OpenAI Chat Completions API most providers speak.
package translate

import (
	"encoding/json"
	"fmt"
	"strings"
)

// AnthropicRequest keeps only the fields that have an OpenAI equivalent.
// Claude-only knobs (thinking, context_management, safeguards, output_config,
// cache_control) are dropped on purpose.
type AnthropicRequest struct {
	Model         string          `json:"model"`
	System        json.RawMessage `json:"system,omitempty"`
	Messages      []Message       `json:"messages"`
	Tools         []Tool          `json:"tools,omitempty"`
	ToolChoice    *ToolChoice     `json:"tool_choice,omitempty"`
	MaxTokens     int             `json:"max_tokens,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
}

type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type Block struct {
	Type string `json:"type"`

	Text string `json:"text,omitempty"`

	// thinking
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`

	// image, document
	Source *Source `json:"source,omitempty"`
}

type Source struct {
	Type      string `json:"type"` // base64, url, text
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	Type        string          `json:"type,omitempty"`
}

type ToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use,omitempty"`
}

// Options are the per-provider bits that shape the upstream body.
type Options struct {
	Model          string
	MaxTokens      int // cap; 0 means no cap
	MaxTokensParam string
}

// Blocks parses content that may be a plain string or a list of blocks.
func Blocks(raw json.RawMessage) ([]Block, error) {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		return []Block{{Type: "text", Text: s}}, nil
	}
	var bs []Block
	if err := json.Unmarshal(raw, &bs); err != nil {
		return nil, err
	}
	return bs, nil
}

// ToOpenAI builds the /chat/completions body.
func ToOpenAI(req *AnthropicRequest, opt Options) (map[string]any, error) {
	var msgs []map[string]any

	sys, err := Blocks(req.System)
	if err != nil {
		return nil, fmt.Errorf("system: %w", err)
	}
	if t := joinText(sys); t != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": t})
	}

	for i, m := range req.Messages {
		blocks, err := Blocks(m.Content)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		switch m.Role {
		case "user":
			msgs = append(msgs, userMessages(blocks)...)
		case "assistant":
			if am := assistantMessage(blocks); am != nil {
				msgs = append(msgs, am)
			}
		case "system":
			// Claude Code injects mid-conversation system messages (environment
			// info and so on). OpenAI accepts system at any position.
			if t := joinText(blocks); t != "" {
				msgs = append(msgs, map[string]any{"role": "system", "content": t})
			}
		default:
			return nil, fmt.Errorf("messages[%d]: unknown role %q", i, m.Role)
		}
	}

	body := map[string]any{
		"model":    opt.Model,
		"messages": msgs,
	}

	if tools := openAITools(req.Tools); len(tools) > 0 {
		body["tools"] = tools
		if tc := req.ToolChoice; tc != nil {
			switch tc.Type {
			case "auto":
				body["tool_choice"] = "auto"
			case "any":
				body["tool_choice"] = "required"
			case "none":
				body["tool_choice"] = "none"
			case "tool":
				body["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": tc.Name}}
			}
			if tc.DisableParallelToolUse {
				body["parallel_tool_calls"] = false
			}
		}
	}

	maxTok := req.MaxTokens
	if opt.MaxTokens > 0 && (maxTok == 0 || maxTok > opt.MaxTokens) {
		maxTok = opt.MaxTokens
	}
	if maxTok > 0 {
		param := opt.MaxTokensParam
		if param == "" {
			param = "max_tokens"
		}
		body[param] = maxTok
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	if len(req.StopSequences) > 0 {
		body["stop"] = req.StopSequences
	}
	if req.Stream {
		body["stream"] = true
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	return body, nil
}

// userMessages splits an Anthropic user turn. tool_result blocks become
// role=tool messages, and they must come first: OpenAI requires every tool
// call to be answered right after the assistant message that made it.
func userMessages(blocks []Block) []map[string]any {
	var out []map[string]any
	var parts []map[string]any
	for _, b := range blocks {
		switch b.Type {
		case "tool_result":
			inner, _ := Blocks(b.Content)
			text := joinText(inner)
			if b.IsError {
				text = "Error: " + text
			}
			out = append(out, map[string]any{"role": "tool", "tool_call_id": b.ToolUseID, "content": text})
			// Tool messages are text only; images from a tool (screenshots,
			// Read on a PNG) ride along in the user message that follows.
			for _, ib := range inner {
				if p := imagePart(ib); p != nil {
					parts = append(parts, p)
				}
			}
		case "text":
			if b.Text != "" {
				parts = append(parts, map[string]any{"type": "text", "text": b.Text})
			}
		case "image":
			if p := imagePart(b); p != nil {
				parts = append(parts, p)
			}
		case "document":
			if b.Source != nil && b.Source.Type == "text" {
				parts = append(parts, map[string]any{"type": "text", "text": b.Source.Data})
			}
		}
	}
	if len(parts) == 0 {
		return out
	}
	allText := true
	for _, p := range parts {
		if p["type"] != "text" {
			allText = false
			break
		}
	}
	if allText {
		texts := make([]string, len(parts))
		for i, p := range parts {
			texts[i] = p["text"].(string)
		}
		return append(out, map[string]any{"role": "user", "content": strings.Join(texts, "\n\n")})
	}
	return append(out, map[string]any{"role": "user", "content": parts})
}

func assistantMessage(blocks []Block) map[string]any {
	var texts, reasoning []string
	var calls []map[string]any
	for _, b := range blocks {
		switch b.Type {
		case "thinking":
			// Only a thinking block we minted from the provider's own
			// reasoning goes back. Real Claude thinking is never sent to a
			// third party (and couldn't be: sessions don't change provider).
			if b.Signature == ReasoningSignature && b.Thinking != "" {
				reasoning = append(reasoning, b.Thinking)
			}
		case "text":
			if b.Text != "" {
				texts = append(texts, b.Text)
			}
		case "tool_use":
			args := string(b.Input)
			if strings.TrimSpace(args) == "" || args == "null" {
				args = "{}"
			}
			calls = append(calls, map[string]any{
				"id":       b.ID,
				"type":     "function",
				"function": map[string]any{"name": b.Name, "arguments": args},
			})
		}
	}
	if len(texts) == 0 && len(calls) == 0 {
		return nil
	}
	m := map[string]any{"role": "assistant"}
	if len(reasoning) > 0 {
		// DeepSeek-style thinking models require their reasoning back on
		// assistant turns, tool-calling ones above all.
		m["reasoning_content"] = strings.Join(reasoning, "\n\n")
	}
	if len(texts) > 0 {
		m["content"] = strings.Join(texts, "\n\n")
	} else {
		m["content"] = nil
	}
	if len(calls) > 0 {
		m["tool_calls"] = calls
	}
	return m
}

func imagePart(b Block) map[string]any {
	if b.Type != "image" || b.Source == nil {
		return nil
	}
	var url string
	switch b.Source.Type {
	case "base64":
		url = "data:" + b.Source.MediaType + ";base64," + b.Source.Data
	case "url":
		url = b.Source.URL
	default:
		return nil
	}
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}
}

// openAITools skips server tools (web_search and friends): they have no
// input_schema and only run on Anthropic's side.
func openAITools(tools []Tool) []map[string]any {
	var out []map[string]any
	for _, t := range tools {
		if len(t.InputSchema) == 0 {
			continue
		}
		fn := map[string]any{"name": t.Name, "parameters": t.InputSchema}
		if t.Description != "" {
			fn["description"] = t.Description
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

func joinText(blocks []Block) string {
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}
