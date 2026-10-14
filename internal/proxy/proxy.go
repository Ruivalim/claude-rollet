// Package proxy is the HTTP server Claude Code talks to.
package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"maps"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ruivalim/claude-rollet/internal/adapters"
	"github.com/ruivalim/claude-rollet/internal/claudecode"
	"github.com/ruivalim/claude-rollet/internal/config"
	"github.com/ruivalim/claude-rollet/internal/roll"
	"github.com/ruivalim/claude-rollet/internal/store"
	"github.com/ruivalim/claude-rollet/internal/translate"
)

const anthropicAPI = "https://api.anthropic.com"

type Server struct {
	Store  *store.Store
	Intn   roll.Intn
	Client *http.Client
	Log    *log.Logger
	// Reveal prints provider names in the log. Off by default: a glance at
	// the server log is enough to spoil the experiment.
	Reveal bool

	// ConfigPath, when set, is re-read whenever the file changes, so
	// `rollet provider add` or `rollet odds` apply without a restart.
	ConfigPath string
	// OddsOverride / OddsOff (serve --claude-odds) survive reloads.
	OddsOverride *float64
	OddsOff      bool

	cfgMu     sync.Mutex
	cfg       *config.Config
	cfgMtime  time.Time
	lastCheck time.Time
}

func New(cfg *config.Config, st *store.Store, logger *log.Logger) *Server {
	return &Server{
		cfg:   cfg,
		Store: st,
		Intn:  roll.CryptoIntn,
		Client: &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: 10 * time.Minute,
			IdleConnTimeout:       90 * time.Second,
			ForceAttemptHTTP2:     true,
		}},
		Log: logger,
	}
}

// Config returns the current config, reloading it if the file changed. A
// broken edit is logged and the last good config keeps serving.
func (s *Server) Config() *config.Config {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	if s.ConfigPath == "" || time.Since(s.lastCheck) < time.Second {
		return s.cfg
	}
	s.lastCheck = time.Now()
	st, err := os.Stat(s.ConfigPath)
	if err != nil || st.ModTime().Equal(s.cfgMtime) {
		return s.cfg
	}
	s.cfgMtime = st.ModTime()
	next, err := config.Load(s.ConfigPath)
	if err == nil {
		s.applyOverride(next)
		err = next.Validate()
	}
	if err != nil {
		s.Log.Printf("config changed but is invalid, keeping the previous one: %v", err)
		return s.cfg
	}
	s.cfg = next
	s.Log.Printf("config reloaded (%d providers)", len(next.Providers))
	return s.cfg
}

// WatchConfig starts tracking path; cfg must be what was just loaded from it.
func (s *Server) WatchConfig(path string) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	s.ConfigPath = path
	if st, err := os.Stat(path); err == nil {
		s.cfgMtime = st.ModTime()
	}
	s.lastCheck = time.Now()
	s.applyOverride(s.cfg)
}

func (s *Server) applyOverride(c *config.Config) {
	if s.OddsOff {
		c.ClaudeOdds = nil
	} else if s.OddsOverride != nil {
		v := *s.OddsOverride
		c.ClaudeOdds = &v
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/health":
		writeJSON(w, 200, map[string]any{"ok": true, "providers": len(s.Config().Providers)})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages":
		s.messages(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/count_tokens":
		s.countTokens(w, r)
	default:
		// Anything else Claude Code calls (models list, telemetry-ish
		// endpoints) goes to Anthropic untouched.
		body, _ := io.ReadAll(r.Body)
		s.passthrough(w, r, body, s.defaultAnthropic())
	}
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, 400, "reading request: "+err.Error())
		return
	}
	cc := claudecode.Parse(r.Header, body)
	cfg := s.Config()

	sess, isNew, err := s.Store.Touch(cc.SessionID, store.Info{CWD: cc.CWD, Title: cc.Title}, func() (string, string, error) {
		p, err := roll.Pick(cfg, s.Intn)
		if err != nil {
			return "", "", err
		}
		return p.Name, p.Model, nil
	})
	if err != nil {
		s.Log.Printf("store: %v", err)
		writeError(w, 500, "rollet: "+err.Error())
		return
	}
	_, prov := cfg.Provider(sess.Provider)
	if prov == nil {
		writeError(w, 503, "rollet: the provider this session was rolled to is no longer configured; start a new session")
		return
	}
	if isNew {
		s.Log.Printf("session %s rolled -> %s  (%s)", short(sess.ID), s.name(prov), cc.CWD)
	}

	switch prov.Type {
	case config.TypeAnthropic:
		s.passthrough(w, r, body, *prov)
	default:
		s.openai(w, r, body, *prov)
	}
}

func (s *Server) openai(w http.ResponseWriter, r *http.Request, body []byte, p config.Provider) {
	var req translate.AnthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, 400, "invalid request body: "+err.Error())
		return
	}
	upBody, err := BuildBody(&req, p)
	if err != nil {
		writeError(w, 400, "translating request: "+err.Error())
		return
	}
	resp, err := DoOpenAI(r, s.Client, p, upBody)
	if err != nil {
		s.Log.Printf("upstream %s: %v", s.name(&p), err)
		writeError(w, 502, "upstream unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		msg := UpstreamMessage(raw)
		s.Log.Printf("upstream %s: HTTP %d: %s", s.name(&p), resp.StatusCode, msg)
		writeError(w, resp.StatusCode, msg)
		return
	}

	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(200)
		flush := func() {}
		if f, ok := w.(http.Flusher); ok {
			flush = f.Flush
		}
		if err := translate.Stream(resp.Body, w, flush, req.Model); err != nil {
			s.Log.Printf("stream %s: %v", s.name(&p), err)
		}
		return
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		writeError(w, 502, "reading upstream: "+err.Error())
		return
	}
	out, err := translate.FromOpenAI(raw, req.Model)
	if err != nil {
		writeError(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, out)
}

// BuildBody runs the shared translation, then the provider's adapter.
func BuildBody(req *translate.AnthropicRequest, p config.Provider) (map[string]any, error) {
	a, _ := adapters.Get(p.Type)
	param := p.MaxTokensParam
	if param == "" {
		param = a.MaxTokensParam
	}
	body, err := translate.ToOpenAI(req, translate.Options{
		Model:          p.ModelFor(req.Model),
		MaxTokens:      p.MaxTokens,
		MaxTokensParam: param,
	})
	if err != nil {
		return nil, err
	}
	if a.Patch != nil {
		a.Patch(body)
	}
	// extra_body goes last: it's the user's escape hatch, adapters don't
	// get to override it.
	maps.Copy(body, p.ExtraBody)
	return body, nil
}

// DoOpenAI posts a translated body to the provider's /chat/completions.
// Exported so `rollet provider test` exercises the exact same path.
func DoOpenAI(r *http.Request, client *http.Client, p config.Provider, body map[string]any) (*http.Response, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	up, err := http.NewRequestWithContext(r.Context(), http.MethodPost, p.BaseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	up.Header.Set("Content-Type", "application/json")
	up.Header.Set("Authorization", "Bearer "+p.Key())
	if body["stream"] == true {
		up.Header.Set("Accept", "text/event-stream")
	}
	return client.Do(up)
}

// UpstreamMessage pulls a readable message out of whatever error body the
// provider returned.
func UpstreamMessage(raw []byte) string {
	var e struct {
		Error any    `json:"error"`
		Msg   string `json:"message"`
	}
	if json.Unmarshal(raw, &e) == nil {
		switch v := e.Error.(type) {
		case map[string]any:
			if m, ok := v["message"].(string); ok && m != "" {
				return m
			}
		case string:
			if v != "" {
				return v
			}
		}
		if e.Msg != "" {
			return e.Msg
		}
	}
	msg := strings.TrimSpace(string(raw))
	if len(msg) > 500 {
		msg = msg[:500] + "…"
	}
	if msg == "" {
		msg = "upstream returned an empty error"
	}
	return msg
}

var hopHeaders = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true, "Proxy-Authorization": true,
	"Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true, "Host": true, "Content-Length": true,
}

// passthrough forwards the request as is. With no api_key on the provider,
// Claude Code's own credentials (API key or subscription OAuth) go along.
func (s *Server) passthrough(w http.ResponseWriter, r *http.Request, body []byte, p config.Provider) {
	if p.Model != "" && r.URL.Path == "/v1/messages" {
		body = rewriteModel(body, p.ModelFor(requestModel(body)))
	}
	target := p.BaseURL + r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	up, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	for k, vs := range r.Header {
		if hopHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			up.Header.Add(k, v)
		}
	}
	if key := p.Key(); key != "" {
		up.Header.Del("Authorization")
		up.Header.Set("X-Api-Key", key)
	}
	resp, err := s.Client.Do(up)
	if err != nil {
		s.Log.Printf("upstream %s: %v", s.name(&p), err)
		writeError(w, 502, "upstream unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		if hopHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// countTokens: Claude Code uses it for context accounting. Translated
// providers have no such endpoint, so it is estimated at ~4 bytes per token.
func (s *Server) countTokens(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	cc := claudecode.Parse(r.Header, body)
	if sess, ok, _ := s.Store.Lookup(cc.SessionID); ok {
		if _, p := s.Config().Provider(sess.Provider); p != nil && p.Type == config.TypeAnthropic {
			s.passthrough(w, r, body, *p)
			return
		}
	}
	writeJSON(w, 200, map[string]any{"input_tokens": len(body) / 4})
}

func (s *Server) defaultAnthropic() config.Provider {
	for _, p := range s.Config().Providers {
		if p.Type == config.TypeAnthropic && p.Model == "" {
			return p
		}
	}
	return config.Provider{Name: "anthropic", Type: config.TypeAnthropic, BaseURL: anthropicAPI}
}

func (s *Server) name(p *config.Provider) string {
	if s.Reveal {
		return p.Name
	}
	return "???"
}

func requestModel(body []byte) string {
	var v struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &v)
	return v.Model
}

func rewriteModel(body []byte, model string) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	q, _ := json.Marshal(model)
	m["model"] = q
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, translate.ErrorBody(status, msg))
}
