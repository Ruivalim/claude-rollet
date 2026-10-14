// Package store keeps the session -> provider log in a single JSON file.
//
// Every operation is read-modify-write under an flock, so the server and a
// `rollet guess` run from another terminal never overwrite each other.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Session struct {
	ID       string    `json:"id"`
	Provider string    `json:"provider"`
	Model    string    `json:"model,omitempty"` // upstream model actually used
	CWD      string    `json:"cwd,omitempty"`
	Title    string    `json:"title,omitempty"` // first user prompt, trimmed
	Created  time.Time `json:"created"`
	LastSeen time.Time `json:"last_seen"`
	Requests int       `json:"requests"`

	Guess     string     `json:"guess,omitempty"`
	GuessedAt *time.Time `json:"guessed_at,omitempty"`
	// Revealed flips once you look. A guess made after that proves nothing,
	// so guessing is refused from then on.
	Revealed bool `json:"revealed,omitempty"`
}

// Info is what the proxy learns from a request. Empty fields never overwrite
// filled ones.
type Info struct {
	CWD   string
	Title string
}

type Store struct {
	path string
	mu   sync.Mutex
}

var ErrNotFound = errors.New("session not found")

func New(path string) *Store { return &Store{path: path} }

func (s *Store) Path() string { return s.path }

// Touch returns the session, creating it with roll() when it is new. The roll
// happens under the lock, so two concurrent first requests of the same session
// always agree on the provider.
func (s *Store) Touch(id string, info Info, roll func() (provider, model string, err error)) (Session, bool, error) {
	var out Session
	var isNew bool
	err := s.update(func(m map[string]*Session) error {
		now := time.Now().UTC()
		sess, ok := m[id]
		if !ok {
			prov, model, err := roll()
			if err != nil {
				return err
			}
			sess = &Session{ID: id, Provider: prov, Model: model, Created: now}
			m[id] = sess
			isNew = true
		}
		if sess.CWD == "" {
			sess.CWD = info.CWD
		}
		if sess.Title == "" {
			sess.Title = info.Title
		}
		sess.LastSeen = now
		sess.Requests++
		out = *sess
		return nil
	})
	return out, isNew, err
}

// Lookup returns the session without counting it as a request.
func (s *Store) Lookup(id string) (Session, bool, error) {
	all, err := s.load()
	if err != nil {
		return Session{}, false, err
	}
	sess, ok := all[id]
	if !ok {
		return Session{}, false, nil
	}
	return *sess, true, nil
}

// List returns every session, most recent first.
func (s *Store) List() ([]Session, error) {
	all, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make([]Session, 0, len(all))
	for _, v := range all {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	return out, nil
}

// Find resolves a unique id prefix.
func (s *Store) Find(prefix string) (Session, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return Session{}, errors.New("empty session id")
	}
	all, err := s.load()
	if err != nil {
		return Session{}, err
	}
	var hits []*Session
	for id, v := range all {
		if strings.HasPrefix(id, prefix) {
			hits = append(hits, v)
		}
	}
	switch len(hits) {
	case 0:
		return Session{}, fmt.Errorf("%w: %s", ErrNotFound, prefix)
	case 1:
		return *hits[0], nil
	default:
		return Session{}, fmt.Errorf("prefix %q matches %d sessions, type more characters", prefix, len(hits))
	}
}

var ErrRevealed = errors.New("session already revealed, a guess now would not count")

func (s *Store) SetGuess(id, guess string) (Session, error) {
	var out Session
	err := s.update(func(m map[string]*Session) error {
		sess, ok := m[id]
		if !ok {
			return fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		if sess.Revealed {
			return ErrRevealed
		}
		now := time.Now().UTC()
		sess.Guess = guess
		sess.GuessedAt = &now
		out = *sess
		return nil
	})
	return out, err
}

func (s *Store) MarkRevealed(id string) (Session, error) {
	var out Session
	err := s.update(func(m map[string]*Session) error {
		sess, ok := m[id]
		if !ok {
			return fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		sess.Revealed = true
		out = *sess
		return nil
	})
	return out, err
}

func (s *Store) update(fn func(map[string]*Session) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	m, err := s.read()
	if err != nil {
		return err
	}
	if err := fn(m); err != nil {
		return err
	}
	return s.write(m)
}

func (s *Store) load() (map[string]*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	return s.read()
}

func (s *Store) lock() (func(), error) {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(s.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
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

func (s *Store) read() (map[string]*Session, error) {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]*Session{}, nil
	}
	if err != nil {
		return nil, err
	}
	var list []*Session
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, fmt.Errorf("%s is corrupted: %w", s.path, err)
		}
	}
	m := make(map[string]*Session, len(list))
	for _, v := range list {
		m[v.ID] = v
	}
	return m, nil
}

// write goes through a temp file + rename so a crash mid-write never leaves
// half a JSON behind.
func (s *Store) write(m map[string]*Session) error {
	list := make([]*Session, 0, len(m))
	for _, v := range m {
		list = append(list, v)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Created.Before(list[j].Created) })
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".sessions-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}
