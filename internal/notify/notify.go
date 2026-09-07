// Package notify sends native macOS desktop notifications.
//
// It prefers terminal-notifier (which supports click-to-open a URL) and falls
// back to the built-in osascript when it isn't installed.
package notify

import (
	"os/exec"
	"strings"
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
	return exec.Command("osascript", "-e", osaScript(title, message)).Run()
}

// osaScript builds the AppleScript for a notification, escaping title and
// message as AppleScript string literals. Without escaping, a value containing a
// double quote would break out of the literal and inject arbitrary AppleScript.
func osaScript(title, message string) string {
	return "display notification " + osaString(message) + " with title " + osaString(title)
}

// osaString renders s as a quoted AppleScript string literal. AppleScript only
// understands \\ and \" as escapes (not Go's \x/\u forms), so we escape those
// and fold newlines to spaces rather than emit a literal Go %q would produce.
func osaString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", " ", "\r", " ")
	return `"` + r.Replace(s) + `"`
}
