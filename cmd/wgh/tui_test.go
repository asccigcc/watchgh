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
		got, carry := parseKeys([]byte(in))
		if len(carry) != 0 {
			t.Errorf("parseKeys(%q) unexpected carry %q", in, carry)
		}
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
	got, _ := parseKeys([]byte("\x1b[Bq"))
	if len(got) != 2 || got[0].kind != keyDown || got[1].kind != keyQuit {
		t.Errorf("got %+v, want [down quit]", got)
	}
}

func TestParseKeysTildeNavigation(t *testing.T) {
	// The tilde-final CSI keys must decode to the right navigation actions, with
	// the trailing ~ fully consumed (not left to fall through as keyNone).
	cases := map[string]keyKind{
		"\x1b[5~": keyPageUp,
		"\x1b[6~": keyPageDown,
		"\x1b[1~": keyTop,    // Home
		"\x1b[4~": keyBottom, // End
	}
	for in, want := range cases {
		got, carry := parseKeys([]byte(in))
		if len(carry) != 0 {
			t.Errorf("parseKeys(%q) unexpected carry %q", in, carry)
		}
		if len(got) != 1 || got[0].kind != want {
			t.Errorf("parseKeys(%q) = %+v, want single %v", in, got, want)
		}
	}
}

func TestParseKeysIgnoresModifierParams(t *testing.T) {
	// A modified arrow (ESC [ 1 ; 5 A) still resolves to the base arrow.
	got, _ := parseKeys([]byte("\x1b[1;5A"))
	if len(got) != 1 || got[0].kind != keyUp {
		t.Errorf("modified arrow: got %+v, want [up]", got)
	}
}

func TestParseKeysHoldsSplitCSI(t *testing.T) {
	// A CSI split across two reads must not be misread as a lone Escape (quit):
	// the incomplete tail is returned as carry and completed on the next read.
	got, carry := parseKeys([]byte("j\x1b["))
	if len(got) != 1 || got[0].kind != keyDown {
		t.Errorf("first half: got %+v, want [down]", got)
	}
	if string(carry) != "\x1b[" {
		t.Errorf("first half carry = %q, want ESC[", carry)
	}
	// Prepending the carry to the next read completes the arrow.
	got, carry = parseKeys(append(carry, 'A'))
	if len(carry) != 0 {
		t.Errorf("second half carry = %q, want none", carry)
	}
	if len(got) != 1 || got[0].kind != keyUp {
		t.Errorf("completed sequence: got %+v, want [up]", got)
	}
}

func TestParseKeysBareEscapeStillQuits(t *testing.T) {
	// A bare trailing ESC is a real Escape keypress and must quit, not hang
	// waiting for more bytes.
	got, carry := parseKeys([]byte("\x1b"))
	if len(carry) != 0 {
		t.Errorf("bare ESC carry = %q, want none", carry)
	}
	if len(got) != 1 || got[0].kind != keyQuit {
		t.Errorf("bare ESC: got %+v, want [quit]", got)
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

func TestRecountMatchesFiltersAndRoster(t *testing.T) {
	m := &model{
		counts: make([]int, len(tabDefs)),
		all: []timeline.Event{
			{Unread: true, IsMine: false, Source: "notification"},  // Inbox
			{Unread: true, IsMine: false, Source: "notification"},  // Inbox
			{Unread: false, IsMine: false, Source: "notification"}, // Read
			{Unread: true, IsMine: true, Source: "graphql"},        // CI
		},
		prs: []timeline.Event{{}, {}, {}}, // three roster rows
	}
	m.recount()

	// Order: Inbox(0) · My PRs(1) · Read(2) · CI(3).
	want := []int{2, 3, 1, 1}
	for i, w := range want {
		if m.counts[i] != w {
			t.Errorf("counts[%d] = %d, want %d", i, m.counts[i], w)
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
