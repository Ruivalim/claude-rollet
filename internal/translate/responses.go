package translate

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// CodexSignaturePrefix marks thinking blocks that carry a Responses API
// reasoning item (with its encrypted_content). Claude Code stores the block
// and sends it back untouched, which is how the reasoning survives between
// turns with store=false.
const CodexSignaturePrefix = "rollet-codex:"

// ResponsesOptions shape the /responses body.
type ResponsesOptions struct {
	Model     string
	SessionID string // prompt_cache_key: keeps the prefix cache warm per session
	Effort    string // default reasoning effort when Claude Code doesn't send one
}

// ToResponses builds a Responses API body (as the ChatGPT Codex backend wants
// it: store=false, stream=true, system prompt in instructions).
func ToResponses(req *AnthropicRequest, opt ResponsesOptions) (map[string]any, error) {
	sys, err := Blocks(req.System)
	if err != nil {
		return nil, fmt.Errorf("system: %w", err)
	}
	instructions := joinText(sys)
	if instructions == "" {
		instructions = "You are a helpful assistant."
	}

	var input []map[string]any
	for i, m := range req.Messages {
		blocks, err := Blocks(m.Content)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		switch m.Role {
		case "user":
			input = append(input, responsesUser(blocks)...)
		case "assistant":
			input = append(input, responsesAssistant(blocks)...)
		case "system":
			if t := joinText(blocks); t != "" {
				input = append(input, map[string]any{
					"role":    "developer",
					"content": []map[string]any{{"type": "input_text", "text": t}},
				})
			}
		default:
			return nil, fmt.Errorf("messages[%d]: unknown role %q", i, m.Role)
		}
	}

	effort := opt.Effort
	if req.OutputConfig != nil && req.OutputConfig.Effort != "" {
		effort = req.OutputConfig.Effort
	}
	if effort == "" {
		effort = "medium"
	}

	body := map[string]any{
		"model":               opt.Model,
		"instructions":        instructions,
		"input":               input,
		"store":               false,
		"stream":              true,
		"include":             []string{"reasoning.encrypted_content"},
		"reasoning":           map[string]any{"effort": effort, "summary": "auto"},
		"parallel_tool_calls": true,
		"tool_choice":         "auto",
	}
	if opt.SessionID != "" {
		body["prompt_cache_key"] = opt.SessionID
	}

	var tools []map[string]any
	for _, t := range req.Tools {
		if len(t.InputSchema) == 0 {
			continue
		}
		tools = append(tools, map[string]any{
			"type": "function", "name": t.Name, "description": t.Description,
			"parameters": t.InputSchema, "strict": false,
		})
	}
	if len(tools) > 0 {
		body["tools"] = tools
		if tc := req.ToolChoice; tc != nil {
			switch tc.Type {
			case "any":
				body["tool_choice"] = "required"
			case "none":
				body["tool_choice"] = "none"
			case "tool":
				body["tool_choice"] = map[string]any{"type": "function", "name": tc.Name}
			}
			if tc.DisableParallelToolUse {
				body["parallel_tool_calls"] = false
			}
		}
	}
	return body, nil
}

func responsesUser(blocks []Block) []map[string]any {
	var out, parts []map[string]any
	for _, b := range blocks {
		switch b.Type {
		case "tool_result":
			inner, _ := Blocks(b.Content)
			text := joinText(inner)
			if b.IsError {
				text = "Error: " + text
			}
			out = append(out, map[string]any{"type": "function_call_output", "call_id": b.ToolUseID, "output": text})
			for _, ib := range inner {
				if p := responsesImage(ib); p != nil {
					parts = append(parts, p)
				}
			}
		case "text":
			if b.Text != "" {
				parts = append(parts, map[string]any{"type": "input_text", "text": b.Text})
			}
		case "image":
			if p := responsesImage(b); p != nil {
				parts = append(parts, p)
			}
		case "document":
			if b.Source != nil && b.Source.Type == "text" {
				parts = append(parts, map[string]any{"type": "input_text", "text": b.Source.Data})
			}
		}
	}
	if len(parts) > 0 {
		out = append(out, map[string]any{"role": "user", "content": parts})
	}
	return out
}

func responsesImage(b Block) map[string]any {
	if p := imagePart(b); p != nil {
		return map[string]any{"type": "input_image", "image_url": p["image_url"].(map[string]any)["url"]}
	}
	return nil
}

func responsesAssistant(blocks []Block) []map[string]any {
	var out []map[string]any
	for _, b := range blocks {
		switch b.Type {
		case "thinking":
			raw, ok := strings.CutPrefix(b.Signature, CodexSignaturePrefix)
			if !ok {
				continue // Claude's or another provider's thinking stays out
			}
			var item map[string]any
			if json.Unmarshal([]byte(raw), &item) == nil && item["type"] == "reasoning" {
				out = append(out, item)
			}
		case "text":
			if b.Text != "" {
				out = append(out, map[string]any{
					"type": "message", "role": "assistant", "status": "completed",
					"content": []map[string]any{{"type": "output_text", "text": b.Text, "annotations": []any{}}},
				})
			}
		case "tool_use":
			args := string(b.Input)
			if strings.TrimSpace(args) == "" || args == "null" {
				args = "{}"
			}
			out = append(out, map[string]any{"type": "function_call", "call_id": b.ID, "name": b.Name, "arguments": args})
		}
	}
	return out
}

// --- stream: Responses SSE -> Anthropic SSE ---

type responsesEvent struct {
	Type        string `json:"type"`
	OutputIndex int    `json:"output_index"`
	Delta       string `json:"delta"`
	Item        *struct {
		Type      string `json:"type"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"item"`
	Response *struct {
		Status            string `json:"status"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	} `json:"response"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// StreamResponses converts a Responses API event stream into Anthropic SSE.
// Each output item becomes one content block: reasoning -> thinking (with the
// raw item stored in the signature), message -> text, function_call ->
// tool_use.
func StreamResponses(upstream io.Reader, w io.Writer, flush func(), model string) error {
	s := &streamer{w: w, flush: flush, model: model, index: -1}
	err := s.runResponses(upstream)
	if err != nil {
		s.start()
		s.closeBlock()
		s.event("error", ErrorBody(502, err.Error()))
	}
	return err
}

func (s *streamer) runResponses(upstream io.Reader) error {
	br := bufio.NewReaderSize(upstream, 64*1024)
	openItem := -1    // output_index of the open block
	argsSent := false // function_call arguments already streamed as deltas
	terminal := false
	for !terminal {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return fmt.Errorf("reading upstream: %w", err)
		}
		eof := err == io.EOF
		data, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "data:")
		data = strings.TrimSpace(data)
		if ok && data != "" && data != "[DONE]" {
			var ev responsesEvent
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				return fmt.Errorf("upstream sent invalid JSON event: %w", err)
			}
			s.start()
			switch ev.Type {
			case "response.output_item.added":
				if ev.Item == nil {
					break
				}
				openItem = ev.OutputIndex
				switch ev.Item.Type {
				case "reasoning":
					s.open("thinking", map[string]any{"type": "thinking", "thinking": "", "signature": ""})
				case "message":
					s.open("text", map[string]any{"type": "text", "text": ""})
				case "function_call":
					s.hadTools = true
					argsSent = false
					s.open("tool_use", map[string]any{"type": "tool_use", "id": toolID(ev.Item.CallID), "name": ev.Item.Name, "input": map[string]any{}})
				default:
					s.closeBlock() // web_search_call and friends: nothing to show
					openItem = -1
				}
			case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
				if s.kind == "thinking" && ev.OutputIndex == openItem {
					s.delta(map[string]any{"type": "thinking_delta", "thinking": ev.Delta})
				}
			case "response.reasoning_summary_part.done":
				if s.kind == "thinking" && ev.OutputIndex == openItem {
					s.delta(map[string]any{"type": "thinking_delta", "thinking": "\n\n"})
				}
			case "response.output_text.delta", "response.refusal.delta":
				if s.kind == "text" && ev.OutputIndex == openItem {
					s.delta(map[string]any{"type": "text_delta", "text": ev.Delta})
				}
			case "response.function_call_arguments.delta":
				if s.kind == "tool_use" && ev.OutputIndex == openItem && ev.Delta != "" {
					argsSent = true
					s.delta(map[string]any{"type": "input_json_delta", "partial_json": ev.Delta})
				}
			case "response.output_item.done":
				if ev.OutputIndex != openItem {
					break
				}
				if s.kind == "thinking" {
					var raw struct {
						Item json.RawMessage `json:"item"`
					}
					_ = json.Unmarshal([]byte(data), &raw)
					s.sig = CodexSignaturePrefix + string(raw.Item)
				}
				if s.kind == "tool_use" && !argsSent && ev.Item != nil && ev.Item.Arguments != "" {
					s.delta(map[string]any{"type": "input_json_delta", "partial_json": ev.Item.Arguments})
				}
				s.closeBlock()
				openItem = -1
			case "response.completed", "response.incomplete":
				terminal = true
				if r := ev.Response; r != nil {
					if r.Usage != nil {
						s.usage = openAIUsage{PromptTokens: r.Usage.InputTokens, CompletionTokens: r.Usage.OutputTokens}
					}
					if r.Status == "incomplete" {
						if r.IncompleteDetails != nil && r.IncompleteDetails.Reason == "max_output_tokens" {
							s.finish = "length"
						} else {
							reason := "unknown"
							if r.IncompleteDetails != nil {
								reason = r.IncompleteDetails.Reason
							}
							return fmt.Errorf("response incomplete: %s", reason)
						}
					}
				}
				if s.finish == "" {
					s.finish = "stop"
				}
			case "response.failed":
				msg := "response failed"
				if ev.Response != nil && ev.Response.Error != nil {
					msg = ev.Response.Error.Code + ": " + ev.Response.Error.Message
				}
				return fmt.Errorf("upstream error: %s", msg)
			case "error":
				return fmt.Errorf("upstream error: %s %s", ev.Code, ev.Message)
			}
		}
		if eof {
			break
		}
	}
	if !terminal {
		return ErrTruncated
	}
	s.finishMessage()
	return nil
}

func (s *streamer) delta(d map[string]any) {
	s.event("content_block_delta", map[string]any{"type": "content_block_delta", "index": s.index, "delta": d})
}

// Collect rebuilds a complete Anthropic message from Anthropic SSE, for
// clients that asked for a non-streaming answer from an upstream that only
// streams.
func Collect(sse []byte) (map[string]any, error) {
	msg := map[string]any{"type": "message", "role": "assistant", "content": []map[string]any{}}
	blocks := []map[string]any{}
	partial := map[int]*strings.Builder{}
	sc := bufio.NewScanner(strings.NewReader(string(sse)))
	sc.Buffer(make([]byte, 64*1024), 256*1024*1024)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return nil, err
		}
		idx, _ := ev["index"].(float64)
		i := int(idx)
		switch ev["type"] {
		case "message_start":
			for k, v := range ev["message"].(map[string]any) {
				if k != "content" {
					msg[k] = v
				}
			}
		case "content_block_start":
			blocks = append(blocks, ev["content_block"].(map[string]any))
		case "content_block_delta":
			if i >= len(blocks) {
				return nil, fmt.Errorf("delta for unknown block %d", i)
			}
			b, d := blocks[i], ev["delta"].(map[string]any)
			switch d["type"] {
			case "text_delta":
				b["text"] = b["text"].(string) + d["text"].(string)
			case "thinking_delta":
				b["thinking"] = b["thinking"].(string) + d["thinking"].(string)
			case "signature_delta":
				b["signature"] = d["signature"]
			case "input_json_delta":
				if partial[i] == nil {
					partial[i] = &strings.Builder{}
				}
				partial[i].WriteString(d["partial_json"].(string))
			}
		case "message_delta":
			d := ev["delta"].(map[string]any)
			msg["stop_reason"], msg["stop_sequence"] = d["stop_reason"], d["stop_sequence"]
			msg["usage"] = ev["usage"]
		case "error":
			e, _ := ev["error"].(map[string]any)
			return nil, fmt.Errorf("%v", e["message"])
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	for i, p := range partial {
		blocks[i]["input"] = parseArgs(p.String())
	}
	msg["content"] = blocks
	return msg, nil
}
