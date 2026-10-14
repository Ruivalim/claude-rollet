// Package codex signs in with a ChatGPT subscription (the same device-code
// OAuth the Codex CLI uses) and keeps the token fresh.
//
// rollet keeps its own login instead of reading ~/.codex/auth.json: OpenAI
// rotates the refresh token on every refresh, so two programs sharing one
// login keep logging each other out.
package codex

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ruivalim/claude-rollet/internal/config"
)

const (
	clientID        = "app_EMoamEEZ73f0CkXaXp7hrann" // Codex CLI's public OAuth client
	VerificationURL = "https://auth.openai.com/codex/device"
	jwtClaimPath    = "https://api.openai.com/auth"
)

// Endpoints is swappable so tests can stand in for auth.openai.com.
type Endpoints struct {
	UserCode, DeviceToken, Token, DeviceRedirect string
}

var OpenAI = Endpoints{
	UserCode:       "https://auth.openai.com/api/accounts/deviceauth/usercode",
	DeviceToken:    "https://auth.openai.com/api/accounts/deviceauth/token",
	Token:          "https://auth.openai.com/oauth/token",
	DeviceRedirect: "https://auth.openai.com/deviceauth/callback",
}

type Tokens struct {
	Access    string    `json:"access_token"`
	Refresh   string    `json:"refresh_token"`
	Expires   time.Time `json:"expires"`
	AccountID string    `json:"account_id"`
}

// Polling floor and slow_down step from RFC 8628; tests shrink them.
var (
	minPoll      = time.Second
	slowDownStep = 5 * time.Second
)

var ErrNotLoggedIn = errors.New("not signed in to ChatGPT, run `rollet codex login`")

// Auth owns the token file. One Auth per process; the flock covers the rest.
type Auth struct {
	Path      string
	Endpoints Endpoints
	Client    *http.Client
	mu        sync.Mutex
}

// DefaultPath sits next to config.json, so ROLLET_HOME moves it too.
func DefaultPath() string {
	home, err := config.Home()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, "codex-auth.json")
}

func NewAuth(path string) *Auth {
	return &Auth{Path: path, Endpoints: OpenAI, Client: &http.Client{Timeout: 30 * time.Second}}
}

// Token returns a valid access token, refreshing it when it's about to
// expire. Refresh happens under an exclusive file lock and re-reads the file
// first: if another process (the service, a `provider test`) just refreshed,
// its new token is used instead of burning the rotated refresh token again.
func (a *Auth) Token(ctx context.Context) (Tokens, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	t, err := a.read()
	if err != nil {
		return Tokens{}, err
	}
	if time.Until(t.Expires) > 5*time.Minute {
		return t, nil
	}
	unlock, err := a.lock()
	if err != nil {
		return Tokens{}, err
	}
	defer unlock()
	if t, err = a.read(); err != nil {
		return Tokens{}, err
	}
	if time.Until(t.Expires) > 5*time.Minute {
		return t, nil
	}
	fresh, err := a.tokenRequest(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {t.Refresh},
		"client_id":     {clientID},
	})
	if err != nil {
		return Tokens{}, fmt.Errorf("refreshing ChatGPT login (run `rollet codex login` if it keeps failing): %w", err)
	}
	return fresh, a.write(fresh)
}

func (a *Auth) Status() (Tokens, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.read()
}

// Save stores tokens (used after login, and by tests).
func (a *Auth) Save(t Tokens) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	unlock, err := a.lock()
	if err != nil {
		return err
	}
	defer unlock()
	return a.write(t)
}

func (a *Auth) Logout() error {
	err := os.Remove(a.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// DeviceCode is what the user types at VerificationURL.
type DeviceCode struct {
	ID       string
	UserCode string
	Interval time.Duration
}

func (a *Auth) StartDeviceLogin(ctx context.Context) (DeviceCode, error) {
	var out struct {
		DeviceAuthID string          `json:"device_auth_id"`
		UserCode     string          `json:"user_code"`
		Interval     json.RawMessage `json:"interval"`
	}
	status, raw, err := a.postJSON(ctx, a.Endpoints.UserCode, map[string]any{"client_id": clientID}, &out)
	if err != nil {
		return DeviceCode{}, err
	}
	if status == 404 {
		return DeviceCode{}, errors.New("OpenAI has device login switched off for this account")
	}
	if status != 200 || out.DeviceAuthID == "" || out.UserCode == "" {
		return DeviceCode{}, fmt.Errorf("device login failed (HTTP %d): %s", status, clip(raw))
	}
	// interval comes as a number or a numeric string; RFC 8628 says 5s if absent
	secs, err := strconv.ParseFloat(strings.Trim(string(out.Interval), `" `), 64)
	if err != nil || secs <= 0 {
		secs = 5
	}
	return DeviceCode{ID: out.DeviceAuthID, UserCode: out.UserCode, Interval: time.Duration(secs * float64(time.Second))}, nil
}

// FinishDeviceLogin polls until the code is approved, then trades it for
// tokens and saves them. Gives up after 15 minutes, like the Codex CLI.
func (a *Auth) FinishDeviceLogin(ctx context.Context, dc DeviceCode) (Tokens, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	interval := max(dc.Interval, minPoll)
	for {
		select {
		case <-ctx.Done():
			return Tokens{}, fmt.Errorf("device login: %w", ctx.Err())
		case <-time.After(interval):
		}
		var out struct {
			AuthorizationCode string `json:"authorization_code"`
			CodeVerifier      string `json:"code_verifier"`
		}
		status, raw, err := a.postJSON(ctx, a.Endpoints.DeviceToken, map[string]any{
			"device_auth_id": dc.ID, "user_code": dc.UserCode,
		}, &out)
		if err != nil {
			return Tokens{}, err
		}
		switch {
		case status == 200 && out.AuthorizationCode != "":
			t, err := a.tokenRequest(ctx, url.Values{
				"grant_type":    {"authorization_code"},
				"client_id":     {clientID},
				"code":          {out.AuthorizationCode},
				"code_verifier": {out.CodeVerifier},
				"redirect_uri":  {a.Endpoints.DeviceRedirect},
			})
			if err != nil {
				return Tokens{}, err
			}
			return t, a.Save(t)
		case status == 403 || status == 404 || bytes.Contains(raw, []byte("authorization_pending")):
			// not approved yet
		case bytes.Contains(raw, []byte("slow_down")):
			interval += slowDownStep
		default:
			return Tokens{}, fmt.Errorf("device login failed (HTTP %d): %s", status, clip(raw))
		}
	}
}

func (a *Auth) tokenRequest(ctx context.Context, form url.Values) (Tokens, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.Endpoints.Token, strings.NewReader(form.Encode()))
	if err != nil {
		return Tokens{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.Client.Do(req)
	if err != nil {
		return Tokens{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return Tokens{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, clip(raw))
	}
	var out struct {
		Access    string  `json:"access_token"`
		Refresh   string  `json:"refresh_token"`
		ExpiresIn float64 `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Access == "" || out.Refresh == "" || out.ExpiresIn <= 0 {
		return Tokens{}, fmt.Errorf("token response missing fields: %s", clip(raw))
	}
	account, err := AccountID(out.Access)
	if err != nil {
		return Tokens{}, err
	}
	return Tokens{
		Access:    out.Access,
		Refresh:   out.Refresh,
		Expires:   time.Now().Add(time.Duration(out.ExpiresIn * float64(time.Second))).UTC(),
		AccountID: account,
	}, nil
}

// AccountID reads chatgpt_account_id out of the access token (a JWT). The
// backend wants it as a header on every call.
func AccountID(access string) (string, error) {
	parts := strings.Split(access, ".")
	if len(parts) != 3 {
		return "", errors.New("access token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return "", fmt.Errorf("access token payload: %w", err)
	}
	var claims map[string]json.RawMessage
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("access token payload: %w", err)
	}
	var auth struct {
		AccountID string `json:"chatgpt_account_id"`
	}
	_ = json.Unmarshal(claims[jwtClaimPath], &auth)
	if auth.AccountID == "" {
		return "", errors.New("access token has no ChatGPT account (is this a ChatGPT login?)")
	}
	return auth.AccountID, nil
}

func (a *Auth) postJSON(ctx context.Context, endpoint string, body any, out any) (int, []byte, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.Client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == 200 {
		_ = json.Unmarshal(raw, out)
	}
	return resp.StatusCode, raw, nil
}

func (a *Auth) read() (Tokens, error) {
	raw, err := os.ReadFile(a.Path)
	if errors.Is(err, os.ErrNotExist) {
		return Tokens{}, ErrNotLoggedIn
	}
	if err != nil {
		return Tokens{}, err
	}
	var t Tokens
	if err := json.Unmarshal(raw, &t); err != nil || t.Refresh == "" {
		return Tokens{}, fmt.Errorf("%s is damaged, run `rollet codex login` again", a.Path)
	}
	return t, nil
}

func (a *Auth) write(t Tokens) error {
	if err := os.MkdirAll(filepath.Dir(a.Path), 0o700); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(t, "", "  ")
	tmp, err := os.CreateTemp(filepath.Dir(a.Path), ".codex-auth-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), a.Path)
}

func (a *Auth) lock() (func(), error) {
	if err := os.MkdirAll(filepath.Dir(a.Path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(a.Path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

func clip(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
