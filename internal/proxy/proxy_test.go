package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ruivalim/claude-rollet/internal/codex"
	"github.com/ruivalim/claude-rollet/internal/config"
	"github.com/ruivalim/claude-rollet/internal/store"
	"github.com/ruivalim/claude-rollet/internal/translate"
)

type captured struct {
	mu      sync.Mutex
	headers []http.Header
	bodies  []map[string]any
}

func (c *captured) add(r *http.Request) map[string]any {
	var b map[string]any
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &b)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.headers = append(c.headers, r.Header.Clone())
	c.bodies = append(c.bodies, b)
	return b
}

func (c *captured) n() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bodies)
}

func fakeOpenAI(t *testing.T, status int) (*httptest.Server, *captured) {
	c := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("openai path = %s", r.URL.Path)
		}
		body := c.add(r)
		if status != 200 {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":{"message":"slow down"}}`)
			return
		}
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello from fake\"}}]}\n\n"+
				"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		_, _ = io.WriteString(w, `{"model":"secret-model-name","choices":[{"message":{"content":"hello from fake"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

func fakeAnthropic(t *testing.T) (*httptest.Server, *captured) {
	c := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.add(r)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Request-Id", "req_fake")
		_, _ = io.WriteString(w, `{"type":"message","content":[{"type":"text","text":"real claude"}],"path":"`+r.URL.Path+"?"+r.URL.RawQuery+`"}`)
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

func newProxy(t *testing.T, providers ...config.Provider) (*httptest.Server, *Server) {
	cfg := &config.Config{Providers: providers}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	s := New(cfg, store.New(filepath.Join(t.TempDir(), "sessions.json")), log.New(io.Discard, "", 0))
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv, s
}

func post(t *testing.T, url, session, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer client-oauth-token")
	if session != "" {
		req.Header.Set("X-Claude-Code-Session-Id", session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, string(raw)
}

const msgBody = `{"model":"claude-opus-5-5","max_tokens":64000,"messages":[{"role":"user","content":"hi"}]}`

func TestOpenAIRouteNonStreaming(t *testing.T) {
	up, got := fakeOpenAI(t, 200)
	px, _ := newProxy(t, config.Provider{Name: "fake", Type: config.TypeOpenAI, BaseURL: up.URL + "/v1", APIKey: "sk-provider", Model: "fake-1", MaxTokens: 1000})

	resp, body := post(t, px.URL+"/v1/messages?beta=true", "s1", msgBody)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(body), &out)
	if out["model"] != "claude-opus-5-5" || strings.Contains(body, "secret-model-name") {
		t.Errorf("upstream model leaked: %s", body)
	}
	if got.headers[0].Get("Authorization") != "Bearer sk-provider" {
		t.Errorf("auth = %q: client token must never reach a third party", got.headers[0].Get("Authorization"))
	}
	if got.bodies[0]["model"] != "fake-1" || got.bodies[0]["max_tokens"] != float64(1000) {
		t.Errorf("upstream body = %v", got.bodies[0])
	}
}

func TestOpenAIRouteStreaming(t *testing.T) {
	up, _ := fakeOpenAI(t, 200)
	px, _ := newProxy(t, config.Provider{Name: "fake", Type: config.TypeOpenAI, BaseURL: up.URL + "/v1", APIKey: "k", Model: "m"})
	resp, body := post(t, px.URL+"/v1/messages", "s1", strings.Replace(msgBody, `"max_tokens"`, `"stream":true,"max_tokens"`, 1))
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %s", ct)
	}
	for _, want := range []string{"event: message_start", "hello from fake", `"stop_reason":"end_turn"`, "event: message_stop"} {
		if !strings.Contains(body, want) {
			t.Errorf("stream missing %q:\n%s", want, body)
		}
	}
}

func TestUpstreamErrorKeepsStatus(t *testing.T) {
	up, _ := fakeOpenAI(t, 429)
	px, _ := newProxy(t, config.Provider{Name: "fake", Type: config.TypeOpenAI, BaseURL: up.URL + "/v1", APIKey: "k", Model: "m"})
	resp, body := post(t, px.URL+"/v1/messages", "s1", msgBody)
	// 429 must stay 429 so Claude Code backs off and retries.
	if resp.StatusCode != 429 || !strings.Contains(body, "rate_limit_error") || !strings.Contains(body, "slow down") {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
}

func TestUpstreamDown(t *testing.T) {
	px, _ := newProxy(t, config.Provider{Name: "dead", Type: config.TypeOpenAI, BaseURL: "http://127.0.0.1:1/v1", APIKey: "k", Model: "m"})
	resp, body := post(t, px.URL+"/v1/messages", "s1", msgBody)
	if resp.StatusCode != 502 || !strings.Contains(body, `"type":"error"`) {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	if strings.Contains(body, "dead") {
		t.Errorf("error leaks provider name: %s", body)
	}
}

func TestAnthropicPassthrough(t *testing.T) {
	up, got := fakeAnthropic(t)
	px, _ := newProxy(t, config.Provider{Name: "claude", Type: config.TypeAnthropic, BaseURL: up.URL})
	resp, body := post(t, px.URL+"/v1/messages?beta=true", "s1", msgBody)
	if resp.StatusCode != 200 || !strings.Contains(body, "real claude") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `/v1/messages?beta=true`) {
		t.Errorf("query string lost: %s", body)
	}
	if got.headers[0].Get("Authorization") != "Bearer client-oauth-token" {
		t.Error("subscription token must be forwarded to Anthropic")
	}
	if resp.Header.Get("Request-Id") != "req_fake" {
		t.Error("response headers not forwarded")
	}
	if got.bodies[0]["max_tokens"] != float64(64000) {
		t.Error("passthrough must not touch the body")
	}
}

func TestAnthropicCompatibleWithKeyAndModel(t *testing.T) {
	up, got := fakeAnthropic(t)
	px, _ := newProxy(t, config.Provider{Name: "kimi", Type: config.TypeAnthropic, BaseURL: up.URL, APIKey: "sk-kimi", Model: "kimi-k3"})
	post(t, px.URL+"/v1/messages", "s1", msgBody)
	h := got.headers[0]
	if h.Get("Authorization") != "" || h.Get("X-Api-Key") != "sk-kimi" {
		t.Errorf("auth headers = %v / %v", h.Get("Authorization"), h.Get("X-Api-Key"))
	}
	if got.bodies[0]["model"] != "kimi-k3" {
		t.Errorf("model = %v", got.bodies[0]["model"])
	}
}

func TestSessionsAreSticky(t *testing.T) {
	upA, gotA := fakeOpenAI(t, 200)
	upB, gotB := fakeOpenAI(t, 200)
	px, s := newProxy(t,
		config.Provider{Name: "a", Type: config.TypeOpenAI, BaseURL: upA.URL + "/v1", APIKey: "k", Model: "m"},
		config.Provider{Name: "b", Type: config.TypeOpenAI, BaseURL: upB.URL + "/v1", APIKey: "k", Model: "m"},
	)
	draw := int64(0)
	s.Intn = func(int64) int64 { return draw }

	post(t, px.URL+"/v1/messages", "sess-a", msgBody) // rolls a
	draw = 1
	for range 5 {
		post(t, px.URL+"/v1/messages", "sess-a", msgBody) // must stay on a
	}
	post(t, px.URL+"/v1/messages", "sess-b", msgBody) // rolls b

	if gotA.n() != 6 || gotB.n() != 1 {
		t.Fatalf("a=%d b=%d", gotA.n(), gotB.n())
	}
	sess, _, _ := s.Store.Lookup("sess-a")
	if sess.Requests != 6 || sess.Provider != "a" {
		t.Errorf("session = %+v", sess)
	}
}

func TestRemovedProvider(t *testing.T) {
	up, _ := fakeOpenAI(t, 200)
	px, s := newProxy(t, config.Provider{Name: "gone", Type: config.TypeOpenAI, BaseURL: up.URL + "/v1", APIKey: "k", Model: "m"})
	post(t, px.URL+"/v1/messages", "s1", msgBody)
	s.Config().Providers[0].Name = "renamed"
	resp, body := post(t, px.URL+"/v1/messages", "s1", msgBody)
	if resp.StatusCode != 503 || strings.Contains(body, "gone") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

func TestCountTokens(t *testing.T) {
	up, got := fakeOpenAI(t, 200)
	px, _ := newProxy(t, config.Provider{Name: "x", Type: config.TypeOpenAI, BaseURL: up.URL + "/v1", APIKey: "k", Model: "m"})
	resp, body := post(t, px.URL+"/v1/messages/count_tokens", "s1", msgBody)
	if resp.StatusCode != 200 || !strings.Contains(body, "input_tokens") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if got.n() != 0 {
		t.Error("count_tokens must not hit the provider")
	}
}

func TestBadBody(t *testing.T) {
	up, _ := fakeOpenAI(t, 200)
	px, _ := newProxy(t, config.Provider{Name: "x", Type: config.TypeOpenAI, BaseURL: up.URL + "/v1", APIKey: "k", Model: "m"})
	resp, body := post(t, px.URL+"/v1/messages", "s1", `{"messages":"nope"}`)
	if resp.StatusCode != 400 || !strings.Contains(body, "invalid_request_error") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

func TestUpstreamMessage(t *testing.T) {
	for raw, want := range map[string]string{
		`{"error":{"message":"m1"}}`: "m1",
		`{"error":"m2"}`:             "m2",
		`{"message":"m3"}`:           "m3",
		`<html>bad gateway</html>`:   "<html>bad gateway</html>",
		``:                           "upstream returned an empty error",
	} {
		if got := UpstreamMessage([]byte(raw)); got != want {
			t.Errorf("%q -> %q, want %q", raw, got, want)
		}
	}
}

func TestConfigHotReload(t *testing.T) {
	upA, gotA := fakeOpenAI(t, 200)
	upB, gotB := fakeOpenAI(t, 200)
	path := filepath.Join(t.TempDir(), "config.json")
	first := &config.Config{Providers: []config.Provider{{Name: "a", Type: config.TypeOpenAI, BaseURL: upA.URL + "/v1", APIKey: "k", Model: "m"}}}
	if err := first.Save(path); err != nil {
		t.Fatal(err)
	}
	px, s := newProxy(t, first.Providers...)
	s.WatchConfig(path)

	post(t, px.URL+"/v1/messages", "old", msgBody)

	second := &config.Config{Providers: []config.Provider{{Name: "b", Type: config.TypeOpenAI, BaseURL: upB.URL + "/v1", APIKey: "k", Model: "m"}}}
	_ = second.Save(path)
	future := time.Now().Add(time.Hour) // mtime resolution can hide a fast rewrite
	_ = os.Chtimes(path, future, future)
	s.lastCheck = time.Time{}

	post(t, px.URL+"/v1/messages", "new", msgBody)
	if gotA.n() != 1 || gotB.n() != 1 {
		t.Fatalf("a=%d b=%d: new session should land on the reloaded provider", gotA.n(), gotB.n())
	}

	// A broken edit must not take the proxy down.
	_ = os.WriteFile(path, []byte("{oops"), 0o600)
	future = future.Add(time.Hour)
	_ = os.Chtimes(path, future, future)
	s.lastCheck = time.Time{}
	resp, body := post(t, px.URL+"/v1/messages", "new", msgBody)
	if resp.StatusCode != 200 {
		t.Fatalf("broken config killed routing: %d %s", resp.StatusCode, body)
	}
}

func TestOddsOverrideSurvivesReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	half := 0.5
	c := &config.Config{ClaudeOdds: &half, Providers: []config.Provider{{Name: "a", Type: config.TypeOpenAI, BaseURL: "http://x", APIKey: "k", Model: "m"}}}
	_ = c.Save(path)
	_, s := newProxy(t, c.Providers...)
	low := 0.1
	s.OddsOverride = &low
	s.WatchConfig(path)
	future := time.Now().Add(time.Hour)
	_ = os.Chtimes(path, future, future)
	s.lastCheck = time.Time{}
	if got := *s.Config().ClaudeOdds; got != 0.1 {
		t.Fatalf("odds after reload = %v", got)
	}
}

func TestBuildBodyAppliesAdapter(t *testing.T) {
	var req translate.AnthropicRequest
	_ = json.Unmarshal([]byte(`{"model":"claude-opus-5-5","max_tokens":128000,"temperature":1,"messages":[{"role":"user","content":"hi"}]}`), &req)

	body, err := BuildBody(&req, config.Provider{Type: "chatgpt", Model: "gpt-x", MaxTokens: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if body["max_completion_tokens"] != 1000 || body["max_tokens"] != nil {
		t.Errorf("chatgpt must use max_completion_tokens: %v", body)
	}
	if _, has := body["temperature"]; has {
		t.Error("chatgpt patch should drop temperature")
	}

	// The user's max_tokens_param and extra_body beat the adapter.
	body, _ = BuildBody(&req, config.Provider{Type: "chatgpt", Model: "gpt-x", MaxTokensParam: "max_tokens", ExtraBody: map[string]any{"temperature": 0.3}})
	if body["max_tokens"] != 128000 || body["temperature"] != 0.3 {
		t.Errorf("user overrides lost: %v", body)
	}

	body, _ = BuildBody(&req, config.Provider{Type: "mimo", Model: "mimo-v2.5-pro"})
	if body["max_tokens"] != 128000 || body["temperature"] != float64(1) {
		t.Errorf("mimo body = %v", body)
	}
}

func TestDelay(t *testing.T) {
	up, got := fakeOpenAI(t, 200)
	px, s := newProxy(t, config.Provider{Name: "x", Type: config.TypeOpenAI, BaseURL: up.URL + "/v1", APIKey: "k", Model: "m"})
	s.Config().Delay = &config.Delay{First: "300ms", Between: "100ms-150ms"}

	timed := func(body string) time.Duration {
		start := time.Now()
		post(t, px.URL+"/v1/messages", "d1", body)
		return time.Since(start)
	}
	if d := timed(msgBody); d < 300*time.Millisecond {
		t.Errorf("first turn took %v, want >= 300ms", d)
	}
	later := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}]}`
	if d := timed(later); d < 100*time.Millisecond || d > 290*time.Millisecond {
		t.Errorf("later turn took %v, want the between range", d)
	}
	if got.n() != 2 {
		t.Errorf("upstream calls = %d", got.n())
	}
}

func TestDelayCancelledNeverReachesUpstream(t *testing.T) {
	up, got := fakeOpenAI(t, 200)
	px, s := newProxy(t, config.Provider{Name: "x", Type: config.TypeOpenAI, BaseURL: up.URL + "/v1", APIKey: "k", Model: "m"})
	s.Config().Delay = &config.Delay{First: "2s"}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, px.URL+"/v1/messages", strings.NewReader(msgBody))
	req.Header.Set("X-Claude-Code-Session-Id", "c1")
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("expected the client timeout")
	}
	time.Sleep(2500 * time.Millisecond) // past the delay: a leaked request would have gone out by now
	if got.n() != 0 {
		t.Errorf("cancelled turn still hit the provider %d time(s)", got.n())
	}
}

func fakeCodex(t *testing.T) (*httptest.Server, *captured) {
	c := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := c.add(r)
		if r.URL.Path != "/backend-api/codex/responses" {
			w.WriteHeader(404)
			return
		}
		if body["store"] != false || body["stream"] != true {
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"detail":"Store must be set to false"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range []string{
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"message"}}`,
			`{"type":"response.output_text.delta","output_index":0,"delta":"hello from codex"}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"message"}}`,
			`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":5,"output_tokens":3}}}`,
		} {
			_, _ = io.WriteString(w, "data: "+e+"\n\n")
		}
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

func codexProxy(t *testing.T, loggedIn bool) (*httptest.Server, *captured) {
	up, got := fakeCodex(t)
	px, s := newProxy(t, config.Provider{Name: "gpt", Type: "codex", BaseURL: up.URL + "/backend-api", Model: "gpt-6-sol"})
	s.CodexAuth = codex.NewAuth(filepath.Join(t.TempDir(), "codex-auth.json"))
	if loggedIn {
		if err := s.CodexAuth.Save(codex.Tokens{Access: "tok-1", Refresh: "r", Expires: time.Now().Add(time.Hour), AccountID: "acct-1"}); err != nil {
			t.Fatal(err)
		}
	}
	return px, got
}

func TestCodexRoute(t *testing.T) {
	px, got := codexProxy(t, true)
	resp, body := post(t, px.URL+"/v1/messages", "cx1", strings.Replace(msgBody, `"max_tokens"`, `"stream":true,"max_tokens"`, 1))
	if resp.StatusCode != 200 || !strings.Contains(body, "hello from codex") || !strings.Contains(body, "message_stop") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	h := got.headers[0]
	if h.Get("Authorization") != "Bearer tok-1" || h.Get("Chatgpt-Account-Id") != "acct-1" || h.Get("Session-Id") != "cx1" {
		t.Errorf("headers = %v", h)
	}
	if got.bodies[0]["model"] != "gpt-6-sol" || got.bodies[0]["prompt_cache_key"] != "cx1" {
		t.Errorf("body = %v", got.bodies[0])
	}

	// non-streaming request from Claude Code: collected into one message
	resp, body = post(t, px.URL+"/v1/messages", "cx1", msgBody)
	var msg map[string]any
	_ = json.Unmarshal([]byte(body), &msg)
	if resp.StatusCode != 200 || msg["model"] != "claude-opus-5-5" || msg["stop_reason"] != "end_turn" {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

func TestCodexNotLoggedIn(t *testing.T) {
	px, got := codexProxy(t, false)
	resp, body := post(t, px.URL+"/v1/messages", "cx2", msgBody)
	if resp.StatusCode != 401 || !strings.Contains(body, "rollet codex login") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if got.n() != 0 {
		t.Error("no token, no upstream call")
	}
}

func TestQuotaProbeGoesToAnthropicWithoutSession(t *testing.T) {
	anth, gotA := fakeAnthropic(t)
	up, gotO := fakeOpenAI(t, 200)
	px, s := newProxy(t,
		config.Provider{Name: "claude", Type: config.TypeAnthropic, BaseURL: anth.URL},
		config.Provider{Name: "mimo", Type: config.TypeOpenAI, BaseURL: up.URL + "/v1", APIKey: "k", Model: "m"},
	)
	s.Intn = func(int64) int64 { return 1 } // would roll mimo
	s.Config().Delay = &config.Delay{First: "2s"}

	start := time.Now()
	resp, _ := post(t, px.URL+"/v1/messages", "q1", `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"quota"}]}`)
	if resp.StatusCode != 200 || gotA.n() != 1 || gotO.n() != 0 {
		t.Fatalf("status %d, anthropic %d, mimo %d", resp.StatusCode, gotA.n(), gotO.n())
	}
	if time.Since(start) > time.Second {
		t.Error("quota check must not be delayed")
	}
	if list, _ := s.Store.List(); len(list) != 0 {
		t.Errorf("quota check left a session behind: %+v", list)
	}
}
