package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ruivalim/claude-rollet/internal/config"
)

// fakeModel answers like a real OpenAI-compatible model: calls the tool when
// one is offered and the last message is from the user, answers in text
// after a tool result. With useTools=false it ignores tools, which is what
// weak models do and what the probe must catch.
func fakeModel(t *testing.T, useTools bool) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Stream   bool             `json:"stream"`
			Tools    []any            `json:"tools"`
			Messages []map[string]any `json:"messages"`
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Error(err)
		}
		last := req.Messages[len(req.Messages)-1]
		if last["role"] == "tool" && last["tool_call_id"] != "call_w" {
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"error":{"message":"tool_call_id mismatch"}}`)
			return
		}
		callTool := useTools && len(req.Tools) > 0 && last["role"] == "user"

		if !req.Stream {
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"pong"},"finish_reason":"stop"}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if callTool {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_w","function":{"name":"get_weather","arguments":"{\"ci"}}]}}]}`+"\n\n")
			fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"Lisbon\"}"}}]}}]}`+"\n\n")
			fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\ndata: [DONE]\n\n")
			return
		}
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"1 2 3 4 5"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func provider(url string) config.Provider {
	return config.Provider{Name: "t", Type: config.TypeOpenAI, BaseURL: url, APIKey: "k", Model: "m"}
}

func TestRunAllPass(t *testing.T) {
	srv := fakeModel(t, true)
	var seen []string
	res, err := Run(context.Background(), provider(srv.URL), func(r Result) { seen = append(seen, r.Name) })
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != len(Names()) {
		t.Errorf("callback saw %v", seen)
	}
	for _, r := range res {
		if !r.OK {
			t.Errorf("%s failed: %s", r.Name, r.Detail)
		}
	}
	if !strings.Contains(res[2].Detail, "Lisbon") {
		t.Errorf("tool detail = %s", res[2].Detail)
	}
}

func TestRunCatchesModelThatIgnoresTools(t *testing.T) {
	srv := fakeModel(t, false)
	res, err := Run(context.Background(), provider(srv.URL), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res[0].OK || !res[1].OK {
		t.Error("text and streaming should still pass")
	}
	if res[2].OK || !strings.Contains(res[2].Detail, "without calling the tool") {
		t.Errorf("tool check = %+v", res[2])
	}
}

func TestRunBadKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid api key"}}`)
	}))
	defer srv.Close()
	res, _ := Run(context.Background(), provider(srv.URL), nil)
	for _, r := range res {
		if r.OK || !strings.Contains(r.Detail, "invalid api key") {
			t.Errorf("%s: %+v", r.Name, r)
		}
	}
}

func TestRunInvalidProvider(t *testing.T) {
	if _, err := Run(context.Background(), config.Provider{Name: "x", Type: config.TypeOpenAI}, nil); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestAssembleRejectsBrokenOrder(t *testing.T) {
	bad := "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"x\"}}\n\n"
	if _, err := Assemble([]byte(bad)); err == nil {
		t.Fatal("delta without start must fail")
	}
}

func TestListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" || r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(401)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"zeta"},{"id":"alpha"},{"id":""}]}`)
	}))
	defer srv.Close()
	ids, err := ListModels(context.Background(), srv.URL, "k")
	if err != nil || strings.Join(ids, ",") != "alpha,zeta" {
		t.Fatalf("ids=%v err=%v", ids, err)
	}
	if _, err := ListModels(context.Background(), srv.URL, "wrong"); err == nil {
		t.Error("401 must be an error")
	}
}
