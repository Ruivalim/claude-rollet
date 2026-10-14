package translate

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

type openAIChunk struct {
	Choices []struct {
		Delta struct {
			Content *string `json:"content"`
			Reasoning
			ToolCalls []openAIToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *openAIUsage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// ErrTruncated means the upstream stream ended without a finish_reason or
// [DONE]: the answer is incomplete, and saying end_turn would be a lie.
var ErrTruncated = errors.New("upstream stream ended before finishing")

// streamer turns OpenAI chunks into Anthropic SSE events. Anthropic content is
// a sequence of blocks, each opened, filled with deltas and closed, so it
// tracks which block is open and starts a new one whenever the kind changes.
type streamer struct {
	w     io.Writer
	flush func()
	model string

	started  bool
	index    int    // index of the open block, -1 when none
	kind     string // "thinking", "text" or "tool_use"
	toolIdx  int    // OpenAI tool_calls index of the open tool block
	toolID   string
	hadTools bool
	finish   string
	usage    openAIUsage
	// sig, when set, is the signature for the open thinking block; otherwise
	// ReasoningSignature is used.
	sig string
}

// Stream reads an OpenAI SSE body and writes Anthropic SSE. On a mid-stream
// failure it still sends an error event, so Claude Code reports it instead of
// waiting forever.
func Stream(upstream io.Reader, w io.Writer, flush func(), model string) error {
	s := &streamer{w: w, flush: flush, model: model, index: -1}
	err := s.run(upstream)
	if err != nil {
		s.start()
		s.event("error", ErrorBody(502, err.Error()))
		return err
	}
	return nil
}

func (s *streamer) run(upstream io.Reader) error {
	br := bufio.NewReaderSize(upstream, 64*1024)
	done := false
	for !done {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return fmt.Errorf("reading upstream: %w", err)
		}
		eof := err == io.EOF
		line = strings.TrimRight(line, "\r\n")

		// Blank lines separate events; ":" lines are keepalives (OpenRouter
		// sends ": OPENROUTER PROCESSING"); event:/id: carry nothing we need.
		if data, ok := strings.CutPrefix(line, "data:"); ok {
			data = strings.TrimSpace(data)
			if data == "[DONE]" {
				done = true
			} else if data != "" {
				if err := s.chunk(data); err != nil {
					return err
				}
			}
		}
		if eof {
			break
		}
	}
	if !done && s.finish == "" {
		return ErrTruncated
	}
	s.finishMessage()
	return nil
}

func (s *streamer) chunk(data string) error {
	var c openAIChunk
	if err := json.Unmarshal([]byte(data), &c); err != nil {
		return fmt.Errorf("upstream sent invalid JSON chunk: %w", err)
	}
	if c.Error != nil {
		return fmt.Errorf("upstream error: %s", c.Error.Message)
	}
	s.start()
	if c.Usage != nil {
		s.usage = *c.Usage
	}
	for _, ch := range c.Choices {
		if r := ch.Delta.Text(); r != "" {
			if s.kind != "thinking" {
				s.open("thinking", map[string]any{"type": "thinking", "thinking": "", "signature": ""})
			}
			s.event("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": s.index,
				"delta": map[string]any{"type": "thinking_delta", "thinking": r},
			})
		}
		if t := ch.Delta.Content; t != nil && *t != "" {
			if s.kind != "text" {
				s.open("text", map[string]any{"type": "text", "text": ""})
			}
			s.event("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": s.index,
				"delta": map[string]any{"type": "text_delta", "text": *t},
			})
		}
		for i, tc := range ch.Delta.ToolCalls {
			idx := i
			if tc.Index != nil {
				idx = *tc.Index
			}
			// A new call starts when the index moves or a fresh id shows up.
			// Some providers (Gemini's compat layer) send each call whole,
			// with no index, so the id check is what separates them.
			isNew := s.kind != "tool_use" || idx != s.toolIdx || (tc.ID != "" && tc.ID != s.toolID)
			if isNew {
				s.toolIdx, s.toolID = idx, tc.ID
				s.hadTools = true
				s.open("tool_use", map[string]any{
					"type": "tool_use", "id": toolID(tc.ID), "name": tc.Function.Name, "input": map[string]any{},
				})
			}
			if tc.Function.Arguments != "" {
				s.event("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": s.index,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": tc.Function.Arguments},
				})
			}
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			s.finish = *ch.FinishReason
		}
	}
	return nil
}

func (s *streamer) start() {
	if s.started {
		return
	}
	s.started = true
	s.event("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": NewID("msg_"), "type": "message", "role": "assistant", "model": s.model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

func (s *streamer) open(kind string, block map[string]any) {
	s.closeBlock()
	s.index++
	s.kind = kind
	s.event("content_block_start", map[string]any{"type": "content_block_start", "index": s.index, "content_block": block})
}

func (s *streamer) closeBlock() {
	if s.kind == "" {
		return
	}
	if s.kind == "thinking" {
		sig := s.sig
		if sig == "" {
			sig = ReasoningSignature
		}
		s.event("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": s.index,
			"delta": map[string]any{"type": "signature_delta", "signature": sig},
		})
		s.sig = ""
	}
	s.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": s.index})
	s.kind = ""
}

func (s *streamer) finishMessage() {
	s.start()
	s.closeBlock()
	s.event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": StopReason(s.finish, s.hadTools), "stop_sequence": nil},
		"usage": map[string]any{"input_tokens": s.usage.PromptTokens, "output_tokens": s.usage.CompletionTokens},
	})
	s.event("message_stop", map[string]any{"type": "message_stop"})
}

func (s *streamer) event(name string, payload map[string]any) {
	raw, _ := json.Marshal(payload)
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, raw)
	if s.flush != nil {
		s.flush()
	}
}
