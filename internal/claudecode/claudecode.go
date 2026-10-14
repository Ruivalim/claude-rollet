// Package claudecode knows the few things about Claude Code that rollet needs:
// how to read a session out of a request and where its transcript lives.
package claudecode

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// SessionHeader is sent by Claude Code on every request (verified on 2.1.x).
const SessionHeader = "X-Claude-Code-Session-Id"

type requestView struct {
	Metadata struct {
		UserID string `json:"user_id"`
	} `json:"metadata"`
	System     json.RawMessage `json:"system"`
	Messages   []rawMessage    `json:"messages"`
	Safeguards []struct {
		ClassifierContext struct {
			LiveCWD string `json:"live_cwd"`
		} `json:"classifier_context"`
	} `json:"safeguards"`
}

type rawMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type textBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

var (
	legacySessionRe = regexp.MustCompile(`session_([0-9a-fA-F-]{36})`)
	cwdRe           = regexp.MustCompile(`(?m)(?:Primary working directory|Working directory):\s*(\S+)`)
	reminderRe      = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)
)

// Request is what rollet extracts from one /v1/messages call.
type Request struct {
	SessionID string
	CWD       string
	Title     string
}

// Parse never fails: a request it can't read still gets a stable session key,
// so routing keeps working for clients other than Claude Code.
func Parse(h http.Header, body []byte) Request {
	var v requestView
	_ = json.Unmarshal(body, &v)

	r := Request{SessionID: sessionID(h, v)}
	for _, sg := range v.Safeguards {
		if c := sg.ClassifierContext.LiveCWD; c != "" {
			r.CWD = c
			break
		}
	}
	if r.CWD == "" {
		r.CWD = cwdFromText(v)
	}
	r.Title = title(v)
	if r.SessionID == "" {
		r.SessionID = fallbackID(v)
	}
	return r
}

func sessionID(h http.Header, v requestView) string {
	if id := strings.TrimSpace(h.Get(SessionHeader)); id != "" {
		return id
	}
	uid := v.Metadata.UserID
	if strings.HasPrefix(strings.TrimSpace(uid), "{") {
		var meta struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal([]byte(uid), &meta) == nil && meta.SessionID != "" {
			return meta.SessionID
		}
	}
	if m := legacySessionRe.FindStringSubmatch(uid); m != nil {
		return m[1]
	}
	return ""
}

// fallbackID hashes the system prompt and the first user message: both stay
// the same for the whole conversation, so it acts as a sticky key.
func fallbackID(v requestView) string {
	h := sha256.New()
	h.Write(v.System)
	for _, m := range v.Messages {
		if m.Role == "user" {
			h.Write(m.Content)
			break
		}
	}
	return "anon-" + hex.EncodeToString(h.Sum(nil))[:16]
}

func cwdFromText(v requestView) string {
	texts := textsOf(v.System)
	for _, m := range v.Messages {
		texts = append(texts, textsOf(m.Content)...)
	}
	for _, t := range texts {
		if m := cwdRe.FindStringSubmatch(t); m != nil {
			return m[1]
		}
	}
	return ""
}

// title is the first real thing the user typed: Claude Code prepends
// <system-reminder> blocks to the first message, those are skipped.
func title(v requestView) string {
	for _, m := range v.Messages {
		if m.Role != "user" {
			continue
		}
		for _, t := range textsOf(m.Content) {
			t = strings.TrimSpace(reminderRe.ReplaceAllString(t, ""))
			if t == "" {
				continue
			}
			t = strings.Join(strings.Fields(t), " ")
			if r := []rune(t); len(r) > 100 {
				t = string(r[:100]) + "…"
			}
			return t
		}
		return ""
	}
	return ""
}

func textsOf(raw json.RawMessage) []string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil
	}
	if s[0] == '"' {
		var str string
		if json.Unmarshal(raw, &str) == nil {
			return []string{str}
		}
		return nil
	}
	var blocks []textBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	var out []string
	for _, b := range blocks {
		if b.Type == "text" {
			out = append(out, b.Text)
		}
	}
	return out
}

// Transcript finds ~/.claude/projects/<slug>/<id>.jsonl and the cwd recorded
// inside it. That cwd is the authoritative one for `claude --resume`, since
// Claude Code only finds a session from the directory it was started in.
func Transcript(claudeDir, id string) (path, cwd string, ok bool) {
	matches, _ := filepath.Glob(filepath.Join(claudeDir, "projects", "*", id+".jsonl"))
	if len(matches) == 0 {
		return "", "", false
	}
	path = matches[0]
	f, err := os.Open(path)
	if err != nil {
		return path, "", true
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		var line struct {
			CWD string `json:"cwd"`
		}
		if json.Unmarshal(sc.Bytes(), &line) == nil && line.CWD != "" {
			return path, line.CWD, true
		}
	}
	return path, "", true
}

// Dir is Claude Code's config dir, honoring CLAUDE_CONFIG_DIR.
func Dir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}
