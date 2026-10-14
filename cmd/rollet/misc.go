package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/huh"

	"github.com/ruivalim/claude-rollet/internal/config"
	"github.com/ruivalim/claude-rollet/internal/proxy"
	"github.com/ruivalim/claude-rollet/internal/service"
)

func cmdInit(args []string) error {
	if _, err := os.Stat(configPath()); err == nil {
		hint("Config already exists at %s", configPath())
		hint("Add providers with `rollet provider add`, see them with `rollet provider list`.")
		return nil
	}
	cfg := &config.Config{Listen: config.DefaultListen}
	withClaude := true
	odds := "0.5"
	if interactive() {
		fmt.Println(sTitle.Render("rollet") + sMuted.Render(" · let's set up the roulette"))
		fmt.Println()
		if err := huh.NewForm(huh.NewGroup(
			huh.NewInput().Title("Listen address").Value(&cfg.Listen).Validate(func(s string) error {
				_, _, err := net.SplitHostPort(s)
				return err
			}),
			huh.NewConfirm().Title("Put Claude itself in the roulette?").
				Description("Uses your Claude Code login. Without it there's nothing to compare against.").
				Value(&withClaude),
			huh.NewInput().Title("Chance of landing on Claude").
				Description("0.5 = half the sessions; lower it to see the others more often. 'off' = by weight only").
				Value(&odds).Validate(func(s string) error { _, err := parseOdds(s); return err }),
		)).WithTheme(theme()).Run(); err != nil {
			return err
		}
	}
	cfg.ClaudeOdds, _ = parseOdds(odds)
	if withClaude {
		cfg.Providers = append(cfg.Providers, config.Provider{Name: "claude", Type: config.TypeAnthropic, BaseURL: "https://api.anthropic.com"})
	}
	if err := cfg.Save(configPath()); err != nil {
		return err
	}
	ok("Config written to %s", configPath())

	if interactive() {
		add := true
		if err := huh.NewForm(huh.NewGroup(huh.NewConfirm().Title("Add another provider now?").Value(&add))).WithTheme(theme()).Run(); err != nil {
			return err
		}
		for add {
			if err := cmdProviderAdd(nil); err != nil {
				return err
			}
			if err := huh.NewForm(huh.NewGroup(huh.NewConfirm().Title("Add one more?").Value(&add))).WithTheme(theme()).Run(); err != nil {
				return err
			}
		}
	}
	fmt.Println()
	fmt.Println(sBold.Render("Next"))
	fmt.Println("  rollet service install     " + sMuted.Render("keep the proxy running 24/7"))
	fmt.Println("  rollet env >> ~/.zshrc     " + sMuted.Render("every claude goes through the roulette"))
	fmt.Println("  rollet claude              " + sMuted.Render("or opt in one session at a time"))
	return nil
}

func cmdEnv(args []string) error {
	fs := flag.NewFlagSet("env", flag.ContinueOnError)
	shell := fs.String("shell", filepath.Base(os.Getenv("SHELL")), "zsh, bash, fish or nu")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, _ := loadConfig()
	line, err := envLine(*shell, proxyURL(cfg))
	if err != nil {
		return err
	}
	fmt.Println(line)
	return nil
}

func envLine(shell, url string) (string, error) {
	switch shell {
	case "zsh", "bash", "sh", "ksh", "dash", "":
		return fmt.Sprintf("export ANTHROPIC_BASE_URL=%s  # claude-rollet", url), nil
	case "fish":
		return fmt.Sprintf("set -gx ANTHROPIC_BASE_URL %s  # claude-rollet", url), nil
	case "nu":
		return fmt.Sprintf("$env.ANTHROPIC_BASE_URL = %q  # claude-rollet", url), nil
	default:
		return "", fmt.Errorf("don't know how to write env for %q, use --shell zsh|bash|fish|nu", shell)
	}
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen", "", "address to listen on (default from config)")
	odds := fs.String("claude-odds", "", "override claude_odds for this run (0.2, 20%, off)")
	reveal := fs.Bool("reveal", false, "log which provider each session landed on")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	logger := log.New(os.Stderr, "rollet ", log.LstdFlags)
	srv := proxy.New(cfg, openStore(), logger)
	srv.Reveal = *reveal
	if *odds != "" {
		v, err := parseOdds(*odds)
		if err != nil {
			return err
		}
		srv.OddsOverride, srv.OddsOff = v, v == nil
	}
	srv.WatchConfig(configPath())
	cfg = srv.Config()
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("%s: %w", configPath(), err)
	}
	addr := cfg.Listen
	if *listen != "" {
		addr = *listen
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	hs := &http.Server{Handler: srv, ReadHeaderTimeout: 30 * time.Second}

	var names []string
	for _, p := range cfg.Providers {
		if p.W() > 0 {
			names = append(names, p.Name)
		}
	}
	logger.Printf("listening on http://%s · %d providers in the roll: %s", ln.Addr(), len(names), strings.Join(names, ", "))
	if cfg.ClaudeOdds != nil {
		logger.Printf("claude_odds %s", pct(*cfg.ClaudeOdds))
	}
	if !*reveal {
		logger.Printf("blind mode: providers are hidden in this log (serve --reveal to show)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	logger.Printf("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return hs.Shutdown(shutCtx)
}

func cmdService(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: rollet service install|uninstall|restart|status|logs [-f]")
	}
	m, err := service.For()
	if err != nil {
		return err
	}
	switch args[0] {
	case "install":
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		if err := cfg.Validate(); err != nil {
			return fmt.Errorf("fix the config before installing: %w", err)
		}
		for _, p := range cfg.Providers {
			if p.APIKeyEnv != "" {
				warn("%s reads $%s, which the service won't see. Edit it to store the key in the config: rollet provider edit %s", p.Name, p.APIKeyEnv, p.Name)
			}
		}
		bin, err := service.Binary()
		if err != nil {
			return err
		}
		spec := service.Spec{Binary: bin, Env: map[string]string{}}
		if h := os.Getenv("ROLLET_HOME"); h != "" {
			spec.Env["ROLLET_HOME"] = h
		} else {
			// Pin the home the CLI uses now, so a later XDG change can't
			// split the service and the CLI across two configs.
			spec.Env["ROLLET_HOME"] = homeOrDefault()
		}
		if err := m.Install(spec); err != nil {
			return err
		}
		ok("Installed %s", m.Path())
		if err := waitHealthy(cfg, 5*time.Second); err != nil {
			warn("%v; check `rollet service logs`", err)
			return nil
		}
		ok("Proxy answering on %s", proxyURL(cfg))
		if runtime.GOOS == "linux" {
			hint("To keep it running while you're logged out: loginctl enable-linger $USER")
		}
		return nil
	case "uninstall":
		if err := m.Uninstall(); err != nil {
			return err
		}
		ok("Service removed. If you exported ANTHROPIC_BASE_URL, remove that line too or Claude Code will fail to connect.")
		return nil
	case "restart":
		return m.Restart()
	case "status":
		return m.Status()
	case "logs":
		return m.Logs(len(args) > 1 && (args[1] == "-f" || args[1] == "--follow"))
	case "print":
		bin, err := service.Binary()
		if err != nil {
			bin = "/path/to/rollet"
		}
		fmt.Print(m.Render(service.Spec{Binary: bin, Env: map[string]string{"ROLLET_HOME": homeOrDefault()}}))
		return nil
	default:
		return fmt.Errorf("unknown service command %q", args[0])
	}
}

func waitHealthy(cfg *config.Config, d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		if err := requireProxy(cfg); err == nil {
			return nil
		} else if time.Now().After(deadline) {
			return err
		}
		time.Sleep(250 * time.Millisecond)
	}
}
