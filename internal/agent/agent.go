// Package agent manages the launchd LaunchAgent that runs wgh's background
// poller (`wgh __poll`). Opening wgh installs and loads it once; from then on
// launchd keeps it alive across logins and reboots, so desktop notifications
// arrive even when no window is open. `wgh stop` tears it down.
package agent

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Label is the launchd job label; it also names the plist file.
const Label = "com.watchgh.poller"

// Running reports whether the LaunchAgent is currently loaded.
func Running() bool {
	return exec.Command("launchctl", "list", Label).Run() == nil
}

// Ensure installs and loads the agent if it isn't already running. It is
// idempotent and safe to call on every launch; a running agent is left alone.
func Ensure() error {
	if Running() {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating the wgh binary: %w", err)
	}
	plist, err := writePlist(exe)
	if err != nil {
		return err
	}
	_ = bootout() // clear any stale registration before (re)loading
	return bootstrap(plist)
}

// Stop unloads the agent and removes its plist, so it stays down until the next
// launch re-installs it. A not-running agent is not an error.
func Stop() error {
	_ = bootout()
	if err := os.Remove(plistPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing the LaunchAgent plist: %w", err)
	}
	return nil
}

// domain is the per-user launchd domain target (gui/<uid>).
func domain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }

func plistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", Label+".plist")
}

func logPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "watchgh-poller.log")
	}
	return filepath.Join(dir, "watchgh", "poller.log")
}

// writePlist renders the LaunchAgent plist for exe and returns its path.
func writePlist(exe string) (string, error) {
	path := plistPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("creating LaunchAgents dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(renderPlist(exe, logPath())), 0o644); err != nil {
		return "", fmt.Errorf("writing the LaunchAgent plist: %w", err)
	}
	return path, nil
}

// renderPlist builds the LaunchAgent plist XML for the poller. The explicit PATH
// lets the launchd-spawned poller find `gh` (token) and `terminal-notifier`/
// `open`, which a bare launchd environment omits. exe and log are XML-escaped so
// a path containing '&' or another metacharacter can't produce a malformed plist.
func renderPlist(exe, log string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>__poll</string>
    </array>
    <key>RunAtLoad</key><true/>
    <key>KeepAlive</key><true/>
    <key>ProcessType</key><string>Background</string>
    <key>StandardOutPath</key><string>%s</string>
    <key>StandardErrorPath</key><string>%s</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key><string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string>
    </dict>
</dict>
</plist>
`, Label, xmlEscape(exe), xmlEscape(log), xmlEscape(log))
}

// xmlEscape renders s as escaped XML character data (&, <, >, quotes).
func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// bootstrap loads the plist into the user domain, falling back to the legacy
// verb on the odd system where bootstrap balks.
func bootstrap(plist string) error {
	if out, err := run("launchctl", "bootstrap", domain(), plist); err == nil {
		return nil
	} else if out2, err2 := run("launchctl", "load", "-w", plist); err2 == nil {
		return nil
	} else {
		return fmt.Errorf("loading the LaunchAgent failed: %v (%s)", err, out2+out)
	}
}

// bootout unloads the agent; errors are ignored by callers (it may not be up).
func bootout() error {
	if _, err := run("launchctl", "bootout", domain()+"/"+Label); err == nil {
		return nil
	}
	_, err := run("launchctl", "unload", "-w", plistPath())
	return err
}

func run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}
