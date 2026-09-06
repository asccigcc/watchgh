// Package config loads watchgit's user-tunable thresholds from a flat
// `key = value` file (a TOML-compatible subset — no tables or arrays needed for
// a handful of scalars). It lives next to the store, and every field falls back
// to a sensible default, so the file is entirely optional.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config holds the thresholds that were previously hardcoded across the CLI,
// renderer, and menu-bar app.
type Config struct {
	StaleAfter           time.Duration // unread + actionable older than this earns the DUE marker
	Retention            time.Duration // prune read/resolved events older than this
	PollFloor            time.Duration // minimum spacing between polls, regardless of server hints
	MenuRows             int           // max rows shown in the menu-bar dropdown
	NotifyActionableOnly bool          // desktop-notify actionable events only
}

// Defaults returns the built-in configuration used when no file (or no key) is
// present. These mirror the constants they replaced.
func Defaults() Config {
	return Config{
		StaleAfter:           24 * time.Hour,
		Retention:            7 * 24 * time.Hour,
		PollFloor:            60 * time.Second,
		MenuRows:             5,
		NotifyActionableOnly: true,
	}
}

// DefaultPath is the config file's location — the same per-user directory that
// holds the store (`~/Library/Application Support/watchgit/` on macOS).
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "watchgit", "config.toml"), nil
}

// Load reads the config from DefaultPath. A missing file yields Defaults with
// no error; a malformed file yields Defaults plus the error, so a caller can
// warn and still run.
func Load() (Config, error) {
	path, err := DefaultPath()
	if err != nil {
		return Defaults(), err
	}
	return LoadFile(path)
}

// LoadFile reads and parses path, returning Defaults if the file is absent.
func LoadFile(path string) (Config, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return Defaults(), nil
	}
	if err != nil {
		return Defaults(), err
	}
	defer f.Close()
	return Parse(f)
}

// Parse overlays the file's keys onto Defaults, so an omitted key keeps its
// default. Unknown keys are ignored (forward-compatible); bad values error.
func Parse(r io.Reader) (Config, error) {
	cfg := Defaults()
	sc := bufio.NewScanner(r)
	for line := 1; sc.Scan(); line++ {
		raw := strings.TrimSpace(sc.Text())
		if raw == "" || strings.HasPrefix(raw, "#") {
			continue
		}
		key, val, ok := splitKV(raw)
		if !ok {
			return cfg, fmt.Errorf("config line %d: expected `key = value`, got %q", line, raw)
		}
		if err := cfg.set(key, val); err != nil {
			return cfg, fmt.Errorf("config line %d: %w", line, err)
		}
	}
	if err := sc.Err(); err != nil {
		return cfg, err
	}
	return cfg, cfg.validate()
}

func splitKV(line string) (key, val string, ok bool) {
	i := strings.IndexByte(line, '=')
	if i < 0 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:i])
	return key, cleanValue(line[i+1:]), key != ""
}

// cleanValue trims a value, honoring a "quoted" span verbatim or dropping an
// unquoted trailing `# comment`.
func cleanValue(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && v[0] == '"' {
		if j := strings.IndexByte(v[1:], '"'); j >= 0 {
			return v[1 : 1+j]
		}
	}
	if i := strings.IndexByte(v, '#'); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return v
}

func (c *Config) set(key, val string) error {
	switch key {
	case "stale_after":
		return setDuration(&c.StaleAfter, val)
	case "retention":
		return setDuration(&c.Retention, val)
	case "poll_floor":
		return setDuration(&c.PollFloor, val)
	case "menu_rows":
		n, err := strconv.Atoi(val)
		if err != nil {
			return fmt.Errorf("menu_rows: %v", err)
		}
		c.MenuRows = n
	case "actionable_only_notify":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return fmt.Errorf("actionable_only_notify: %v", err)
		}
		c.NotifyActionableOnly = b
	default:
		// Ignore unknown keys so old binaries tolerate newer config files.
	}
	return nil
}

func setDuration(dst *time.Duration, val string) error {
	d, err := parseDuration(val)
	if err != nil {
		return err
	}
	*dst = d
	return nil
}

// parseDuration extends time.ParseDuration with a day unit ("7d"), which the
// stdlib rejects but reads far more naturally for a retention window.
func parseDuration(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		if days, err := strconv.ParseFloat(n, 64); err == nil {
			return time.Duration(days * float64(24*time.Hour)), nil
		}
	}
	return time.ParseDuration(s)
}

func (c Config) validate() error {
	switch {
	case c.StaleAfter <= 0:
		return errors.New("stale_after must be positive")
	case c.Retention <= 0:
		return errors.New("retention must be positive")
	case c.PollFloor <= 0:
		return errors.New("poll_floor must be positive")
	case c.MenuRows < 1:
		return errors.New("menu_rows must be at least 1")
	}
	return nil
}
