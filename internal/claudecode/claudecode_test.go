package claudecode

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A trimmed copy of a real Claude Code 2.1.299 request.
const realish = `{
  "model":"claude-opus-5-5",
  "metadata":{"user_id":"{\"device_id\":\"d\",\"account_uuid\":\"a\",\"session_id\":\"e6dfb782-9b72-4959-bf6d-b7f7e6a51609\"}"},
  "system":[{"type":"text","text":"You are Claude Code"}],
  "messages":[
    {"role":"user","content":[
      {"type":"text","text":"<system-reminder>\nCLAUDE.md stuff\n</system-reminder>"},
      {"type":"text","text":"  fix   the\nflaky test  "}
    ]},
    {"role":"system","content":[{"type":"text","text":"# Environment\n - Primary working directory: /home/u/proj\n"}]}
  ],
  "safeguards":[{"type":"dangerous_tool_use","classifier_context":{"live_cwd":"/home/u/proj/sub"}}]
}`

func TestParseRealRequest(t *testing.T) {
	h := http.Header{}
	h.Set(SessionHeader, "from-header")
	r := Parse(h, []byte(realish))
	if r.SessionID != "from-header" {
		t.Errorf("header should win, got %s", r.SessionID)
	}
	if r.CWD != "/home/u/proj/sub" {
		t.Errorf("cwd = %s", r.CWD)
	}
	if r.Title != "fix the flaky test" {
		t.Errorf("title = %q", r.Title)
	}

	r = Parse(http.Header{}, []byte(realish))
	if r.SessionID != "e6dfb782-9b72-4959-bf6d-b7f7e6a51609" {
		t.Errorf("metadata JSON session = %s", r.SessionID)
	}
}

func TestParseFallbacks(t *testing.T) {
	legacy := `{"metadata":{"user_id":"user_abc_account__session_11111111-2222-3333-4444-555555555555"},
	  "messages":[{"role":"system","content":"Working directory: /srv/app"},{"role":"user","content":"hello"}]}`
	r := Parse(http.Header{}, []byte(legacy))
	if r.SessionID != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("legacy session = %s", r.SessionID)
	}
	if r.CWD != "/srv/app" {
		t.Errorf("cwd from text = %s", r.CWD)
	}

	// No session anywhere: key must be stable across turns of one
	// conversation and differ between conversations.
	turn1 := `{"system":"s","messages":[{"role":"user","content":"start A"}]}`
	turn2 := `{"system":"s","messages":[{"role":"user","content":"start A"},{"role":"assistant","content":"ok"},{"role":"user","content":"more"}]}`
	other := `{"system":"s","messages":[{"role":"user","content":"start B"}]}`
	a, b, c := Parse(nil, []byte(turn1)), Parse(nil, []byte(turn2)), Parse(nil, []byte(other))
	if !strings.HasPrefix(a.SessionID, "anon-") || a.SessionID != b.SessionID || a.SessionID == c.SessionID {
		t.Errorf("fallback ids: %s %s %s", a.SessionID, b.SessionID, c.SessionID)
	}

	if r := Parse(nil, []byte("not json")); r.SessionID == "" {
		t.Error("garbage body must still get a key")
	}
}

func TestTitleTruncatesOnRunes(t *testing.T) {
	long := strings.Repeat("ã", 150)
	r := Parse(nil, []byte(`{"messages":[{"role":"user","content":"`+long+`"}]}`))
	if got := []rune(r.Title); len(got) != 101 || got[100] != '…' {
		t.Errorf("title runes = %d", len(got))
	}
}

func TestTranscript(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "projects", "-home-u-proj")
	_ = os.MkdirAll(proj, 0o700)
	id := "abc-123"
	content := `{"type":"queue-operation","sessionId":"abc-123"}
{"type":"user","cwd":"/home/u/proj","sessionId":"abc-123"}
`
	_ = os.WriteFile(filepath.Join(proj, id+".jsonl"), []byte(content), 0o600)

	path, cwd, ok := Transcript(dir, id)
	if !ok || cwd != "/home/u/proj" || !strings.HasSuffix(path, id+".jsonl") {
		t.Fatalf("got %s %s %v", path, cwd, ok)
	}
	if _, _, ok := Transcript(dir, "missing"); ok {
		t.Error("missing session reported as found")
	}
}

func TestProbeAndFirstTurn(t *testing.T) {
	quota := Parse(nil, []byte(`{"max_tokens":1,"messages":[{"role":"user","content":"quota"}]}`))
	if !quota.Probe || quota.Title != "" {
		t.Errorf("quota check must be a probe with no title: %+v", quota)
	}
	first := Parse(nil, []byte(realish))
	if first.Probe || !first.FirstTurn {
		t.Errorf("first real turn: %+v", first)
	}
	later := Parse(nil, []byte(`{"max_tokens":128000,"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}]}`))
	if later.Probe || later.FirstTurn {
		t.Errorf("later turn: %+v", later)
	}
	if noMax := Parse(nil, []byte(`{"messages":[{"role":"user","content":"x"}]}`)); noMax.Probe {
		t.Error("missing max_tokens is not a probe")
	}
}

func TestTitleDropsWrapperTags(t *testing.T) {
	r := Parse(nil, []byte(`{"messages":[{"role":"user","content":[
	  {"type":"text","text":"<system-reminder>ctx</system-reminder>"},
	  {"type":"text","text":"<session>Hey mate, how we doing today?</session>"}]}]}`))
	if r.Title != "Hey mate, how we doing today?" {
		t.Errorf("title = %q", r.Title)
	}
	// a prompt that only had tags in it falls through to the next block
	r = Parse(nil, []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"<session></session>"},{"type":"text","text":"real one"}]}]}`))
	if r.Title != "real one" {
		t.Errorf("title = %q", r.Title)
	}
	// comparisons like a < b stay intact
	r = Parse(nil, []byte(`{"messages":[{"role":"user","content":"why is a < b true when b > a"}]}`))
	if r.Title != "why is a < b true when b > a" {
		t.Errorf("title = %q", r.Title)
	}
}

func TestTitleFromClaudeCodeTitleRequest(t *testing.T) {
	// Captured from Claude Code 2.1.299 in interactive mode.
	body := `{"max_tokens":128000,"messages":[{"role":"user","content":[{"type":"text","text":"<session>\noi, responde so com: ok um\n</session>\n\nWrite the title in the predominant language of the session."}]}]}`
	if got := Parse(nil, []byte(body)).Title; got != "oi, responde so com: ok um" {
		t.Errorf("title = %q", got)
	}
	// titles stored by older versions, cut mid-tag
	for stored, want := range map[string]string{
		"<session> Hey mate, how we doing today? </session…":             "Hey mate, how we doing today?",
		"<session> Eaiii meu, belezinha? </session> Write the title in…": "Eaiii meu, belezinha?",
		"<session> Hey mate, how we doing today? </sessi…":               "Hey mate, how we doing today?",
		"plain old title": "plain old title",
	} {
		if got := CleanTitle(stored); got != want {
			t.Errorf("CleanTitle(%q) = %q, want %q", stored, got, want)
		}
	}
}
