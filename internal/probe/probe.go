// Package probe checks that a provider survives the round trip through the
// translation: it boots the real proxy in memory with only that provider and
// talks to it the way Claude Code does.
package probe

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ruivalim/claude-rollet/internal/config"
	"github.com/ruivalim/claude-rollet/internal/proxy"
	"github.com/ruivalim/claude-rollet/internal/store"
)

type Result struct {
	Name   string
	OK     bool
	Detail string
	Took   time.Duration
}

type check struct {
	name string
	run  func(ctx context.Context, url string) (string, error)
}

var checks = []check{
	{"text reply", textReply},
	{"streaming", streaming},
	{"tool call + tool result", toolRoundTrip},
}

func Names() []string {
	out := make([]string, len(checks))
	for i, c := range checks {
		out[i] = c.name
	}
	return out
}

// Run executes every check, calling onResult as each one finishes. A failed
// check doesn't stop the others: knowing text works but tools don't is useful.
func Run(ctx context.Context, p config.Provider, onResult func(Result)) ([]Result, error) {
	// A paused provider (weight 0) is still worth testing before you
	// unpause it; the weight only matters to the roll.
	p.Weight = nil
	cfg := &config.Config{Providers: []config.Provider{p}}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "rollet-probe-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	srv := httptest.NewServer(proxy.New(cfg, store.New(filepath.Join(dir, "s.json")), log.New(io.Discard, "", 0)))
	defer srv.Close()

	var out []Result
	for _, c := range checks {
		cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		start := time.Now()
		detail, err := c.run(cctx, srv.URL)
		cancel()
		r := Result{Name: c.name, OK: err == nil, Detail: detail, Took: time.Since(start)}
		if err != nil {
			r.Detail = err.Error()
		}
		out = append(out, r)
		if onResult != nil {
			onResult(r)
		}
	}
	return out, nil
}

const weatherTool = `{"name":"get_weather","description":"Get the current weather for a city.","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}`

func textReply(ctx context.Context, url string) (string, error) {
	body := `{"model":"claude-opus-5-5","max_tokens":200,"messages":[{"role":"user","content":"Reply with the single word: pong"}]}`
	raw, err := send(ctx, url, body)
	if err != nil {
		return "", err
	}
	var m struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", fmt.Errorf("bad JSON back: %w", err)
	}
	for _, c := range m.Content {
		if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
			return quote(c.Text), nil
		}
	}
	return "", fmt.Errorf("no text in the answer: %s", clip(string(raw)))
}

func streaming(ctx context.Context, url string) (string, error) {
	body := `{"model":"claude-opus-5-5","max_tokens":200,"stream":true,"messages":[{"role":"user","content":"Count from 1 to 5, separated by spaces."}]}`
	raw, err := send(ctx, url, body)
	if err != nil {
		return "", err
	}
	msg, err := Assemble(raw)
	if err != nil {
		return "", err
	}
	if msg.Text == "" {
		return "", fmt.Errorf("stream finished without text (stop_reason %s)", msg.StopReason)
	}
	return fmt.Sprintf("%s · %d events", quote(msg.Text), msg.Events), nil
}

func toolRoundTrip(ctx context.Context, url string) (string, error) {
	ask := `{"model":"claude-opus-5-5","max_tokens":500,"stream":true,"tools":[` + weatherTool + `],
	  "messages":[{"role":"user","content":"What's the weather in Lisbon right now? Use the get_weather tool."}]}`
	raw, err := send(ctx, url, ask)
	if err != nil {
		return "", err
	}
	msg, err := Assemble(raw)
	if err != nil {
		return "", err
	}
	if len(msg.Tools) == 0 {
		return "", fmt.Errorf("model answered without calling the tool: %s", quote(msg.Text))
	}
	call := msg.Tools[0]
	if msg.StopReason != "tool_use" {
		return "", fmt.Errorf("tool called but stop_reason is %q, Claude Code would not run it", msg.StopReason)
	}
	city, _ := call.Input["city"].(string)
	if call.Name != "get_weather" || city == "" {
		return "", fmt.Errorf("bad tool call: %s(%v)", call.Name, call.Input)
	}

	// Second leg: send the tool result back, as Claude Code does after
	// running a tool. This is where the tool_result -> role:tool mapping and
	// tool_call_id pairing get exercised.
	input, _ := json.Marshal(call.Input)
	follow := fmt.Sprintf(`{"model":"claude-opus-5-5","max_tokens":300,"stream":true,"tools":[%s],"messages":[
	  {"role":"user","content":"What's the weather in Lisbon right now? Use the get_weather tool."},
	  {"role":"assistant","content":[{"type":"tool_use","id":%q,"name":"get_weather","input":%s}]},
	  {"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"Sunny, 23 C, light wind."}]}
	]}`, weatherTool, call.ID, input, call.ID)
	raw, err = send(ctx, url, follow)
	if err != nil {
		return "", fmt.Errorf("sending tool result back: %w", err)
	}
	msg2, err := Assemble(raw)
	if err != nil {
		return "", fmt.Errorf("answer after tool result: %w", err)
	}
	if msg2.Text == "" {
		return "", fmt.Errorf("no answer after the tool result (stop_reason %s)", msg2.StopReason)
	}
	return fmt.Sprintf("get_weather(%q) → %s", city, quote(msg2.Text)), nil
}

func send(ctx context.Context, url, body string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/messages", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Claude-Code-Session-Id", "rollet-probe")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, clip(e.Error.Message))
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, clip(string(raw)))
	}
	return raw, nil
}

type ToolCall struct {
	ID    string
	Name  string
	Input map[string]any
}

type Message struct {
	Text       string
	Tools      []ToolCall
	StopReason string
	Events     int
}

// Assemble rebuilds a message from Anthropic SSE and rejects any event order
// the Anthropic SDK inside Claude Code would refuse.
func Assemble(raw []byte) (Message, error) {
	var m Message
	type block struct {
		kind    string
		id      string
		name    string
		partial strings.Builder
	}
	var blocks []*block
	open := -1
	gotStart, gotStop := false, false

	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
			Block struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return m, fmt.Errorf("invalid SSE data: %w", err)
		}
		m.Events++
		switch ev.Type {
		case "message_start":
			gotStart = true
		case "content_block_start":
			if !gotStart || open != -1 || ev.Index != len(blocks) {
				return m, fmt.Errorf("out-of-order content_block_start (index %d)", ev.Index)
			}
			blocks = append(blocks, &block{kind: ev.Block.Type, id: ev.Block.ID, name: ev.Block.Name})
			open = ev.Index
		case "content_block_delta":
			if ev.Index != open {
				return m, fmt.Errorf("delta for block %d while %d is open", ev.Index, open)
			}
			switch ev.Delta.Type {
			case "text_delta":
				m.Text += ev.Delta.Text
			case "input_json_delta":
				blocks[open].partial.WriteString(ev.Delta.PartialJSON)
			}
		case "content_block_stop":
			if ev.Index != open {
				return m, fmt.Errorf("stop for block %d while %d is open", ev.Index, open)
			}
			open = -1
		case "message_delta":
			m.StopReason = ev.Delta.StopReason
		case "message_stop":
			gotStop = true
		case "error":
			return m, fmt.Errorf("error mid-stream: %s", ev.Error.Message)
		}
	}
	if !gotStart || !gotStop {
		return m, fmt.Errorf("stream incomplete (start=%v stop=%v)", gotStart, gotStop)
	}
	for _, b := range blocks {
		if b.kind != "tool_use" {
			continue
		}
		in := map[string]any{}
		if s := b.partial.String(); s != "" {
			if err := json.Unmarshal([]byte(s), &in); err != nil {
				return m, fmt.Errorf("tool input is not valid JSON: %s", clip(s))
			}
		}
		m.Tools = append(m.Tools, ToolCall{ID: b.id, Name: b.name, Input: in})
	}
	m.Text = strings.TrimSpace(m.Text)
	return m, nil
}

func quote(s string) string { return fmt.Sprintf("%q", clip(strings.Join(strings.Fields(s), " "))) }

func clip(s string) string {
	if r := []rune(s); len(r) > 80 {
		return string(r[:80]) + "…"
	}
	return s
}
