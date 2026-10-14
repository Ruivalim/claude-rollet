package translate

import (
	"bufio"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func mustReq(t *testing.T, raw string) *AnthropicRequest {
	t.Helper()
	var r AnthropicRequest
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatal(err)
	}
	return &r
}

func roles(msgs []map[string]any) string {
	var rs []string
	for _, m := range msgs {
		rs = append(rs, m["role"].(string))
	}
	return strings.Join(rs, ",")
}

func TestToOpenAIConversation(t *testing.T) {
	req := mustReq(t, `{
	  "model": "claude-opus-5-5",
	  "system": [{"type":"text","text":"You are Claude Code."},{"type":"text","text":"Be brief.","cache_control":{"type":"ephemeral"}}],
	  "messages": [
	    {"role":"user","content":[{"type":"text","text":"<system-reminder>x</system-reminder>"},{"type":"text","text":"list files"}]},
	    {"role":"system","content":[{"type":"text","text":"Primary working directory: /tmp/p"}]},
	    {"role":"assistant","content":[
	      {"type":"thinking","thinking":"secret","signature":"sig"},
	      {"type":"text","text":"Sure."},
	      {"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls"}},
	      {"type":"tool_use","id":"toolu_2","name":"Read","input":{}}
	    ]},
	    {"role":"user","content":[
	      {"type":"text","text":"also this"},
	      {"type":"tool_result","tool_use_id":"toolu_1","content":"a.go\nb.go"},
	      {"type":"tool_result","tool_use_id":"toolu_2","is_error":true,"content":[{"type":"text","text":"no such file"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAA"}}]}
	    ]},
	    {"role":"assistant","content":"plain string answer"}
	  ],
	  "tools":[
	    {"name":"Bash","description":"run","input_schema":{"type":"object"}},
	    {"type":"web_search_20250305","name":"web_search"}
	  ],
	  "tool_choice":{"type":"any","disable_parallel_tool_use":true},
	  "max_tokens": 128000,
	  "stop_sequences":["STOP"],
	  "thinking":{"type":"adaptive"},
	  "stream": true
	}`)
	body, err := ToOpenAI(req, Options{Model: "deepseek-chat", MaxTokens: 8192, MaxTokensParam: "max_completion_tokens"})
	if err != nil {
		t.Fatal(err)
	}
	msgs := body["messages"].([]map[string]any)

	// tool messages must come right after the assistant that called them,
	// before the user's own text, or OpenAI-compatible APIs reject the request.
	if got, want := roles(msgs), "system,user,system,assistant,tool,tool,user,assistant"; got != want {
		t.Fatalf("roles = %s, want %s", got, want)
	}
	if msgs[0]["content"] != "You are Claude Code.\n\nBe brief." {
		t.Errorf("system = %q", msgs[0]["content"])
	}
	asst := msgs[3]
	if asst["content"] != "Sure." {
		t.Errorf("thinking leaked or text lost: %q", asst["content"])
	}
	calls := asst["tool_calls"].([]map[string]any)
	if len(calls) != 2 || calls[0]["id"] != "toolu_1" {
		t.Fatalf("tool_calls = %v", calls)
	}
	if args := calls[0]["function"].(map[string]any)["arguments"]; args != `{"command":"ls"}` {
		t.Errorf("arguments = %v", args)
	}
	if msgs[4]["tool_call_id"] != "toolu_1" || msgs[4]["content"] != "a.go\nb.go" {
		t.Errorf("tool msg 1 = %v", msgs[4])
	}
	if msgs[5]["content"] != "Error: no such file" {
		t.Errorf("error tool result = %v", msgs[5]["content"])
	}
	// the image from the tool result has nowhere to go in a tool message,
	// so it travels with the next user message
	parts, ok := msgs[6]["content"].([]map[string]any)
	if !ok || len(parts) != 2 || parts[1]["type"] != "image_url" {
		t.Fatalf("user parts = %#v", msgs[6]["content"])
	}
	if url := parts[1]["image_url"].(map[string]any)["url"]; url != "data:image/png;base64,AAA" {
		t.Errorf("image url = %v", url)
	}
	if msgs[7]["content"] != "plain string answer" {
		t.Errorf("string content = %v", msgs[7]["content"])
	}

	tools := body["tools"].([]map[string]any)
	if len(tools) != 1 {
		t.Errorf("server tool should be skipped, got %d tools", len(tools))
	}
	if body["tool_choice"] != "required" || body["parallel_tool_calls"] != false {
		t.Errorf("tool_choice = %v parallel = %v", body["tool_choice"], body["parallel_tool_calls"])
	}
	if body["max_completion_tokens"] != 8192 || body["max_tokens"] != nil {
		t.Errorf("max tokens not capped/renamed: %v", body)
	}
	if body["stream"] != true || body["stream_options"] == nil {
		t.Errorf("stream flags missing")
	}
	if body["thinking"] != nil {
		t.Errorf("Claude-only thinking param leaked upstream")
	}
	if _, err := json.Marshal(body); err != nil {
		t.Fatal(err)
	}
}

func TestToOpenAIEdges(t *testing.T) {
	t.Run("max tokens below cap stays", func(t *testing.T) {
		b, _ := ToOpenAI(mustReq(t, `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`), Options{Model: "x", MaxTokens: 8192})
		if b["max_tokens"] != 100 {
			t.Errorf("got %v", b["max_tokens"])
		}
	})
	t.Run("assistant with only thinking is dropped", func(t *testing.T) {
		b, _ := ToOpenAI(mustReq(t, `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"thinking","thinking":"x"}]}]}`), Options{Model: "x"})
		if got := roles(b["messages"].([]map[string]any)); got != "user" {
			t.Errorf("roles = %s", got)
		}
	})
	t.Run("tool_use with null input becomes {}", func(t *testing.T) {
		b, _ := ToOpenAI(mustReq(t, `{"model":"m","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"X","input":null}]}]}`), Options{Model: "x"})
		m := b["messages"].([]map[string]any)[0]
		if m["content"] != nil {
			t.Errorf("content should be null with only tool calls")
		}
		if args := m["tool_calls"].([]map[string]any)[0]["function"].(map[string]any)["arguments"]; args != "{}" {
			t.Errorf("args = %v", args)
		}
	})
	t.Run("unknown role is an error", func(t *testing.T) {
		if _, err := ToOpenAI(mustReq(t, `{"model":"m","messages":[{"role":"robot","content":"x"}]}`), Options{Model: "x"}); err == nil {
			t.Error("expected error")
		}
	})
	t.Run("bad content shape is an error", func(t *testing.T) {
		if _, err := ToOpenAI(mustReq(t, `{"model":"m","messages":[{"role":"user","content":42}]}`), Options{Model: "x"}); err == nil {
			t.Error("expected error")
		}
	})
	t.Run("named tool choice", func(t *testing.T) {
		b, _ := ToOpenAI(mustReq(t, `{"model":"m","messages":[],"tools":[{"name":"T","input_schema":{}}],"tool_choice":{"type":"tool","name":"T"}}`), Options{Model: "x"})
		tc := b["tool_choice"].(map[string]any)
		if tc["function"].(map[string]any)["name"] != "T" {
			t.Errorf("tool_choice = %v", tc)
		}
	})
	t.Run("tool choice without tools is omitted", func(t *testing.T) {
		b, _ := ToOpenAI(mustReq(t, `{"model":"m","messages":[],"tool_choice":{"type":"any"}}`), Options{Model: "x"})
		if _, ok := b["tool_choice"]; ok {
			t.Error("tool_choice without tools is rejected by most providers")
		}
	})
}

func TestFromOpenAI(t *testing.T) {
	raw := `{"choices":[{"message":{"content":"done","tool_calls":[
	  {"id":"call_1","function":{"name":"Bash","arguments":"{\"command\":\"ls\"}"}},
	  {"id":"","function":{"name":"Read","arguments":"{not json"}}
	]},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`
	out, err := FromOpenAI([]byte(raw), "claude-opus-5-5")
	if err != nil {
		t.Fatal(err)
	}
	if out["model"] != "claude-opus-5-5" {
		t.Errorf("model must echo what Claude Code asked for, got %v", out["model"])
	}
	if out["stop_reason"] != "tool_use" {
		t.Errorf("finish=stop with tool calls must still be tool_use, got %v", out["stop_reason"])
	}
	content := out["content"].([]map[string]any)
	if len(content) != 3 {
		t.Fatalf("content = %v", content)
	}
	if content[1]["input"].(map[string]any)["command"] != "ls" {
		t.Errorf("args not parsed: %v", content[1]["input"])
	}
	if id := content[2]["id"].(string); !strings.HasPrefix(id, "toolu_") {
		t.Errorf("missing id should be generated, got %q", id)
	}
	if content[2]["input"].(map[string]any)["_raw_arguments"] != "{not json" {
		t.Errorf("broken args should survive raw: %v", content[2]["input"])
	}
	if u := out["usage"].(map[string]any); u["input_tokens"] != 10 || u["output_tokens"] != 5 {
		t.Errorf("usage = %v", u)
	}

	if _, err := FromOpenAI([]byte(`{"choices":[]}`), "m"); err == nil {
		t.Error("no choices should fail")
	}
	if _, err := FromOpenAI([]byte(`<html>`), "m"); err == nil {
		t.Error("non-JSON should fail")
	}
	out, _ = FromOpenAI([]byte(`{"choices":[{"message":{"content":null},"finish_reason":"length"}]}`), "m")
	if out["stop_reason"] != "max_tokens" {
		t.Errorf("length -> %v", out["stop_reason"])
	}
}

// --- streaming ---

type sseEvent struct {
	Name string
	Data map[string]any
}

func parseSSE(t *testing.T, s string) []sseEvent {
	t.Helper()
	var evs []sseEvent
	sc := bufio.NewScanner(strings.NewReader(s))
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	var name string
	for sc.Scan() {
		line := sc.Text()
		if n, ok := strings.CutPrefix(line, "event: "); ok {
			name = n
		} else if d, ok := strings.CutPrefix(line, "data: "); ok {
			var m map[string]any
			if err := json.Unmarshal([]byte(d), &m); err != nil {
				t.Fatalf("bad data line %q: %v", d, err)
			}
			if m["type"] != name {
				t.Fatalf("event %q carries type %v", name, m["type"])
			}
			evs = append(evs, sseEvent{name, m})
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("reading SSE: %v", err)
	}
	return evs
}

type assembled struct {
	Blocks     []map[string]any
	StopReason string
	Usage      map[string]any
	Err        string
}

// assemble replays events the way the Anthropic SDK does and fails on any
// sequencing the SDK would choke on.
func assemble(t *testing.T, evs []sseEvent) assembled {
	t.Helper()
	var a assembled
	open := -1
	partial := map[int]string{}
	if len(evs) == 0 || evs[0].Name != "message_start" {
		t.Fatalf("first event must be message_start, got %v", evs)
	}
	for _, e := range evs[1:] {
		switch e.Name {
		case "content_block_start":
			idx := int(e.Data["index"].(float64))
			if open != -1 {
				t.Fatalf("block %d started while %d open", idx, open)
			}
			if idx != len(a.Blocks) {
				t.Fatalf("block index %d, expected %d", idx, len(a.Blocks))
			}
			open = idx
			a.Blocks = append(a.Blocks, e.Data["content_block"].(map[string]any))
		case "content_block_delta":
			idx := int(e.Data["index"].(float64))
			if idx != open {
				t.Fatalf("delta for %d while %d open", idx, open)
			}
			d := e.Data["delta"].(map[string]any)
			switch d["type"] {
			case "text_delta":
				a.Blocks[idx]["text"] = a.Blocks[idx]["text"].(string) + d["text"].(string)
			case "thinking_delta":
				a.Blocks[idx]["thinking"] = a.Blocks[idx]["thinking"].(string) + d["thinking"].(string)
			case "signature_delta":
				a.Blocks[idx]["signature"] = d["signature"]
			case "input_json_delta":
				partial[idx] += d["partial_json"].(string)
			default:
				t.Fatalf("unknown delta %v", d["type"])
			}
		case "content_block_stop":
			idx := int(e.Data["index"].(float64))
			if idx != open {
				t.Fatalf("stop for %d while %d open", idx, open)
			}
			if a.Blocks[idx]["type"] == "tool_use" {
				var in any = map[string]any{}
				if p := partial[idx]; p != "" {
					if err := json.Unmarshal([]byte(p), &in); err != nil {
						t.Fatalf("tool input %q is not JSON: %v", p, err)
					}
				}
				a.Blocks[idx]["input"] = in
			}
			open = -1
		case "message_delta":
			if open != -1 {
				t.Fatalf("message_delta with block %d open", open)
			}
			a.StopReason = e.Data["delta"].(map[string]any)["stop_reason"].(string)
			a.Usage = e.Data["usage"].(map[string]any)
		case "message_stop":
		case "error":
			a.Err = e.Data["error"].(map[string]any)["message"].(string)
		default:
			t.Fatalf("unknown event %q", e.Name)
		}
	}
	return a
}

func runStream(t *testing.T, upstream string) (assembled, error) {
	t.Helper()
	var out strings.Builder
	flushes := 0
	err := Stream(strings.NewReader(upstream), &out, func() { flushes++ }, "claude-opus-5-5")
	if flushes == 0 {
		t.Error("never flushed: Claude Code would see nothing until the end")
	}
	evs := parseSSE(t, out.String())
	if evs[0].Data["message"].(map[string]any)["model"] != "claude-opus-5-5" {
		t.Error("message_start must echo the requested model")
	}
	return assemble(t, evs), err
}

func sse(lines ...string) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString("data: ")
		b.WriteString(l)
		b.WriteString("\n\n")
	}
	return b.String()
}

func TestStreamTextThenSplitToolCalls(t *testing.T) {
	up := ": OPENROUTER PROCESSING\n\n" + sse(
		`{"choices":[{"delta":{"role":"assistant","content":""}}]}`,
		`{"choices":[{"delta":{"content":"Let me "}}]}`,
		`{"choices":[{"delta":{"content":"look."}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"Bash","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"comm"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"and\":\"ls\"}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","function":{"name":"Read","arguments":"{\"file_path\":\"/x\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20}}`,
		`[DONE]`,
	)
	a, err := runStream(t, up)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Blocks) != 3 {
		t.Fatalf("blocks = %v", a.Blocks)
	}
	if a.Blocks[0]["text"] != "Let me look." {
		t.Errorf("text = %q", a.Blocks[0]["text"])
	}
	if a.Blocks[1]["id"] != "call_a" || a.Blocks[1]["input"].(map[string]any)["command"] != "ls" {
		t.Errorf("tool 1 = %v", a.Blocks[1])
	}
	if a.Blocks[2]["name"] != "Read" {
		t.Errorf("tool 2 = %v", a.Blocks[2])
	}
	if a.StopReason != "tool_use" {
		t.Errorf("stop = %s", a.StopReason)
	}
	// usage arrives in a trailing chunk after finish_reason
	if a.Usage["input_tokens"] != float64(100) || a.Usage["output_tokens"] != float64(20) {
		t.Errorf("usage = %v", a.Usage)
	}
}

func TestStreamWholeCallsWithoutIndex(t *testing.T) {
	// Gemini-style: each call arrives complete, no index, same position 0.
	up := sse(
		`{"choices":[{"delta":{"tool_calls":[{"id":"c1","function":{"name":"A","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"id":"c2","function":{"name":"B","arguments":"{\"x\":1}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`[DONE]`,
	)
	a, err := runStream(t, up)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Blocks) != 2 || a.Blocks[0]["name"] != "A" || a.Blocks[1]["name"] != "B" {
		t.Fatalf("blocks = %v", a.Blocks)
	}
	if a.StopReason != "tool_use" {
		t.Errorf("finish=stop with tools must be tool_use, got %s", a.StopReason)
	}
}

func TestStreamCRLFAndNoDoneButFinished(t *testing.T) {
	up := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\r\n\r\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\r\n\r\n"
	a, err := runStream(t, up)
	if err != nil {
		t.Fatal(err)
	}
	if a.Blocks[0]["text"] != "hi" || a.StopReason != "max_tokens" {
		t.Errorf("got %+v", a)
	}
}

func TestStreamTruncated(t *testing.T) {
	up := sse(`{"choices":[{"delta":{"content":"half an ans"}}]}`)
	a, err := runStream(t, up)
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v", err)
	}
	if a.Err == "" {
		t.Error("client must receive an error event, not a clean end_turn")
	}
	if a.StopReason != "" {
		t.Error("truncated stream must not claim a stop_reason")
	}
}

func TestStreamErrorChunk(t *testing.T) {
	up := sse(`{"error":{"message":"rate limited"}}`)
	a, err := runStream(t, up)
	if err == nil || !strings.Contains(a.Err, "rate limited") {
		t.Fatalf("err=%v event=%q", err, a.Err)
	}
}

func TestStreamGarbageChunk(t *testing.T) {
	_, err := runStream(t, sse(`{"choices":[{"delta":{"content":"ok"}}]}`, `{nope`))
	if err == nil {
		t.Fatal("invalid JSON chunk must fail loudly")
	}
}

func TestStreamEmptyAnswer(t *testing.T) {
	a, err := runStream(t, sse(`[DONE]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Blocks) != 0 || a.StopReason != "end_turn" {
		t.Errorf("got %+v", a)
	}
}

func TestStreamHugeLine(t *testing.T) {
	// A single chunk bigger than the reader buffer (a model dumping a whole
	// file into one tool call) must not be cut.
	big := strings.Repeat("x", 300*1024)
	chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": big}}}})
	a, err := runStream(t, sse(string(chunk), `[DONE]`))
	if err != nil {
		t.Fatal(err)
	}
	if a.Blocks[0]["text"] != big {
		t.Errorf("text length %d, want %d", len(a.Blocks[0]["text"].(string)), len(big))
	}
}

func TestStreamReasoningBecomesThinking(t *testing.T) {
	up := sse(
		`{"choices":[{"delta":{"role":"assistant","content":"","reasoning_content":""}}]}`,
		`{"choices":[{"delta":{"content":"","reasoning_content":"91 = 7 x "}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"13."}}]}`,
		`{"choices":[{"delta":{"content":"No.","reasoning_content":""}}]}`,
		`{"choices":[{"delta":{"reasoning":"openrouter spelling"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`[DONE]`,
	)
	a, err := runStream(t, up)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Blocks) != 3 {
		t.Fatalf("blocks = %v", a.Blocks)
	}
	if a.Blocks[0]["type"] != "thinking" || a.Blocks[0]["thinking"] != "91 = 7 x 13." || a.Blocks[0]["signature"] != ReasoningSignature {
		t.Errorf("thinking block = %v", a.Blocks[0])
	}
	if a.Blocks[1]["text"] != "No." || a.Blocks[2]["thinking"] != "openrouter spelling" {
		t.Errorf("blocks = %v", a.Blocks)
	}
}

func TestReasoningRoundTrip(t *testing.T) {
	out, err := FromOpenAI([]byte(`{"choices":[{"message":{"content":"ok","reasoning_content":"because"},"finish_reason":"stop"}]}`), "m")
	if err != nil {
		t.Fatal(err)
	}
	blocks := out["content"].([]map[string]any)
	if blocks[0]["type"] != "thinking" || blocks[0]["signature"] != ReasoningSignature {
		t.Fatalf("blocks = %v", blocks)
	}

	// What Claude Code sends back on the next turn.
	req := mustReq(t, `{"model":"m","messages":[
	  {"role":"user","content":"q"},
	  {"role":"assistant","content":[
	    {"type":"thinking","thinking":"because","signature":"rollet-reasoning"},
	    {"type":"thinking","thinking":"real claude thoughts","signature":"EqQBCkYIBxgCKkB..."},
	    {"type":"redacted_thinking","data":"xyz"},
	    {"type":"tool_use","id":"t1","name":"X","input":{}}
	  ]}
	]}`)
	body, err := ToOpenAI(req, Options{Model: "x"})
	if err != nil {
		t.Fatal(err)
	}
	asst := body["messages"].([]map[string]any)[1]
	if asst["reasoning_content"] != "because" {
		t.Errorf("reasoning_content = %v (Claude's own thinking must never be forwarded)", asst["reasoning_content"])
	}

	// No minted thinking: no field at all, providers that don't know it stay happy.
	req = mustReq(t, `{"model":"m","messages":[{"role":"assistant","content":"hi"}]}`)
	body, _ = ToOpenAI(req, Options{Model: "x"})
	if _, has := body["messages"].([]map[string]any)[0]["reasoning_content"]; has {
		t.Error("reasoning_content added out of nowhere")
	}
}
