// Package service installs rollet as a user service: a systemd user unit on
// Linux, a LaunchAgent on macOS. No root needed on either.
package service

import (
	"errors"
	"fmt"
	"html"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

const (
	unitName = "claude-rollet.service"
	label    = "dev.rollet.claude"
)

type Spec struct {
	Binary string            // absolute path to the rollet binary
	Env    map[string]string // ROLLET_HOME and friends, if set
}

// Manager is the platform-specific half.
type Manager interface {
	Path() string
	Render(Spec) string
	Install(Spec) error
	Uninstall() error
	Restart() error
	Status() error
	Logs(follow bool) error
}

func For() (Manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	switch runtime.GOOS {
	case "linux":
		return &systemd{home: home}, nil
	case "darwin":
		return &launchd{home: home}, nil
	default:
		return nil, fmt.Errorf("no service support for %s, run `rollet serve` yourself", runtime.GOOS)
	}
}

// Binary resolves the running executable, refusing `go run` temp builds:
// a service pointing into /tmp dies with the next reboot or cache clean.
func Binary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	if strings.Contains(exe, "go-build") || strings.HasPrefix(exe, os.TempDir()) {
		return "", fmt.Errorf("%s looks like a temporary build; install rollet first (make install or go install) and run this from the installed binary", exe)
	}
	return exe, nil
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func write(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// --- systemd ---

type systemd struct{ home string }

func (s *systemd) Path() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		base = filepath.Join(s.home, ".config")
	}
	return filepath.Join(base, "systemd", "user", unitName)
}

func (s *systemd) Render(sp Spec) string {
	var env strings.Builder
	for _, k := range sortedKeys(sp.Env) {
		fmt.Fprintf(&env, "Environment=%s\n", systemdQuote(k+"="+sp.Env[k]))
	}
	return fmt.Sprintf(`[Unit]
Description=Claude Rollet: blind roulette proxy for Claude Code
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%s serve
Restart=on-failure
RestartSec=2
%s
[Install]
WantedBy=default.target
`, systemdQuote(sp.Binary), env.String())
}

func (s *systemd) Install(sp Spec) error {
	if err := write(s.Path(), s.Render(sp)); err != nil {
		return err
	}
	if err := run("systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	return run("systemctl", "--user", "enable", "--now", unitName)
}

func (s *systemd) Uninstall() error {
	_ = run("systemctl", "--user", "disable", "--now", unitName)
	if err := os.Remove(s.Path()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return run("systemctl", "--user", "daemon-reload")
}

func (s *systemd) Restart() error { return run("systemctl", "--user", "restart", unitName) }
func (s *systemd) Status() error {
	return run("systemctl", "--user", "--no-pager", "status", unitName)
}
func (s *systemd) Logs(follow bool) error {
	args := []string{"--user", "-u", unitName, "--no-pager", "-n", "100"}
	if follow {
		args = append(args, "-f")
	}
	return run("journalctl", args...)
}

func systemdQuote(s string) string {
	if !strings.ContainsAny(s, " \t\"'\\") {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// --- launchd ---

type launchd struct{ home string }

func (l *launchd) Path() string {
	return filepath.Join(l.home, "Library", "LaunchAgents", label+".plist")
}

func (l *launchd) logPath() string {
	return filepath.Join(l.home, "Library", "Logs", "claude-rollet.log")
}

func (l *launchd) Render(sp Spec) string {
	var env strings.Builder
	if len(sp.Env) > 0 {
		env.WriteString("  <key>EnvironmentVariables</key>\n  <dict>\n")
		for _, k := range sortedKeys(sp.Env) {
			fmt.Fprintf(&env, "    <key>%s</key><string>%s</string>\n", html.EscapeString(k), html.EscapeString(sp.Env[k]))
		}
		env.WriteString("  </dict>\n")
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>serve</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
%s</dict>
</plist>
`, label, html.EscapeString(sp.Binary), html.EscapeString(l.logPath()), html.EscapeString(l.logPath()), env.String())
}

func (l *launchd) domain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }

func (l *launchd) Install(sp Spec) error {
	_ = run("launchctl", "bootout", l.domain()+"/"+label) // reinstall cleanly
	if err := write(l.Path(), l.Render(sp)); err != nil {
		return err
	}
	return run("launchctl", "bootstrap", l.domain(), l.Path())
}

func (l *launchd) Uninstall() error {
	_ = run("launchctl", "bootout", l.domain()+"/"+label)
	if err := os.Remove(l.Path()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (l *launchd) Restart() error {
	return run("launchctl", "kickstart", "-k", l.domain()+"/"+label)
}
func (l *launchd) Status() error { return run("launchctl", "print", l.domain()+"/"+label) }
func (l *launchd) Logs(follow bool) error {
	args := []string{"-n", "100"}
	if follow {
		args = append(args, "-f")
	}
	return run("tail", append(args, l.logPath())...)
}

func sortedKeys(m map[string]string) []string { return slices.Sorted(maps.Keys(m)) }
