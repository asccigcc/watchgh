// Package notify sends native macOS desktop notifications.
//
// It prefers terminal-notifier (which supports click-to-open a URL) and falls
// back to the built-in osascript when it isn't installed.
package notify

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// sendTimeout bounds the notifier subprocess so a wedged helper can't block the
// poll loop that fires notifications.
const sendTimeout = 10 * time.Second

// Send posts a desktop notification. url is opened on click when supported.
// Failures are best-effort and returned for optional logging.
func Send(title, message, url string) error {
	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()
	if path, err := exec.LookPath("terminal-notifier"); err == nil {
		args := []string{"-title", title, "-message", message}
		if url != "" {
			args = append(args, "-open", url)
		}
		return exec.CommandContext(ctx, path, args...).Run()
	}
	return exec.CommandContext(ctx, "osascript", "-e", osaScript(title, message)).Run()
}

// HasGrouping reports whether terminal-notifier is installed — the notifier that
// supports -group, which replaces a prior banner in place so a counter can tick
// 2 → 3 rather than stack. Without it, callers fall back to a single, plain
// osascript banner (which can neither replace nor be removed).
func HasGrouping() bool {
	_, err := exec.LookPath("terminal-notifier")
	return err == nil
}

// SendGroup posts a notification tagged with group so that re-posting to the same
// group replaces the previous banner in place — how a backlog counter updates
// without piling up duplicate banners. Requires terminal-notifier; guard with
// HasGrouping (it returns the lookup error otherwise).
func SendGroup(title, message, group string) error {
	path, err := exec.LookPath("terminal-notifier")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()
	return exec.CommandContext(ctx, path,
		"-title", title, "-message", message, "-group", group).Run()
}

// RemoveGroup clears any delivered notification for group (terminal-notifier
// -remove), so a backlog that has cleared doesn't leave a stale count sitting in
// Notification Center. Requires terminal-notifier; guard with HasGrouping.
func RemoveGroup(group string) error {
	path, err := exec.LookPath("terminal-notifier")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()
	return exec.CommandContext(ctx, path, "-remove", group).Run()
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
