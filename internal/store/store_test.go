package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestTouchRollsOnceUnderConcurrency(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	// Two Store values on the same file stand in for two processes: only the
	// flock keeps them honest, not the in-process mutex.
	stores := []*Store{New(path), New(path)}
	var rolls atomic.Int32
	roll := func() (string, string, error) {
		n := rolls.Add(1)
		return fmt.Sprintf("p%d", n), "m", nil
	}

	var wg sync.WaitGroup
	results := make(chan string, 100)
	for i := range 100 {
		wg.Go(func() {
			s, _, err := stores[i%2].Touch("sess-1", Info{CWD: "/x"}, roll)
			if err != nil {
				t.Error(err)
				return
			}
			results <- s.Provider
		})
	}
	wg.Wait()
	close(results)

	if rolls.Load() != 1 {
		t.Fatalf("rolled %d times for one session", rolls.Load())
	}
	for p := range results {
		if p != "p1" {
			t.Fatalf("session landed on %s and p1 at the same time", p)
		}
	}
	s, ok, _ := stores[0].Lookup("sess-1")
	if !ok || s.Requests != 100 {
		t.Fatalf("requests = %d", s.Requests)
	}
}

func TestTouchKeepsFirstInfo(t *testing.T) {
	st := New(filepath.Join(t.TempDir(), "s.json"))
	roll := func() (string, string, error) { return "p", "m", nil }
	_, isNew, _ := st.Touch("a", Info{Title: "first"}, roll)
	if !isNew {
		t.Fatal("expected new")
	}
	s, isNew, _ := st.Touch("a", Info{CWD: "/later", Title: "second"}, roll)
	if isNew || s.Title != "first" || s.CWD != "/later" {
		t.Fatalf("got %+v new=%v", s, isNew)
	}
}

func TestTouchRollErrorStoresNothing(t *testing.T) {
	st := New(filepath.Join(t.TempDir(), "s.json"))
	_, _, err := st.Touch("a", Info{}, func() (string, string, error) { return "", "", errors.New("boom") })
	if err == nil {
		t.Fatal("expected error")
	}
	if _, ok, _ := st.Lookup("a"); ok {
		t.Fatal("failed roll left a session behind")
	}
}

func TestFindAndGuess(t *testing.T) {
	st := New(filepath.Join(t.TempDir(), "s.json"))
	roll := func() (string, string, error) { return "p", "m", nil }
	for _, id := range []string{"abc123", "abd456", "zzz"} {
		_, _, _ = st.Touch(id, Info{}, roll)
	}
	if _, err := st.Find("ab"); err == nil {
		t.Error("ambiguous prefix must fail")
	}
	if _, err := st.Find("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v", err)
	}
	s, err := st.Find("abd")
	if err != nil || s.ID != "abd456" {
		t.Fatalf("got %v %v", s, err)
	}
	if _, err := st.SetGuess("abd456", "deepseek"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetGuess("missing", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v", err)
	}
	s, _ = st.Find("abd")
	if s.Guess != "deepseek" || s.GuessedAt == nil {
		t.Errorf("guess not stored: %+v", s)
	}
}

func TestCorruptedFileIsNotOverwritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	_ = os.WriteFile(path, []byte("{half"), 0o600)
	st := New(path)
	if _, _, err := st.Touch("a", Info{}, func() (string, string, error) { return "p", "", nil }); err == nil {
		t.Fatal("expected error")
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "{half" {
		t.Fatal("corrupted log was replaced, the experiment history is gone")
	}
}

func TestFilePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	st := New(path)
	_, _, _ = st.Touch("a", Info{}, func() (string, string, error) { return "p", "", nil })
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
}

func TestGuessAfterRevealIsRefused(t *testing.T) {
	st := New(filepath.Join(t.TempDir(), "s.json"))
	_, _, _ = st.Touch("a", Info{}, func() (string, string, error) { return "p", "", nil })
	if _, err := st.SetGuess("a", "p"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MarkRevealed("a"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetGuess("a", "q"); !errors.Is(err, ErrRevealed) {
		t.Fatalf("err = %v", err)
	}
	s, _ := st.Find("a")
	if s.Guess != "p" || !s.Revealed {
		t.Errorf("blind guess was lost: %+v", s)
	}
}
