package main

import (
	"strings"
	"testing"
	"time"

	"watchgh/internal/store"
	"watchgh/internal/timeline"
)

func TestParseKeys(t *testing.T) {
	cases := map[string][]keyKind{
		"\x1b[A": {keyUp},
		"\x1b[B": {keyDown},
		"\x1b[H": {keyTop},
		"\x1b[F": {keyBottom},
		"j":      {keyDown},
		"k":      {keyUp},
		"gG":     {keyTop, keyBottom},
		"r":      {keyRead},
		"R":      {keySync},
		"\r":     {keyOpen},
		"\n":     {keyOpen},
		"q":      {keyQuit},
		"\x1b":   {keyQuit}, // lone Escape
		"\x03":   {keyQuit}, // Ctrl-C
		"1":      {keyTab1},
		"4":      {keyTab4},
		"\t":     {keyTabNext},
	}
	for in, want := range cases {
		got := parseKeys([]byte(in))
		if len(got) != len(want) {
			t.Errorf("parseKeys(%q) len = %d, want %d", in, len(got), len(want))
			continue
		}
		for i := range want {
			if got[i].kind != want[i] {
				t.Errorf("parseKeys(%q)[%d] = %v, want %v", in, i, got[i].kind, want[i])
			}
		}
	}
}

func TestParseKeysArrowThenChar(t *testing.T) {
	// An arrow immediately followed by 'q' in one read should yield both.
	got := parseKeys([]byte("\x1b[Bq"))
	if len(got) != 2 || got[0].kind != keyDown || got[1].kind != keyQuit {
		t.Errorf("got %+v, want [down quit]", got)
	}
}

func TestTruncateANSIPlain(t *testing.T) {
	got := truncateANSI("hello world", 5)
	if !strings.HasPrefix(got, "hello") {
		t.Errorf("truncateANSI kept %q, want prefix hello", got)
	}
	if strings.Contains(got, "world") {
		t.Errorf("truncateANSI did not cut: %q", got)
	}
}

func TestTruncateANSIKeepsEscapes(t *testing.T) {
	// The color escape must survive and not count toward width; 3 visible cells.
	got := truncateANSI("\x1b[31mabcdef\x1b[0m", 3)
	if !strings.HasPrefix(got, "\x1b[31m") {
		t.Errorf("dropped leading color: %q", got)
	}
	if strings.Count(got, "a")+strings.Count(got, "b")+strings.Count(got, "c") != 3 {
		t.Errorf("expected 3 visible chars abc, got %q", got)
	}
	if strings.ContainsRune(got, 'd') {
		t.Errorf("truncateANSI kept clipped char: %q", got)
	}
}

func TestPadANSI(t *testing.T) {
	if got := padANSI("ab", 5); got != "ab   " {
		t.Errorf("padANSI(\"ab\",5) = %q, want %q", got, "ab   ")
	}
	if got := padANSI("abcdef", 3); got != "abc" {
		t.Errorf("padANSI truncate = %q, want abc", got)
	}
	// Multi-byte glyphs count as one cell each.
	if got := padANSI("◆x", 4); got != "◆x  " {
		t.Errorf("padANSI multibyte = %q, want %q", got, "◆x  ")
	}
}

func TestTabFilters(t *testing.T) {
	// Inbox/Read/CI are lenses over stored events; "My PRs" is roster-sourced,
	// so its filter never matches and mine events fold into their PR's roster row.
	// Order: Inbox(0) · My PRs(1) · Read(2) · CI(3).
	cases := []struct {
		e    timeline.Event
		want int // matching tab index, or -1 if none (roster-only)
	}{
		{timeline.Event{Unread: true, IsMine: false, Source: "notification"}, 0},  // Inbox
		{timeline.Event{Unread: false, IsMine: false, Source: "notification"}, 2}, // Read
		{timeline.Event{Unread: true, IsMine: true, Source: "graphql"}, 3},        // CI
		{timeline.Event{Unread: false, IsMine: true, Source: "graphql"}, 3},       // CI (read)
		{timeline.Event{Unread: true, IsMine: true, Source: "notification"}, -1},  // My PRs → roster
	}
	for i, c := range cases {
		hits, matched := 0, -1
		for idx, d := range tabDefs {
			if d.show(c.e) {
				hits++
				matched = idx
			}
		}
		if c.want == -1 {
			if hits != 0 {
				t.Errorf("case %d: want no show match, got %d", i, hits)
			}
			continue
		}
		if hits != 1 || matched != c.want {
			t.Errorf("case %d: matched tab %d (hits %d), want %d", i, matched, hits, c.want)
		}
	}
}

func TestSynthPR(t *testing.T) {
	if e := synthPR(store.OpenPR{MergeState: "BLOCKED"}); e.Kind != timeline.KindBlocked || !e.Unread {
		t.Errorf("blocked PR: got kind %v unread %v, want KindBlocked+unread", e.Kind, e.Unread)
	}
	if e := synthPR(store.OpenPR{CIState: "FAILURE"}); e.Kind != timeline.KindCIFailed || !e.Actionable {
		t.Errorf("failed CI: got kind %v actionable %v, want KindCIFailed+actionable", e.Kind, e.Actionable)
	}
	if e := synthPR(store.OpenPR{CIState: "SUCCESS"}); e.Kind != timeline.KindCIPassed || e.Unread {
		t.Errorf("healthy PR: got kind %v unread %v, want KindCIPassed and not unread", e.Kind, e.Unread)
	}
	if e := synthPR(store.OpenPR{Title: "wip"}); e.Kind != timeline.KindOpenPR || e.Detail != "wip" {
		t.Errorf("quiet PR: got kind %v detail %q, want KindOpenPR + title", e.Kind, e.Detail)
	}
}

func TestLatestMineByPR(t *testing.T) {
	now := time.Now()
	all := []timeline.Event{
		{Repo: "o/r", Number: 1, IsMine: true, TS: now.Add(-time.Hour), Detail: "old"},
		{Repo: "o/r", Number: 1, IsMine: true, TS: now, Detail: "new"},
		{Repo: "o/r", Number: 2, IsMine: false, TS: now, Detail: "theirs"},
	}
	m := latestMineByPR(all)
	if m["o/r#1"].Detail != "new" {
		t.Errorf("latest for o/r#1 = %q, want new", m["o/r#1"].Detail)
	}
	if _, ok := m["o/r#2"]; ok {
		t.Error("non-mine event should be excluded from the roster overlay")
	}
}

func TestSignatureDetectsChange(t *testing.T) {
	now := time.Now()
	a := []timeline.Event{{Seq: 1, Unread: true, TS: now}}
	b := []timeline.Event{{Seq: 2, Unread: true, TS: now}}
	if signature(a) == signature(b) {
		t.Errorf("signature should differ when newest seq changes")
	}
	// Same content → same signature (a ticker reload should be a no-op).
	if signature(a) != signature([]timeline.Event{{Seq: 1, Unread: true, TS: now}}) {
		t.Errorf("signature should be stable for identical sets")
	}
	// Marking the item read (unread tally drops) is a change.
	read := []timeline.Event{{Seq: 1, Unread: false, TS: now}}
	if signature(a) == signature(read) {
		t.Errorf("signature should change when unread count changes")
	}
}
