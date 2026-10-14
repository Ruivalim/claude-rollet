package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"

	"github.com/ruivalim/claude-rollet/internal/adapters"
	"github.com/ruivalim/claude-rollet/internal/codex"
	"github.com/ruivalim/claude-rollet/internal/config"
	"github.com/ruivalim/claude-rollet/internal/probe"
)

const (
	choiceClaude          = "claude"
	choiceAnthropicCompat = "custom-anthropic"
)

func cmdProviderAdd(args []string) error {
	if !interactive() {
		return errors.New("provider add is interactive, run it in a terminal (or edit config.json directly)")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	opts := []huh.Option[string]{huh.NewOption("Claude (Anthropic API, your Claude Code login)", choiceClaude)}
	for _, a := range adapters.All() {
		opts = append(opts, huh.NewOption(a.Label, a.Type))
	}
	opts = append(opts, huh.NewOption("Any Anthropic-compatible endpoint (no translation)", choiceAnthropicCompat))

	var choice string
	if err := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().Title("What are you adding?").
			Description("Missing a provider? It's one file in internal/adapters, PRs welcome.").
			Options(opts...).Value(&choice).Height(len(opts) + 2),
	)).WithTheme(theme()).Run(); err != nil {
		return err
	}

	var p config.Provider
	switch choice {
	case choiceClaude:
		p = config.Provider{Name: "claude", Type: config.TypeAnthropic, BaseURL: "https://api.anthropic.com"}
	case choiceAnthropicCompat:
		p = config.Provider{Type: config.TypeAnthropic}
	default:
		a, _ := adapters.Get(choice)
		p = config.Provider{Name: a.Type, Type: a.Type, BaseURL: a.BaseURL}
		if a.Type == adapters.Generic {
			p.Name = ""
		}
	}
	if _, taken := cfg.Provider(p.Name); taken != nil {
		p.Name = ""
	}
	return providerForm(cfg, p, false)
}

func cmdProviderEdit(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: rollet provider edit <name>")
	}
	if !interactive() {
		return errors.New("provider edit is interactive, run it in a terminal")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	_, p := cfg.Provider(args[0])
	if p == nil {
		return fmt.Errorf("no provider named %q", args[0])
	}
	return providerForm(cfg, *p, true)
}

// providerForm drives add and edit: fill, test through the real translation,
// save only if it works (or you insist).
func providerForm(cfg *config.Config, p config.Provider, editing bool) error {
	original := p.Name
	keyField := p.APIKey
	if p.APIKeyEnv != "" {
		keyField = "$" + p.APIKeyEnv
	}
	keyHint := "paste the key, or $VAR to read it from an env var (services don't see your shell env)"
	if p.Type == config.TypeAnthropic {
		keyHint = "leave empty to reuse Claude Code's own login"
	}

	nameInput := huh.NewInput().Title("Name").Description("shows up in reveal and stats").Value(&p.Name).Validate(func(s string) error {
		s = strings.TrimSpace(s)
		if s == "" {
			return errors.New("required")
		}
		if strings.ContainsAny(s, " \t") {
			return errors.New("no spaces")
		}
		if _, other := cfg.Provider(s); other != nil && s != original {
			return errors.New("already taken")
		}
		return nil
	})
	if editing {
		nameInput = nameInput.Description("can't be renamed: past sessions point to it")
	}
	adapter, _ := adapters.Get(p.Type)
	group := []huh.Field{huh.NewInput().Title("Base URL").Value(&p.BaseURL).Validate(validURL)}
	if !adapter.OAuth {
		group = append(group, huh.NewInput().Title("API key").Description(keyHint).EchoMode(huh.EchoModePassword).Value(&keyField).Validate(func(s string) error {
			if p.Translated() && strings.TrimSpace(s) == "" {
				return errors.New("required")
			}
			if v, ok := strings.CutPrefix(strings.TrimSpace(s), "$"); ok && os.Getenv(v) == "" {
				return fmt.Errorf("$%s is empty in this shell", v)
			}
			return nil
		}))
	}
	if !editing {
		group = append([]huh.Field{nameInput}, group...)
	}
	if err := huh.NewForm(huh.NewGroup(group...)).WithTheme(theme()).Run(); err != nil {
		return err
	}
	p.Name = strings.TrimSpace(p.Name)
	p.BaseURL = strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	p.APIKey, p.APIKeyEnv = "", ""
	if v, ok := strings.CutPrefix(strings.TrimSpace(keyField), "$"); ok {
		p.APIKeyEnv = v
	} else {
		p.APIKey = strings.TrimSpace(keyField)
	}
	if adapter.OAuth {
		if _, err := codex.NewAuth(codex.DefaultPath()).Status(); err != nil {
			fmt.Println(sMuted.Render("This provider uses your ChatGPT subscription. Let's sign in first."))
			if err := codexLogin(); err != nil {
				return err
			}
		}
	}

	if err := modelForm(&p); err != nil {
		return err
	}

	results, tested, err := runProbe(p)
	if err != nil {
		return err
	}
	if tested && !allOK(results) {
		save := false
		if err := huh.NewForm(huh.NewGroup(huh.NewConfirm().
			Title("Some checks failed. Save anyway?").
			Description("Sessions that land here may break mid-conversation.").
			Affirmative("Save").Negative("Discard").Value(&save))).WithTheme(theme()).Run(); err != nil {
			return err
		}
		if !save {
			hint("Nothing saved.")
			return nil
		}
	}

	if i, _ := cfg.Provider(original); editing && i >= 0 {
		cfg.Providers[i] = p
	} else {
		cfg.Providers = append(cfg.Providers, p)
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("not saved: %w", err)
	}
	if err := cfg.Save(configPath()); err != nil {
		return err
	}
	ok("Saved %s. A running proxy picks it up on its own.", sBold.Render(p.Name))
	fmt.Println()
	return printProviders(cfg)
}

func modelForm(p *config.Provider) error {
	weight := strconv.Itoa(p.W())
	maxTok := ""
	if p.MaxTokens > 0 {
		maxTok = strconv.Itoa(p.MaxTokens)
	}

	var fields []huh.Field
	if p.Translated() {
		models, err := probe.ListModels(context.Background(), p.BaseURL, p.Key())
		if err != nil || len(models) == 0 {
			a, _ := adapters.Get(p.Type)
			models = a.Models
		}
		if len(models) > 0 {
			const other = "(type another id)"
			if p.Model != "" && !contains(models, p.Model) {
				models = append([]string{p.Model}, models...)
			}
			models = append(models, other)
			if p.Model == "" {
				p.Model = models[0]
			}
			if p.SmallModel == "" {
				p.SmallModel = p.Model
			}
			if err := huh.NewForm(huh.NewGroup(
				huh.NewSelect[string]().Title("Model").Description("/ to filter").
					Options(huh.NewOptions(models...)...).Filtering(true).Height(min(len(models)+2, 12)).Value(&p.Model),
				huh.NewSelect[string]().Title("Small model").Description("used where Claude Code asks for haiku (titles, summaries)").
					Options(huh.NewOptions(models...)...).Filtering(true).Height(min(len(models)+2, 8)).Value(&p.SmallModel),
			)).WithTheme(theme()).Run(); err != nil {
				return err
			}
			if p.Model == other {
				p.Model = ""
			}
			if p.SmallModel == other {
				p.SmallModel = ""
			}
		}
		if p.Model == "" {
			fields = append(fields, huh.NewInput().Title("Model").Value(&p.Model).Validate(required))
		}
		if p.SmallModel == "" {
			fields = append(fields, huh.NewInput().Title("Small model").Description("optional, used where Claude Code asks for haiku").Value(&p.SmallModel))
		}
		fields = append(fields, huh.NewInput().Title("Max output tokens").
			Description("Claude Code asks for up to 128k; most providers cap lower. Empty = no cap").
			Value(&maxTok).Validate(optionalInt))
	} else {
		fields = append(fields, huh.NewInput().Title("Model override").
			Description("empty forwards whatever Claude Code asks for (right for Claude itself)").Value(&p.Model))
	}
	fields = append(fields, huh.NewInput().Title("Weight").
		Description("relative chance; with claude_odds set, relative within Claude or within the others. 0 pauses it").
		Value(&weight).Validate(func(s string) error {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err != nil || n < 0 {
			return errors.New("a whole number, 0 or more")
		}
		return nil
	}))

	if err := huh.NewForm(huh.NewGroup(fields...)).WithTheme(theme()).Run(); err != nil {
		return err
	}
	p.Model = strings.TrimSpace(p.Model)
	p.SmallModel = strings.TrimSpace(p.SmallModel)
	if p.SmallModel == p.Model {
		p.SmallModel = ""
	}
	p.MaxTokens, _ = strconv.Atoi(strings.TrimSpace(maxTok))
	w, _ := strconv.Atoi(strings.TrimSpace(weight))
	if w == 1 {
		p.Weight = nil
	} else {
		p.Weight = &w
	}
	return nil
}

func cmdProviderTest(args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	var targets []config.Provider
	switch {
	case len(args) == 1 && args[0] == "--all":
		targets = cfg.Providers
	case len(args) == 1:
		_, p := cfg.Provider(args[0])
		if p == nil {
			return fmt.Errorf("no provider named %q", args[0])
		}
		targets = []config.Provider{*p}
	case len(args) == 0 && interactive():
		var name string
		names := make([]string, len(cfg.Providers))
		for i, p := range cfg.Providers {
			names[i] = p.Name
		}
		if err := huh.NewForm(huh.NewGroup(huh.NewSelect[string]().Title("Test which provider?").
			Options(huh.NewOptions(names...)...).Value(&name))).WithTheme(theme()).Run(); err != nil {
			return err
		}
		_, p := cfg.Provider(name)
		targets = []config.Provider{*p}
	default:
		return errors.New("usage: rollet provider test <name> | --all")
	}
	failed := 0
	for _, p := range targets {
		results, tested, err := runProbe(p)
		if err != nil {
			warn("%s: %v", p.Name, err)
			failed++
			continue
		}
		if tested && !allOK(results) {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d provider(s) failed", failed)
	}
	return nil
}

// runProbe reports tested=false when there is nothing it can check from
// here: Claude with no key rides on Claude Code's login, which only Claude
// Code holds.
func runProbe(p config.Provider) ([]probe.Result, bool, error) {
	if p.Type == config.TypeAnthropic && p.Key() == "" {
		hint("%s forwards Claude Code's own login, nothing to test from here.", p.Name)
		return nil, false, nil
	}
	if err := (&config.Config{Providers: []config.Provider{p}}).Validate(); err != nil {
		return nil, false, err
	}
	fmt.Println(sBold.Render("Testing "+p.Name) + sMuted.Render(" through the translation layer"))
	if !interactive() {
		res, err := probe.Run(context.Background(), p, func(r probe.Result) { fmt.Println(resultLine(r)) })
		return res, true, err
	}
	m := probeModel{spin: spinner.New(spinner.WithSpinner(spinner.Dot), spinner.WithStyle(sTitle)), pending: probe.Names()}
	prog := tea.NewProgram(m)
	var runErr error
	var results []probe.Result
	go func() {
		results, runErr = probe.Run(context.Background(), p, func(r probe.Result) { prog.Send(r) })
		prog.Send(probeDone{})
	}()
	final, err := prog.Run()
	if err != nil {
		return nil, true, err
	}
	if fm, ok := final.(probeModel); ok && fm.aborted {
		return nil, true, huh.ErrUserAborted
	}
	return results, true, runErr
}

type probeDone struct{}

type probeModel struct {
	spin    spinner.Model
	pending []string
	done    []probe.Result
	aborted bool
}

func (m probeModel) Init() tea.Cmd { return m.spin.Tick }

func (m probeModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case probe.Result:
		m.done = append(m.done, msg)
		if len(m.pending) > 0 {
			m.pending = m.pending[1:]
		}
		return m, nil
	case probeDone:
		m.pending = nil
		return m, tea.Quit
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" || msg.String() == "q" {
			m.aborted = true
			return m, tea.Quit
		}
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m probeModel) View() string {
	var b strings.Builder
	for _, r := range m.done {
		b.WriteString(resultLine(r) + "\n")
	}
	if len(m.pending) > 0 {
		b.WriteString("  " + m.spin.View() + " " + m.pending[0] + "\n")
	}
	return b.String()
}

func resultLine(r probe.Result) string {
	took := sMuted.Render(fmt.Sprintf("%.1fs", r.Took.Seconds()))
	if r.OK {
		return fmt.Sprintf("  %s %s %s  %s", sGood.Render("✓"), r.Name, took, sMuted.Render(r.Detail))
	}
	return fmt.Sprintf("  %s %s %s  %s", sBad.Render("✗"), r.Name, took, sBad.Render(r.Detail))
}

func allOK(rs []probe.Result) bool {
	for _, r := range rs {
		if !r.OK {
			return false
		}
	}
	return len(rs) > 0
}

func cmdProviderList(args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	return printProviders(cfg)
}

func printProviders(cfg *config.Config) error {
	if len(cfg.Providers) == 0 {
		hint("No providers yet: rollet provider add")
		return nil
	}
	odds := cfg.Odds()
	var rows [][]string
	for i, p := range cfg.Providers {
		model := p.Model
		if model == "" {
			model = sMuted.Render("(as requested)")
		}
		key := sGood.Render("ok")
		switch {
		case p.Type == "codex":
			if _, err := codex.NewAuth(codex.DefaultPath()).Status(); err != nil {
				key = sBad.Render("logged out")
			} else {
				key = sGood.Render("chatgpt login")
			}
		case p.Type == config.TypeAnthropic && p.APIKey == "" && p.APIKeyEnv == "":
			key = sMuted.Render("claude login")
		case p.Key() == "":
			key = sBad.Render("missing")
		case p.APIKeyEnv != "":
			key = sGood.Render("$" + p.APIKeyEnv)
		}
		chance := pct(odds[i])
		if p.W() == 0 {
			chance = sMuted.Render("paused")
		}
		rows = append(rows, []string{sBold.Render(p.Name), p.Type, model, strconv.Itoa(p.W()), chance, delayCell(cfg, p), key})
	}
	fmt.Println(table([]string{"NAME", "TYPE", "MODEL", "WEIGHT", "CHANCE", "DELAY first / between", "KEY"}, rows))
	fmt.Println()
	if cfg.ClaudeOdds != nil {
		hint("claude_odds = %s (change with: rollet odds 0.2)", pct(*cfg.ClaudeOdds))
	} else {
		hint("claude_odds off: every provider competes by weight (set with: rollet odds 0.2)")
	}
	if err := cfg.Validate(); err != nil {
		warn("config problem: %v", err)
	}
	return nil
}

func cmdProviderRm(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: rollet provider rm <name>")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	i, p := cfg.Provider(args[0])
	if p == nil {
		return fmt.Errorf("no provider named %q", args[0])
	}
	if interactive() {
		yes := false
		if err := huh.NewForm(huh.NewGroup(huh.NewConfirm().Title("Remove " + p.Name + "?").
			Description("Sessions already routed to it will fail if resumed. Setting weight 0 pauses it instead.").
			Value(&yes))).WithTheme(theme()).Run(); err != nil {
			return err
		}
		if !yes {
			return nil
		}
	}
	cfg.Providers = append(cfg.Providers[:i], cfg.Providers[i+1:]...)
	if err := cfg.Save(configPath()); err != nil {
		return err
	}
	ok("Removed %s.", args[0])
	return nil
}

func cmdOdds(args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return printProviders(cfg)
	}
	v, err := parseOdds(args[0])
	if err != nil {
		return err
	}
	cfg.ClaudeOdds = v
	if err := cfg.Save(configPath()); err != nil {
		return err
	}
	if v == nil {
		ok("claude_odds off.")
	} else {
		ok("Claude now gets %s of new sessions.", pct(*v))
	}
	fmt.Println()
	return printProviders(cfg)
}

func parseOdds(s string) (*float64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "off" || s == "none" {
		return nil, nil
	}
	percent := strings.HasSuffix(s, "%")
	v, err := strconv.ParseFloat(strings.TrimSuffix(s, "%"), 64)
	if err != nil {
		return nil, fmt.Errorf("odds must look like 0.2, 20%% or off, got %q", s)
	}
	if percent {
		v /= 100
	}
	if v < 0 || v > 1 {
		return nil, fmt.Errorf("odds must be between 0 and 1 (or 0%% and 100%%), got %q", s)
	}
	return &v, nil
}

func validURL(s string) error {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("needs to be a full http(s) URL")
	}
	return nil
}

func required(s string) error {
	if strings.TrimSpace(s) == "" {
		return errors.New("required")
	}
	return nil
}

func optionalInt(s string) error {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err != nil || n <= 0 {
		return errors.New("a positive whole number, or empty")
	}
	return nil
}

func delayCell(cfg *config.Config, p config.Provider) string {
	first, between := cfg.DelayFor(p, true), cfg.DelayFor(p, false)
	if first.Zero() && between.Zero() {
		return sMuted.Render("none")
	}
	return first.String() + " / " + between.String()
}

func cmdDelay(args []string) error {
	var provider string
	var rest []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--provider" && i+1 < len(args):
			provider = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--provider="):
			provider = strings.TrimPrefix(args[i], "--provider=")
		default:
			rest = append(rest, args[i])
		}
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return printProviders(cfg)
	}

	target := &cfg.Delay
	label := "every provider"
	if provider != "" {
		_, p := cfg.Provider(provider)
		if p == nil {
			return fmt.Errorf("no provider named %q", provider)
		}
		target, label = &p.Delay, provider
	}

	switch {
	case len(rest) == 1 && rest[0] == "off":
		// Clearing a provider's override falls back to the global delay;
		// to switch a global delay off for one provider, set it to 0.
		*target = nil
	case len(rest) == 2 && (rest[0] == "first" || rest[0] == "between"):
		rg, err := config.ParseRange(rest[1])
		if err != nil {
			return err
		}
		if *target == nil {
			*target = &config.Delay{}
		}
		v := rest[1]
		if rg.Zero() {
			v = "0"
		}
		if rest[0] == "first" {
			(*target).First = v
		} else {
			(*target).Between = v
		}
		if (*target).First == "" && (*target).Between == "" {
			*target = nil
		}
	default:
		return errors.New("usage: rollet delay [first|between] <30s-70s|0> [--provider name]  or  rollet delay off [--provider name]")
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := cfg.Save(configPath()); err != nil {
		return err
	}
	ok("Delay updated for %s.", label)
	fmt.Println()
	return printProviders(cfg)
}
