package main

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	btable "github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"

	"github.com/ruivalim/claude-rollet/internal/claudecode"
	"github.com/ruivalim/claude-rollet/internal/config"
	"github.com/ruivalim/claude-rollet/internal/store"
)

func cmdSessions(args []string) error {
	fs := flag.NewFlagSet("sessions", flag.ContinueOnError)
	reveal := fs.Bool("reveal", false, "show providers (spoils the experiment)")
	limit := fs.Int("n", 50, "how many sessions")
	if err := fs.Parse(args); err != nil {
		return err
	}
	list, err := openStore().List()
	if err != nil {
		return err
	}
	if len(list) == 0 {
		hint("No sessions yet. Point Claude Code at rollet (rollet env, or rollet claude) and start one.")
		return nil
	}
	if len(list) > *limit {
		list = list[:*limit]
	}
	if !interactive() {
		fmt.Println(sessionsTable(list, *reveal))
		return nil
	}
	return browse(list, *reveal)
}

func providerCell(s store.Session, reveal bool) string {
	if reveal || s.Revealed {
		return s.Provider
	}
	return hiddenProvider
}

func guessCell(s store.Session) string {
	switch {
	case s.Guess == "":
		return "-"
	case !s.Revealed:
		return s.Guess
	case s.Guess == s.Provider:
		return s.Guess + " ✓"
	default:
		return s.Guess + " ✗"
	}
}

func project(cwd string) string {
	if cwd == "" {
		return "?"
	}
	return filepath.Base(cwd)
}

func sessionsTable(list []store.Session, reveal bool) string {
	var rows [][]string
	for _, s := range list {
		prov := providerCell(s, reveal)
		if prov == hiddenProvider {
			prov = sHidden.Render(prov)
		}
		rows = append(rows, []string{shortID(s.ID), ago(s.LastSeen), project(s.CWD), fmt.Sprint(s.Requests), guessCell(s), prov, clip(s.Title, 50)})
	}
	return table([]string{"ID", "LAST", "PROJECT", "REQ", "GUESS", "PROVIDER", "FIRST PROMPT"}, rows)
}

// --- interactive browser ---

type browseAction struct {
	kind string // open, guess, reveal
	id   string
}

type browser struct {
	tbl    btable.Model
	list   []store.Session
	action *browseAction
}

func browse(list []store.Session, reveal bool) error {
	cols := []btable.Column{
		{Title: "ID", Width: 8}, {Title: "LAST", Width: 9}, {Title: "PROJECT", Width: 18},
		{Title: "REQ", Width: 4}, {Title: "GUESS", Width: 12}, {Title: "PROVIDER", Width: 12}, {Title: "FIRST PROMPT", Width: 48},
	}
	rows := make([]btable.Row, len(list))
	for i, s := range list {
		rows[i] = btable.Row{shortID(s.ID), ago(s.LastSeen), clip(project(s.CWD), 18), fmt.Sprint(s.Requests), guessCell(s), providerCell(s, reveal), clip(s.Title, 48)}
	}
	st := btable.DefaultStyles()
	st.Header = st.Header.BorderStyle(lipgloss.NormalBorder()).BorderBottom(true).Bold(true).Foreground(muted)
	st.Selected = st.Selected.Foreground(lipgloss.Color("#FFFFFF")).Background(accent).Bold(false)
	t := btable.New(btable.WithColumns(cols), btable.WithRows(rows), btable.WithFocused(true),
		btable.WithHeight(min(len(rows)+1, 20)), btable.WithStyles(st))

	final, err := tea.NewProgram(browser{tbl: t, list: list}).Run()
	if err != nil {
		return err
	}
	a := final.(browser).action
	if a == nil {
		return nil
	}
	switch a.kind {
	case "open":
		return cmdOpen([]string{a.id})
	case "guess":
		return cmdGuess([]string{a.id})
	case "reveal":
		return cmdReveal([]string{a.id})
	}
	return nil
}

func (b browser) Init() tea.Cmd { return nil }

func (b browser) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, isKey := msg.(tea.KeyMsg); isKey {
		sel := b.tbl.Cursor()
		switch k.String() {
		case "q", "esc", "ctrl+c":
			return b, tea.Quit
		case "enter", "o":
			b.action = &browseAction{"open", b.list[sel].ID}
			return b, tea.Quit
		case "g":
			b.action = &browseAction{"guess", b.list[sel].ID}
			return b, tea.Quit
		case "r":
			b.action = &browseAction{"reveal", b.list[sel].ID}
			return b, tea.Quit
		}
	}
	var cmd tea.Cmd
	b.tbl, cmd = b.tbl.Update(msg)
	return b, cmd
}

func (b browser) View() string {
	help := sMuted.Render("↑/↓ move · enter open · g guess · r reveal · q quit")
	return b.tbl.View() + "\n" + help + "\n"
}

// --- open ---

func cmdOpen(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: rollet open <id> [extra claude args...]")
	}
	sess, err := openStore().Find(args[0])
	if err != nil {
		return err
	}
	cwd := sess.CWD
	if _, tcwd, found := claudecode.Transcript(claudecode.Dir(), sess.ID); !found {
		warn("Claude Code has no transcript for %s (deleted, or a different CLAUDE_CONFIG_DIR?). Trying anyway.", shortID(sess.ID))
	} else if tcwd != "" {
		cwd = tcwd
	}
	if cwd == "" {
		return fmt.Errorf("don't know which directory session %s ran in", shortID(sess.ID))
	}
	if _, err := os.Stat(cwd); err != nil {
		return fmt.Errorf("project directory is gone: %s", cwd)
	}
	cfg, _ := loadConfig()
	if err := requireProxy(cfg); err != nil {
		return err
	}
	if err := os.Chdir(cwd); err != nil {
		return err
	}
	hint("→ %s", cwd)
	return execClaude(cfg, append([]string{"--resume", sess.ID}, args[1:]...))
}

func cmdClaude(args []string) error {
	cfg, _ := loadConfig()
	if err := requireProxy(cfg); err != nil {
		return err
	}
	return execClaude(cfg, args)
}

// execClaude replaces this process with claude, so the terminal, signals and
// exit code all belong to Claude Code.
func execClaude(cfg *config.Config, args []string) error {
	bin, err := exec.LookPath("claude")
	if err != nil {
		return errors.New("claude not found in PATH")
	}
	env := append(os.Environ(), "ANTHROPIC_BASE_URL="+proxyURL(cfg))
	return syscall.Exec(bin, append([]string{"claude"}, args...), env)
}

// requireProxy fails early: without it Claude Code would just retry against
// a dead port and print connection errors.
func requireProxy(cfg *config.Config) error {
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(proxyURL(cfg) + "/health")
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode == 200 {
			return nil
		}
	}
	return fmt.Errorf("rollet isn't answering on %s; start it with `rollet service install` (24/7) or `rollet serve`", proxyURL(cfg))
}

// --- guess / reveal / stats ---

func cmdGuess(args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return errors.New("usage: rollet guess <id> [provider]")
	}
	st := openStore()
	sess, err := st.Find(args[0])
	if err != nil {
		return err
	}
	if sess.Revealed {
		return store.ErrRevealed
	}
	names := providerNames(sess)
	var guess string
	if len(args) == 2 {
		guess = args[1]
		if !contains(names, guess) {
			return fmt.Errorf("unknown provider %q (known: %s)", guess, strings.Join(names, ", "))
		}
	} else {
		if !interactive() {
			return errors.New("usage: rollet guess <id> <provider>")
		}
		guess = sess.Guess
		if err := huh.NewForm(huh.NewGroup(huh.NewSelect[string]().
			Title("Who was behind " + shortID(sess.ID) + "?").
			Description(clip(sess.Title, 70)).
			Options(huh.NewOptions(names...)...).Value(&guess))).WithTheme(theme()).Run(); err != nil {
			return err
		}
	}
	if _, err := st.SetGuess(sess.ID, guess); err != nil {
		return err
	}
	ok("Guess recorded: %s. Reveal when you're ready: rollet reveal %s", sBold.Render(guess), shortID(sess.ID))
	return nil
}

func cmdReveal(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: rollet reveal <id>")
	}
	st := openStore()
	sess, err := st.Find(args[0])
	if err != nil {
		return err
	}
	if !sess.Revealed && sess.Guess == "" && interactive() {
		yes := false
		if err := huh.NewForm(huh.NewGroup(huh.NewConfirm().Title("Reveal without guessing first?").
			Description("After the reveal, guesses for this session are locked.").
			Affirmative("Reveal").Negative("Let me guess").Value(&yes))).WithTheme(theme()).Run(); err != nil {
			return err
		}
		if !yes {
			return cmdGuess([]string{sess.ID})
		}
	}
	sess, err = st.MarkRevealed(sess.ID)
	if err != nil {
		return err
	}
	model := sess.Model
	if model == "" {
		model = "as requested by Claude Code"
	}
	fmt.Printf("%s  %s\n", sMuted.Render(shortID(sess.ID)), clip(sess.Title, 70))
	fmt.Printf("It was %s %s\n", sTitle.Render(sess.Provider), sMuted.Render("("+model+")"))
	switch {
	case sess.Guess == "":
	case sess.Guess == sess.Provider:
		fmt.Println(sGood.Render("You guessed " + sess.Guess + ". Right."))
	default:
		fmt.Println(sBad.Render("You guessed " + sess.Guess + ". Wrong."))
	}
	return nil
}

func cmdStats(args []string) error {
	list, err := openStore().List()
	if err != nil {
		return err
	}
	if len(list) == 0 {
		hint("No sessions yet.")
		return nil
	}
	cfg, _ := loadConfig()
	isClaude := func(name string) bool {
		if cfg != nil {
			if _, p := cfg.Provider(name); p != nil {
				return p.IsClaude()
			}
		}
		return strings.Contains(strings.ToLower(name), "claude")
	}

	count := map[string]int{}
	var guessed, exact, binary int
	for _, s := range list {
		count[s.Provider]++
		// Only blind guesses that were later revealed count: an unrevealed
		// guess has no verdict yet, and guessing after a reveal is refused.
		if s.Guess == "" || !s.Revealed {
			continue
		}
		guessed++
		if s.Guess == s.Provider {
			exact++
		}
		if isClaude(s.Guess) == isClaude(s.Provider) {
			binary++
		}
	}

	fmt.Println(sBold.Render(fmt.Sprintf("%d sessions", len(list))))
	names := make([]string, 0, len(count))
	for n := range count {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return count[names[i]] > count[names[j]] })
	var rows [][]string
	for _, n := range names {
		rows = append(rows, []string{n, fmt.Sprint(count[n]), pct(float64(count[n]) / float64(len(list)))})
	}
	fmt.Println(table([]string{"PROVIDER", "SESSIONS", "SHARE"}, rows))
	fmt.Println()

	pendingGuess := 0
	for _, s := range list {
		if s.Guess != "" && !s.Revealed {
			pendingGuess++
		}
	}
	if guessed == 0 {
		hint("No revealed guesses yet. Guess with `rollet guess <id>`, reveal with `rollet reveal <id>`.")
		if pendingGuess > 0 {
			hint("%d guess(es) waiting for a reveal.", pendingGuess)
		}
		return nil
	}
	fmt.Println(sBold.Render("Your guesses") + sMuted.Render(fmt.Sprintf(" (%d revealed)", guessed)))
	fmt.Printf("  exact provider   %s  %s\n", sTitle.Render(pct(float64(exact)/float64(guessed))), sMuted.Render(fmt.Sprintf("%d/%d", exact, guessed)))
	fmt.Printf("  Claude or not?   %s  %s\n", sTitle.Render(pct(float64(binary)/float64(guessed))), sMuted.Render(fmt.Sprintf("%d/%d, a coin flip gets ~50%%", binary, guessed)))
	if pendingGuess > 0 {
		hint("\n%d more guess(es) waiting for a reveal.", pendingGuess)
	}
	return nil
}

func providerNames(sess store.Session) []string {
	var names []string
	if cfg, err := loadConfig(); err == nil {
		for _, p := range cfg.Providers {
			names = append(names, p.Name)
		}
	}
	// Keep the true answer guessable even if it was removed from the config,
	// without hinting which one it is: it just joins the list.
	if !contains(names, sess.Provider) {
		names = append(names, sess.Provider)
		sort.Strings(names)
	}
	return names
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
