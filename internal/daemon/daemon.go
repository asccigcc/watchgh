// Package daemon installs and controls the watchgh background poller as a
// macOS per-user LaunchAgent, so notifications fire without a terminal open.
// A LaunchAgent (not a system LaunchDaemon) runs inside the user's GUI login
// session — that's what lets it post desktop notifications.
package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const label = "com.watchgh"

// PlistPath is where the LaunchAgent definition lives.
func PlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist"), nil
}

// LogPath is where launchd captures the daemon's stdout/stderr.
func LogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Logs", "watchgh.log"), nil
}

// Install writes the LaunchAgent plist pointing at this binary and (re)loads it.
func Install() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	plist, err := PlistPath()
	if err != nil {
		return err
	}
	logPath, err := LogPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(plist, []byte(plistBody(exe, logPath, searchPath(exe))), 0o644); err != nil {
		return err
	}
	// Reload cleanly: bootout any prior instance (ignore "not loaded"), then bootstrap.
	domain := "gui/" + strconv.Itoa(os.Getuid())
	_ = run("launchctl", "bootout", domain+"/"+label)
	if out, err := runOut("launchctl", "bootstrap", domain, plist); err != nil {
		return fmt.Errorf("launchctl bootstrap failed: %v: %s", err, strings.TrimSpace(out))
	}
	return nil
}

// Uninstall unloads the agent and removes its plist.
func Uninstall() error {
	plist, err := PlistPath()
	if err != nil {
		return err
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	_ = run("launchctl", "bootout", domain+"/"+label)
	if err := os.Remove(plist); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// State is a structured view of the LaunchAgent, for UIs that need to branch on
// it (the menu-bar app) rather than print the human string Status returns.
type State struct {
	Installed bool   // plist exists on disk
	Loaded    bool   // launchctl knows about the label
	Running   bool   // launchd reports a live PID
	PID       string // the running PID, when Running
}

// Query reads the agent's install/run state without formatting or side effects.
func Query() (State, error) {
	plist, err := PlistPath()
	if err != nil {
		return State{}, err
	}
	if _, err := os.Stat(plist); os.IsNotExist(err) {
		return State{}, nil
	}
	s := State{Installed: true}
	if out, err := runOut("launchctl", "list", label); err == nil {
		s.Loaded = true
		if pid := field(out, "PID"); pid != "" {
			s.Running, s.PID = true, pid
		}
	}
	return s, nil
}

// Status reports whether the agent is installed and whether it is running.
func Status() (string, error) {
	s, err := Query()
	if err != nil {
		return "", err
	}
	if !s.Installed {
		return "not installed — run `wgh daemon install`", nil
	}
	plist, _ := PlistPath()
	logPath, _ := LogPath()
	state := "installed but not loaded"
	switch {
	case s.Running:
		state = "running (pid " + s.PID + ")"
	case s.Loaded:
		state = "loaded (idle)"
	}
	return fmt.Sprintf("%s\n  plist: %s\n  log:   %s", state, plist, logPath), nil
}

func run(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}

func runOut(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

// field pulls a value from a `launchctl list` dict line like `"PID" = 123;`.
func field(out, key string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, `"`+key+`"`) {
			continue
		}
		if i := strings.Index(line, "="); i >= 0 {
			return strings.Trim(strings.TrimSpace(line[i+1:]), `;" `)
		}
	}
	return ""
}

// searchPath builds a PATH for the agent: launchd hands it a minimal PATH, but
// the daemon shells out to `gh` (for auth), `open`, and the notifier. Include
// the dir holding gh (resolved now) plus the usual tool locations.
func searchPath(exe string) string {
	dirs := []string{filepath.Dir(exe), "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin"}
	if gh, err := exec.LookPath("gh"); err == nil {
		dirs = append([]string{filepath.Dir(gh)}, dirs...)
	}
	seen := map[string]bool{}
	var uniq []string
	for _, d := range dirs {
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		uniq = append(uniq, d)
	}
	return strings.Join(uniq, ":")
}

func plistBody(exe, logPath, path string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>daemon</string>
		<string>run</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>%s</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ProcessType</key>
	<string>Background</string>
	<key>ThrottleInterval</key>
	<integer>30</integer>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, label, xmlEscape(exe), xmlEscape(path), xmlEscape(logPath), xmlEscape(logPath))
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
