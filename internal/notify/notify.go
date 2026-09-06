// Package notify sends native macOS desktop notifications.
//
// It prefers terminal-notifier (which supports click-to-open a URL) and falls
// back to the built-in osascript when it isn't installed.
package notify

import (
	"fmt"
	"os/exec"
)

// Send posts a desktop notification. url is opened on click when supported.
// Failures are best-effort and returned for optional logging.
func Send(title, message, url string) error {
	if path, err := exec.LookPath("terminal-notifier"); err == nil {
		args := []string{"-title", title, "-message", message}
		if url != "" {
			args = append(args, "-open", url)
		}
		return exec.Command(path, args...).Run()
	}
	script := fmt.Sprintf("display notification %q with title %q", message, title)
	return exec.Command("osascript", "-e", script).Run()
}
