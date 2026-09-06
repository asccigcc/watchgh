package timeline

import (
	"strings"
	"testing"
	"time"
)

func TestRefLabelPreservesNumber(t *testing.T) {
	got := refLabel("TwentyeightHealth/TwentyeightHealth", 12115, 18)
	if !strings.HasSuffix(got, "#12115") {
		t.Errorf("refLabel dropped the number: %q", got)
	}
	if w := visibleWidth(got); w > 18 {
		t.Errorf("refLabel width %d exceeds 18: %q", w, got)
	}
	if !strings.Contains(got, "…") {
		t.Errorf("expected long repo name to be truncated: %q", got)
	}
}

func TestRefLabelShortRepoUnchanged(t *testing.T) {
	got := refLabel("acme/api", 412, 18)
	if got != "api#412" {
		t.Errorf("got %q, want %q", got, "api#412")
	}
}

func TestShortRef(t *testing.T) {
	if got := ShortRef("TwentyeightHealth/data-warehouse", 37); got != "data-warehouse#37" {
		t.Errorf("got %q", got)
	}
	if got := ShortRef("acme/api", 0); got != "api" {
		t.Errorf("got %q, want %q", got, "api")
	}
}

func TestRelative(t *testing.T) {
	cases := map[time.Duration]string{
		30 * time.Second: "now",
		5 * time.Minute:  "5m",
		3 * time.Hour:    "3h",
		50 * time.Hour:   "2d",
	}
	for d, want := range cases {
		if got := relative(d); got != want {
			t.Errorf("relative(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestGutterMark(t *testing.T) {
	now := time.Now()
	old := Event{Unread: true, Actionable: true, TS: now.Add(-48 * time.Hour)}
	if got, _ := gutterMark(old, now); got != "DUE" {
		t.Errorf("stale actionable gutter = %q, want DUE", got)
	}
	fresh := Event{Unread: true, Actionable: true, TS: now.Add(-1 * time.Hour)}
	if got, _ := gutterMark(fresh, now); got != "NEW" {
		t.Errorf("fresh unread gutter = %q, want NEW", got)
	}
	read := Event{Unread: false, TS: now.Add(-48 * time.Hour)}
	if got, _ := gutterMark(read, now); got != "" {
		t.Errorf("read gutter = %q, want empty", got)
	}
}
