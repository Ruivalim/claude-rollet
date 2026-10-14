package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/ruivalim/claude-rollet/internal/codex"
)

func cmdCodex(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: rollet codex login|logout|status")
	}
	auth := codex.NewAuth(codex.DefaultPath())
	switch args[0] {
	case "login":
		return codexLogin()
	case "logout":
		if err := auth.Logout(); err != nil {
			return err
		}
		ok("Signed out. Codex providers fail until you log in again.")
		return nil
	case "status":
		t, err := auth.Status()
		if err != nil {
			return err
		}
		ok("Signed in to ChatGPT (account %s)", t.AccountID)
		if left := time.Until(t.Expires); left > 0 {
			hint("Access token renews itself; current one valid for %s more.", left.Round(time.Minute))
		} else {
			hint("Access token expired; it renews on the next request.")
		}
		return nil
	default:
		return fmt.Errorf("unknown codex command %q", args[0])
	}
}

// codexLogin runs the device flow: show a code, wait for the browser.
func codexLogin() error {
	auth := codex.NewAuth(codex.DefaultPath())
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	dc, err := auth.StartDeviceLogin(ctx)
	if err != nil {
		return err
	}
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(accent).Padding(0, 2)
	fmt.Println(box.Render(
		"Open  " + sBold.Render(codex.VerificationURL) + "\n" +
			"Code  " + sTitle.Render(dc.UserCode)))
	openBrowser(codex.VerificationURL)
	fmt.Println(sMuted.Render("Waiting for you to approve it (Ctrl+C cancels)…"))

	t, err := auth.FinishDeviceLogin(ctx, dc)
	if err != nil {
		return err
	}
	ok("Signed in to ChatGPT (account %s). Stored in %s", t.AccountID, codex.DefaultPath())
	return nil
}

// openBrowser is best effort: on a headless box the printed URL is enough.
func openBrowser(url string) {
	if !interactive() {
		return
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return
		}
		cmd = exec.Command("xdg-open", url)
	default:
		return
	}
	_ = cmd.Start()
}
