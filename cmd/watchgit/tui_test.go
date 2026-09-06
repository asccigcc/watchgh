package main

import (
	"strings"
	"testing"
	"time"

	"watchgit/internal/timeline"
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
