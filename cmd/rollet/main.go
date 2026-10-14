// Command rollet is a local proxy that sends each Claude Code session to a
// randomly chosen model provider, without telling you which one.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/huh"

	"github.com/ruivalim/claude-rollet/internal/config"
	"github.com/ruivalim/claude-rollet/internal/store"
)

var version = "dev"

type command struct {
	name    string
	args    string
	summary string
	run     func(args []string) error
}

type section struct {
	title string
	cmds  []command
}

func sections() []section {
	return []section{
		{"Setup", []command{
			{"init", "", "create the config interactively", cmdInit},
			{"env", "[--shell zsh|bash|fish|nu]", "print the line that points Claude Code at rollet", cmdEnv},
			{"claude", "[args...]", "run claude through rollet, without touching your env", cmdClaude},
		}},
		{"Providers", []command{
			{"provider add", "", "add a provider (form + live translation test)", cmdProviderAdd},
			{"provider edit", "<name>", "edit a provider and test it again", cmdProviderEdit},
			{"provider test", "[name | --all]", "test text, streaming and tool calls through the proxy", cmdProviderTest},
			{"provider list", "", "providers, weights and the real chance of each", cmdProviderList},
			{"provider rm", "<name>", "remove a provider", cmdProviderRm},
			{"codex", "login|logout|status", "sign in with your ChatGPT subscription (for codex providers)", cmdCodex},
			{"odds", "[0.2 | 20% | off]", "show or set the chance of landing on Claude", cmdOdds},
			{"delay", "[first|between] <30s-70s|off> [--provider name]", "fake latency to blur the speed gap between models", cmdDelay},
		}},
		{"Proxy", []command{
			{"serve", "[--claude-odds x] [--reveal]", "run the proxy in the foreground", cmdServe},
			{"service", "<action>", "run 24/7 via systemd/launchd: install, uninstall, restart, status, logs [-f]", cmdService},
		}},
		{"The experiment", []command{
			{"sessions", "[--reveal] [-n N] | rm <id...>", "browse sessions (enter open, g guess, r reveal, d delete)", cmdSessions},
			{"open", "<id>", "cd into the project and resume the session through rollet", cmdOpen},
			{"guess", "<id> [provider]", "record which provider you think it was", cmdGuess},
			{"reveal", "<id>", "show where a session went (and if you got it right)", cmdReveal},
			{"stats", "", "how well can you tell them apart?", cmdStats},
		}},
	}
}

func main() {
	if err := dispatch(os.Args[1:]); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			os.Exit(130)
		}
		fmt.Fprintln(os.Stderr, sBad.Render("error: ")+err.Error())
		os.Exit(1)
	}
}

func dispatch(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		usage()
		return nil
	}
	if args[0] == "version" || args[0] == "--version" {
		fmt.Println("rollet", version)
		return nil
	}
	aliases := map[string]string{"ls": "sessions", "providers": "provider list"}
	if a, ok := aliases[args[0]]; ok {
		args = append(strings.Fields(a), args[1:]...)
	}
	for _, sec := range sections() {
		for _, c := range sec.cmds {
			words := strings.Fields(c.name)
			if len(args) >= len(words) && strings.Join(args[:len(words)], " ") == c.name {
				return c.run(args[len(words):])
			}
		}
	}
	if args[0] == "provider" {
		if len(args) == 1 {
			return cmdProviderList(nil)
		}
		return fmt.Errorf("unknown provider command %q (add, edit, test, list, rm)", args[1])
	}
	return fmt.Errorf("unknown command %q, see `rollet help`", args[0])
}

func usage() {
	fmt.Println(sTitle.Render("rollet") + sMuted.Render(" · blind roulette proxy for Claude Code"))
	fmt.Println()
	width := 0
	for _, sec := range sections() {
		for _, c := range sec.cmds {
			width = max(width, len(c.name)+1+len(c.args))
		}
	}
	for _, sec := range sections() {
		fmt.Println(sBold.Render(sec.title))
		for _, c := range sec.cmds {
			pad := strings.Repeat(" ", width-len(c.name)-1-len(c.args)+2)
			fmt.Println("  " + c.name + " " + sMuted.Render(c.args) + pad + c.summary)
		}
		fmt.Println()
	}
	hint("Config and session log live in %s", homeOrDefault())
}

// --- paths ---

func homeOrDefault() string {
	h, err := config.Home()
	if err != nil {
		return "~/.config/claude-rollet"
	}
	return h
}

func configPath() string   { return filepath.Join(homeOrDefault(), "config.json") }
func sessionsPath() string { return filepath.Join(homeOrDefault(), "sessions.json") }
func openStore() *store.Store {
	return store.New(sessionsPath())
}

// loadConfig reads the config without validating it, so a broken provider can
// still be listed, fixed or removed.
func loadConfig() (*config.Config, error) {
	c, err := config.Load(configPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no config at %s yet, run `rollet init`", configPath())
	}
	return c, err
}

func proxyURL(c *config.Config) string {
	listen := config.DefaultListen
	if c != nil && c.Listen != "" {
		listen = c.Listen
	}
	if strings.HasPrefix(listen, ":") {
		listen = "127.0.0.1" + listen
	}
	return "http://" + listen
}
