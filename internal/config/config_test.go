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
	if d.Retention != 30*24*time.Hour {
		t.Errorf("Retention = %v, want 720h", d.Retention)
	}
	if d.PollFloor != 5*time.Minute {
		t.Errorf("PollFloor = %v, want 5m", d.PollFloor)
	}
}

func TestParseFullOverlay(t *testing.T) {
	in := `
stale_after   = "12h"
retention     = "14d"
poll_floor    = "90s"
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
}

func TestParsePartialKeepsDefaults(t *testing.T) {
	c, err := Parse(strings.NewReader(`poll_floor = "90s"`))
	if err != nil {
		t.Fatal(err)
	}
	if c.PollFloor != 90*time.Second {
		t.Errorf("PollFloor = %v, want 90s", c.PollFloor)
	}
	// Everything unset stays at the default.
	if c.StaleAfter != 24*time.Hour || c.Retention != 30*24*time.Hour {
		t.Errorf("unset fields drifted from defaults: %+v", c)
	}
}

func TestParseDurationDays(t *testing.T) {
	cases := map[string]time.Duration{
		"7d":    7 * 24 * time.Hour,
		"1d":    24 * time.Hour,
		"24h":   24 * time.Hour,
		"90s":   90 * time.Second,
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
stale_after = 12h   # trailing comment

poll_floor = "60s"
`
	c, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if c.StaleAfter != 12*time.Hour {
		t.Errorf("StaleAfter = %v, want 12h (trailing comment not stripped?)", c.StaleAfter)
	}
	if c.PollFloor != 60*time.Second {
		t.Errorf("PollFloor = %v", c.PollFloor)
	}
}

func TestParseUnknownKeyIgnored(t *testing.T) {
	c, err := Parse(strings.NewReader("future_setting = 42\npoll_floor = \"90s\""))
	if err != nil {
		t.Fatalf("unknown key should be ignored, got error: %v", err)
	}
	if c.PollFloor != 90*time.Second {
		t.Errorf("PollFloor = %v, want 90s", c.PollFloor)
	}
}

func TestParseRejectsBadValues(t *testing.T) {
	bad := []string{
		`stale_after = "banana"`,
		`poll_floor = "-5s"`,
		`poll_floor = "10s"`, // below the MinPollFloor guardrail
		`no equals sign`,
	}
	for _, in := range bad {
		if _, err := Parse(strings.NewReader(in)); err == nil {
			t.Errorf("Parse(%q) = nil error, want error", in)
		}
	}
}

func TestParseAcceptsPollFloorAtMinimum(t *testing.T) {
	c, err := Parse(strings.NewReader(`poll_floor = "60s"`))
	if err != nil {
		t.Fatalf("poll_floor at the minimum should be accepted: %v", err)
	}
	if c.PollFloor != MinPollFloor {
		t.Errorf("PollFloor = %v, want %v", c.PollFloor, MinPollFloor)
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
	if err := os.WriteFile(path, []byte(`poll_floor = "90s"`), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.PollFloor != 90*time.Second {
		t.Errorf("PollFloor = %v, want 90s", c.PollFloor)
	}
}
