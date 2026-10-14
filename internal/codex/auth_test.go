package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	minPoll, slowDownStep = time.Millisecond, time.Millisecond
	os.Exit(m.Run())
}

func fakeJWT(account string) string {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]any{"alg": "none"}) + "." +
		enc(map[string]any{jwtClaimPath: map[string]any{"chatgpt_account_id": account}}) + ".sig"
}

type fakeOAuth struct {
	srv       *httptest.Server
	polls     atomic.Int32
	refreshes atomic.Int32
	approveAt int32
	slowDown  bool
	refreshOK bool
}

func newFakeOAuth(t *testing.T) *fakeOAuth {
	f := &fakeOAuth{approveAt: 3, refreshOK: true}
	mux := http.NewServeMux()
	mux.HandleFunc("/usercode", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]string
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b["client_id"] != clientID {
			w.WriteHeader(400)
			return
		}
		fmt.Fprint(w, `{"device_auth_id":"dev-1","user_code":"ABCD-1234","interval":"0.01"}`)
	})
	mux.HandleFunc("/devicetoken", func(w http.ResponseWriter, r *http.Request) {
		n := f.polls.Add(1)
		if f.slowDown && n == 1 {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":{"code":"slow_down"}}`)
			return
		}
		if n < f.approveAt {
			w.WriteHeader(403)
			fmt.Fprint(w, `{"error":{"code":"deviceauth_authorization_pending"}}`)
			return
		}
		fmt.Fprint(w, `{"authorization_code":"code-1","code_verifier":"ver-1"}`)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			if r.Form.Get("code") != "code-1" || r.Form.Get("code_verifier") != "ver-1" || r.Form.Get("redirect_uri") == "" {
				w.WriteHeader(400)
				return
			}
			fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"r1","expires_in":3600}`, fakeJWT("acct-1"))
		case "refresh_token":
			n := f.refreshes.Add(1)
			time.Sleep(50 * time.Millisecond) // widen the race window
			if !f.refreshOK {
				w.WriteHeader(401)
				fmt.Fprint(w, `{"error":"refresh_token_reused"}`)
				return
			}
			fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"r%d","expires_in":3600}`, fakeJWT("acct-1"), n+1)
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOAuth) auth(path string) *Auth {
	a := NewAuth(path)
	a.Endpoints = Endpoints{
		UserCode: f.srv.URL + "/usercode", DeviceToken: f.srv.URL + "/devicetoken",
		Token: f.srv.URL + "/token", DeviceRedirect: "https://x/cb",
	}
	return a
}

func TestDeviceLogin(t *testing.T) {
	f := newFakeOAuth(t)
	f.slowDown = true
	path := filepath.Join(t.TempDir(), "codex-auth.json")
	a := f.auth(path)

	if _, err := a.Token(context.Background()); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("before login: %v", err)
	}
	dc, err := a.StartDeviceLogin(context.Background())
	if err != nil || dc.UserCode != "ABCD-1234" {
		t.Fatalf("start: %+v %v", dc, err)
	}
	dc.Interval = 10 * time.Millisecond
	tok, err := a.FinishDeviceLogin(context.Background(), dc)
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccountID != "acct-1" || tok.Refresh != "r1" {
		t.Errorf("tokens = %+v", tok)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("token file mode %v", st.Mode().Perm())
	}
	if got, err := a.Token(context.Background()); err != nil || got.Access != tok.Access {
		t.Errorf("fresh token should be served from disk: %v", err)
	}
	if f.refreshes.Load() != 0 {
		t.Error("refreshed a token that wasn't expiring")
	}
}

func TestDeviceLoginCancelled(t *testing.T) {
	f := newFakeOAuth(t)
	f.approveAt = 1 << 30
	a := f.auth(filepath.Join(t.TempDir(), "a.json"))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := a.FinishDeviceLogin(ctx, DeviceCode{ID: "dev-1", UserCode: "x", Interval: 10 * time.Millisecond}); err == nil {
		t.Fatal("expected cancellation")
	}
}

func writeExpired(t *testing.T, path string) {
	t.Helper()
	a := NewAuth(path)
	if err := a.write(Tokens{Access: fakeJWT("acct-1"), Refresh: "r1", Expires: time.Now().Add(time.Minute), AccountID: "acct-1"}); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshOnceAcrossProcesses(t *testing.T) {
	f := newFakeOAuth(t)
	path := filepath.Join(t.TempDir(), "codex-auth.json")
	writeExpired(t, path)

	// Two Auth values on one file stand in for the service and a CLI
	// command. The refresh token rotates, so a second refresh would use a
	// dead token: exactly one refresh may happen.
	auths := []*Auth{f.auth(path), f.auth(path)}
	var wg sync.WaitGroup
	results := make([]Tokens, 20)
	for i := range results {
		wg.Go(func() {
			tok, err := auths[i%2].Token(context.Background())
			if err != nil {
				t.Error(err)
			}
			results[i] = tok
		})
	}
	wg.Wait()
	if n := f.refreshes.Load(); n != 1 {
		t.Fatalf("refreshed %d times", n)
	}
	for _, r := range results {
		if r.Refresh != "r2" {
			t.Fatalf("someone kept the stale token: %+v", r)
		}
	}
}

func TestRefreshFailureKeepsFile(t *testing.T) {
	f := newFakeOAuth(t)
	f.refreshOK = false
	path := filepath.Join(t.TempDir(), "codex-auth.json")
	writeExpired(t, path)
	_, err := f.auth(path).Token(context.Background())
	if err == nil || !strings.Contains(err.Error(), "rollet codex login") {
		t.Fatalf("err = %v", err)
	}
	tok, _ := NewAuth(path).Status()
	if tok.Refresh != "r1" {
		t.Error("a failed refresh must not wipe the stored login")
	}
}

func TestDamagedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex-auth.json")
	_ = os.WriteFile(path, []byte("{oops"), 0o600)
	if _, err := NewAuth(path).Token(context.Background()); err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Fatalf("err = %v", err)
	}
}

func TestAccountID(t *testing.T) {
	if id, err := AccountID(fakeJWT("acct-9")); err != nil || id != "acct-9" {
		t.Errorf("%s %v", id, err)
	}
	for _, bad := range []string{"", "a.b", "a.!!!.c", fakeJWT("")} {
		if _, err := AccountID(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

func TestLogout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex-auth.json")
	writeExpired(t, path)
	a := NewAuth(path)
	if err := a.Logout(); err != nil {
		t.Fatal(err)
	}
	if err := a.Logout(); err != nil {
		t.Error("logout twice should be fine")
	}
	if _, err := a.Status(); !errors.Is(err, ErrNotLoggedIn) {
		t.Errorf("err = %v", err)
	}
}
