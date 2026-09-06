package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	d := Defaults()
	if d.StaleAfter != 24*time.Hour {
		t.Errorf("StaleAfter = %v, want 24h", d.StaleAfter)
	}
	if d.Retention != 7*24*time.Hour {
		t.Errorf("Retention = %v, want 168h", d.Retention)
	}
	if d.PollFloor != 60*time.Second {
		t.Errorf("PollFloor = %v, want 60s", d.PollFloor)
	}
	if d.MenuRows != 5 {
		t.Errorf("MenuRows = %d, want 5", d.MenuRows)
	}
	if !d.NotifyActionableOnly {
		t.Errorf("NotifyActionableOnly = false, want true")
	}
}

func TestParseFullOverlay(t *testing.T) {
	in := `
stale_after   = "12h"
retention     = "14d"
poll_floor    = "90s"
menu_rows     = 8
actionable_only_notify = false
`
	c, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if c.StaleAfter != 12*time.Hour {
		t.Errorf("StaleAfter = %v", c.StaleAfter)
	}
	if c.Retention != 14*24*time.Hour {
		t.Errorf("Retention = %v, want 336h", c.Retention)
	}
	if c.PollFloor != 90*time.Second {
		t.Errorf("PollFloor = %v", c.PollFloor)
	}
	if c.MenuRows != 8 {
		t.Errorf("MenuRows = %d", c.MenuRows)
	}
	if c.NotifyActionableOnly {
		t.Errorf("NotifyActionableOnly = true, want false")
	}
}

func TestParsePartialKeepsDefaults(t *testing.T) {
	c, err := Parse(strings.NewReader(`menu_rows = 3`))
	if err != nil {
		t.Fatal(err)
	}
	if c.MenuRows != 3 {
		t.Errorf("MenuRows = %d, want 3", c.MenuRows)
	}
	// Everything unset stays at the default.
	if c.StaleAfter != 24*time.Hour || c.Retention != 7*24*time.Hour {
		t.Errorf("unset fields drifted from defaults: %+v", c)
	}
}

func TestParseDurationDays(t *testing.T) {
	cases := map[string]time.Duration{
		"7d":   7 * 24 * time.Hour,
		"1d":   24 * time.Hour,
		"24h":  24 * time.Hour,
		"90s":  90 * time.Second,
		"1h30m": 90 * time.Minute,
	}
	for in, want := range cases {
		got, err := parseDuration(in)
		if err != nil {
			t.Errorf("parseDuration(%q) error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseDuration(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestParseCommentsAndBlanks(t *testing.T) {
	in := `
# a full-line comment
menu_rows = 4   # trailing comment

poll_floor = "30s"
`
	c, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if c.MenuRows != 4 {
		t.Errorf("MenuRows = %d, want 4 (trailing comment not stripped?)", c.MenuRows)
	}
	if c.PollFloor != 30*time.Second {
		t.Errorf("PollFloor = %v", c.PollFloor)
	}
}

func TestParseUnknownKeyIgnored(t *testing.T) {
	c, err := Parse(strings.NewReader("future_setting = 42\nmenu_rows = 2"))
	if err != nil {
		t.Fatalf("unknown key should be ignored, got error: %v", err)
	}
	if c.MenuRows != 2 {
		t.Errorf("MenuRows = %d", c.MenuRows)
	}
}

func TestParseRejectsBadValues(t *testing.T) {
	bad := []string{
		`stale_after = "banana"`,
		`menu_rows = 0`,
		`menu_rows = notanint`,
		`poll_floor = "-5s"`,
		`actionable_only_notify = maybe`,
		`no equals sign`,
	}
	for _, in := range bad {
		if _, err := Parse(strings.NewReader(in)); err == nil {
			t.Errorf("Parse(%q) = nil error, want error", in)
		}
	}
}

func TestLoadFileMissingReturnsDefaults(t *testing.T) {
	c, err := LoadFile(filepath.Join(t.TempDir(), "does-not-exist.toml"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if c != Defaults() {
		t.Errorf("missing file should yield defaults, got %+v", c)
	}
}

func TestLoadFileParses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(`menu_rows = 9`), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.MenuRows != 9 {
		t.Errorf("MenuRows = %d, want 9", c.MenuRows)
	}
}
