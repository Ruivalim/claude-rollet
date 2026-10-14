package translate

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestToResponses(t *testing.T) {
	reasoningItem := `{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"gAAA"}`
	sig, _ := json.Marshal(CodexSignaturePrefix + reasoningItem)
	req := mustReq(t, `{
	  "model":"claude-opus-5-5",
	  "system":[{"type":"text","text":"You are Claude Code."}],
	  "output_config":{"effort":"high"},
	  "messages":[
	    {"role":"user","content":"list files"},
	    {"role":"system","content":"Primary working directory: /p"},
	    {"role":"assistant","content":[
	      {"type":"thinking","thinking":"plan","signature":`+string(sig)+`},
	      {"type":"thinking","thinking":"claude's own","signature":"EqQB..."},
	      {"type":"text","text":"Sure."},
	      {"type":"tool_use","id":"call_1","name":"Bash","input":{"command":"ls"}}
	    ]},
	    {"role":"user","content":[
	      {"type":"tool_result","tool_use_id":"call_1","content":[{"type":"text","text":"a.go"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAA"}}]},
	      {"type":"text","text":"thanks"}
	    ]}
	  ],
	  "tools":[{"name":"Bash","description":"run","input_schema":{"type":"object"}},{"type":"web_search_20250305","name":"web_search"}],
	  "tool_choice":{"type":"tool","name":"Bash"}
	}`)
	body, err := ToResponses(req, ResponsesOptions{Model: "gpt-6-sol", SessionID: "sess-1", Effort: "low"})
	if err != nil {
		t.Fatal(err)
	}
	if body["instructions"] != "You are Claude Code." || body["store"] != false || body["stream"] != true || body["prompt_cache_key"] != "sess-1" {
		t.Errorf("envelope = %v", body)
	}
	if r := body["reasoning"].(map[string]any); r["effort"] != "high" {
		t.Errorf("Claude Code's effort should win over the default: %v", r)
	}
	input := body["input"].([]map[string]any)
	var kinds []string
	for _, it := range input {
		k, _ := it["type"].(string)
		if k == "" {
			k = it["role"].(string)
		}
		kinds = append(kinds, k)
	}
	if got := strings.Join(kinds, ","); got != "user,developer,reasoning,message,function_call,function_call_output,user" {
		t.Fatalf("input kinds = %s", got)
	}
	if input[2]["encrypted_content"] != "gAAA" {
		t.Errorf("reasoning item not restored: %v", input[2])
	}
	if input[4]["call_id"] != "call_1" || input[4]["arguments"] != `{"command":"ls"}` {
		t.Errorf("function_call = %v", input[4])
	}
	if input[5]["call_id"] != "call_1" || input[5]["output"] != "a.go" {
		t.Errorf("function_call_output = %v", input[5])
	}
	parts := input[6]["content"].([]map[string]any)
	if parts[0]["type"] != "input_image" || parts[1]["text"] != "thanks" {
		t.Errorf("tool image should ride in the next user message: %v", parts)
	}
	if tools := body["tools"].([]map[string]any); len(tools) != 1 || tools[0]["name"] != "Bash" {
		t.Errorf("tools = %v", tools)
	}
	if tc := body["tool_choice"].(map[string]any); tc["name"] != "Bash" {
		t.Errorf("tool_choice = %v", tc)
	}
	if _, err := json.Marshal(body); err != nil {
		t.Fatal(err)
	}
}

func TestToResponsesDefaults(t *testing.T) {
	body, _ := ToResponses(mustReq(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`), ResponsesOptions{Model: "x"})
	if body["instructions"] == "" {
		t.Error("the backend rejects empty instructions")
	}
	if body["reasoning"].(map[string]any)["effort"] != "medium" {
		t.Error("default effort")
	}
	if _, has := body["prompt_cache_key"]; has {
		t.Error("no session, no cache key")
	}
	// a thinking block with a broken codex signature is dropped, not sent half-parsed
	sig, _ := json.Marshal(CodexSignaturePrefix + "{broken")
	body, _ = ToResponses(mustReq(t, `{"model":"m","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"x","signature":`+string(sig)+`},{"type":"text","text":"ok"}]}]}`), ResponsesOptions{Model: "x"})
	if n := len(body["input"].([]map[string]any)); n != 1 {
		t.Errorf("input = %v", body["input"])
	}
}

func rsse(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString("event: x\ndata: ")
		b.WriteString(e)
		b.WriteString("\n\n")
	}
	return b.String()
}

func runResponses(t *testing.T, up string) (assembled, error) {
	t.Helper()
	var out strings.Builder
	err := StreamResponses(strings.NewReader(up), &out, func() {}, "claude-opus-5-5")
	return assemble(t, parseSSE(t, out.String())), err
}

const codexHappy = `{"type":"response.created","response":{"status":"in_progress"}}
{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}
{"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"Need to "}
{"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"list files."}
{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"Need to list files."}],"encrypted_content":"gAAAA"}}
{"type":"response.output_item.added","output_index":1,"item":{"type":"message","role":"assistant"}}
{"type":"response.output_text.delta","output_index":1,"delta":"On it."}
{"type":"response.output_item.done","output_index":1,"item":{"type":"message"}}
{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","call_id":"call_a","name":"Bash","arguments":""}}
{"type":"response.function_call_arguments.delta","output_index":2,"delta":"{\"command\":"}
{"type":"response.function_call_arguments.delta","output_index":2,"delta":"\"ls\"}"}
{"type":"response.output_item.done","output_index":2,"item":{"type":"function_call","call_id":"call_a","name":"Bash","arguments":"{\"command\":\"ls\"}"}}
{"type":"response.output_item.added","output_index":3,"item":{"type":"web_search_call"}}
{"type":"response.output_item.done","output_index":3,"item":{"type":"web_search_call"}}
{"type":"response.output_item.added","output_index":4,"item":{"type":"function_call","call_id":"call_b","name":"Read","arguments":""}}
{"type":"response.output_item.done","output_index":4,"item":{"type":"function_call","call_id":"call_b","name":"Read","arguments":"{\"file_path\":\"/x\"}"}}
{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":900,"output_tokens":40}}}`

func TestStreamResponses(t *testing.T) {
	a, err := runResponses(t, rsse(strings.Split(codexHappy, "\n")...))
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Blocks) != 4 {
		t.Fatalf("blocks = %v", a.Blocks)
	}
	th := a.Blocks[0]
	if th["thinking"] != "Need to list files." {
		t.Errorf("thinking = %v", th["thinking"])
	}
	sig, _ := th["signature"].(string)
	raw, ok := strings.CutPrefix(sig, CodexSignaturePrefix)
	var item map[string]any
	if !ok || json.Unmarshal([]byte(raw), &item) != nil || item["encrypted_content"] != "gAAAA" {
		t.Fatalf("signature must carry the reasoning item: %q", sig)
	}
	if a.Blocks[1]["text"] != "On it." {
		t.Errorf("text = %v", a.Blocks[1])
	}
	if a.Blocks[2]["id"] != "call_a" || a.Blocks[2]["input"].(map[string]any)["command"] != "ls" {
		t.Errorf("tool a = %v", a.Blocks[2])
	}
	// arguments that only arrive on item.done must not be lost
	if a.Blocks[3]["input"].(map[string]any)["file_path"] != "/x" {
		t.Errorf("tool b = %v", a.Blocks[3])
	}
	if a.StopReason != "tool_use" || a.Usage["input_tokens"] != float64(900) {
		t.Errorf("stop=%s usage=%v", a.StopReason, a.Usage)
	}

	// and the thinking block must round-trip into the next request
	blk, _ := json.Marshal([]map[string]any{th, {"type": "text", "text": "On it."}})
	body, _ := ToResponses(mustReq(t, `{"model":"m","messages":[{"role":"assistant","content":`+string(blk)+`}]}`), ResponsesOptions{Model: "x"})
	if body["input"].([]map[string]any)[0]["encrypted_content"] != "gAAAA" {
		t.Error("reasoning lost between turns")
	}
}

func TestStreamResponsesEndings(t *testing.T) {
	text := `{"type":"response.output_item.added","output_index":0,"item":{"type":"message"}}
{"type":"response.output_text.delta","output_index":0,"delta":"partial"}`
	cases := map[string]struct {
		tail    string
		stop    string
		wantErr string
	}{
		"max tokens":   {`{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`, "max_tokens", ""},
		"content filt": {`{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"content_filter"}}}`, "", "content_filter"},
		"failed":       {`{"type":"response.failed","response":{"status":"failed","error":{"code":"rate_limit_exceeded","message":"slow down"}}}`, "", "slow down"},
		"error event":  {`{"type":"error","code":"usage_limit_reached","message":"5h limit"}`, "", "5h limit"},
		"truncated":    {``, "", "ended before finishing"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			lines := strings.Split(text, "\n")
			if tc.tail != "" {
				lines = append(lines, tc.tail)
			}
			a, err := runResponses(t, rsse(lines...))
			if tc.wantErr == "" {
				if err != nil || a.StopReason != tc.stop {
					t.Fatalf("err=%v stop=%s", err, a.StopReason)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(a.Err, tc.wantErr) {
				t.Fatalf("err=%v event=%q", err, a.Err)
			}
			if name == "truncated" && !errors.Is(err, ErrTruncated) {
				t.Errorf("err = %v", err)
			}
		})
	}
}

func TestCollect(t *testing.T) {
	var out strings.Builder
	if err := StreamResponses(strings.NewReader(rsse(strings.Split(codexHappy, "\n")...)), &out, nil, "claude-opus-5-5"); err != nil {
		t.Fatal(err)
	}
	msg, err := Collect([]byte(out.String()))
	if err != nil {
		t.Fatal(err)
	}
	if msg["model"] != "claude-opus-5-5" || msg["stop_reason"] != "tool_use" {
		t.Errorf("msg = %v", msg)
	}
	content := msg["content"].([]map[string]any)
	if len(content) != 4 || content[2]["input"].(map[string]any)["command"] != "ls" {
		t.Errorf("content = %v", content)
	}
	if _, err := Collect([]byte("data: {\"type\":\"error\",\"error\":{\"message\":\"boom\"}}\n")); err == nil {
		t.Error("error event must surface")
	}
	empty, _ := Collect(nil)
	if raw, _ := json.Marshal(empty["content"]); string(raw) != "[]" {
		t.Errorf("empty content must be [] not %s", raw)
	}
}
