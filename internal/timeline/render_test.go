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

func TestShowCIRendersGlyphIndependentOfBadge(t *testing.T) {
	now := time.Now()
	e := Event{Kind: KindBlocked, Repo: "acme/api", Number: 1, TS: now, CIState: "SUCCESS", IsMine: true}

	// With ShowCI the row carries the green ✓ CI glyph next to the ⊘ block badge —
	// the two dimensions read independently.
	got := renderRow(e, RenderOpts{LeadWithPR: true, Now: now, ShowCI: true})
	if !strings.Contains(got, "✓") || !strings.Contains(got, "⊘") {
		t.Errorf("ShowCI row missing CI glyph or block badge: %q", got)
	}

	// Without ShowCI (other tabs) the CI glyph column is absent.
	if off := renderRow(e, RenderOpts{LeadWithPR: true, Now: now}); strings.Contains(off, "✓") {
		t.Errorf("CI glyph leaked into a non-ShowCI row: %q", off)
	}
}

func TestCIGlyph(t *testing.T) {
	cases := map[string]string{
		"SUCCESS": "✓", "FAILURE": "✗", "ERROR": "✗",
		"PENDING": "●", "EXPECTED": "●", "": "○", "UNKNOWN": "○",
	}
	for state, want := range cases {
		if g, _ := ciGlyph(state); g != want {
			t.Errorf("ciGlyph(%q) = %q, want %q", state, g, want)
		}
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

func TestRelativeOrDate(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		ts   time.Time
		want string
	}{
		{now.Add(-30 * time.Second), "now"},
		{now.Add(-5 * time.Minute), "5m"},
		{now.Add(-3 * time.Hour), "3h"},
		{now.Add(-50 * time.Hour), "2d"},
		{now.Add(-6 * 24 * time.Hour), "6d"},                          // still relative under a week
		{now.Add(-8 * 24 * time.Hour), "Sep 12"},                      // older than a week -> date, same year
		{time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC), "Dec 31 '25"}, // prior year -> year suffix
	}
	for _, c := range cases {
		if got := relativeOrDate(now, c.ts); got != c.want {
			t.Errorf("relativeOrDate(%s) = %q, want %q", c.ts.Format(time.RFC3339), got, c.want)
		}
	}
}

func TestRenderRowTags(t *testing.T) {
	now := time.Now()
	e := Event{Kind: KindOpenPR, Repo: "acme/api", Number: 7, TS: now, IsMine: true,
		IsDraft: true, Labels: []string{"bug", "wip"}}

	got := renderRow(e, RenderOpts{LeadWithPR: true, Now: now})
	for _, want := range []string{"[draft]", "[bug]", "[wip]"} {
		if !strings.Contains(got, want) {
			t.Errorf("row missing tag %q: %q", want, got)
		}
	}

	// No draft, no labels -> no trailing tag decoration at all.
	plain := renderRow(Event{Kind: KindOpenPR, Repo: "acme/api", Number: 8, TS: now}, RenderOpts{LeadWithPR: true, Now: now})
	if strings.Contains(plain, "[") {
		t.Errorf("untagged row should carry no [chip]: %q", plain)
	}
}

func TestGutterMark(t *testing.T) {
	now := time.Now()
	// A fresh unread item is NEW; once it ages past the window the badge drops even
	// though it's still unread (it just settles into the list). Read items are blank.
	fresh := Event{Unread: true, TS: now.Add(-1 * time.Hour)}
	if got, _ := gutterMark(fresh, now, defaultStaleAfter); got != "NEW" {
		t.Errorf("fresh unread gutter = %q, want NEW", got)
	}
	aged := Event{Unread: true, TS: now.Add(-48 * time.Hour)}
	if got, _ := gutterMark(aged, now, defaultStaleAfter); got != "" {
		t.Errorf("aged unread gutter = %q, want empty (NEW expires after the window)", got)
	}
	read := Event{Unread: false, TS: now.Add(-1 * time.Hour)}
	if got, _ := gutterMark(read, now, defaultStaleAfter); got != "" {
		t.Errorf("read gutter = %q, want empty", got)
	}
}
