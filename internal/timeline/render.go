package timeline

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// ANSI color codes; empty when color is disabled.
type color string

const (
	colReset   color = "\x1b[0m"
	colDim     color = "\x1b[2m"
	colRed     color = "\x1b[31m"
	colGreen   color = "\x1b[32m"
	colYellow  color = "\x1b[33m"
	colBlue    color = "\x1b[34m"
	colMagenta color = "\x1b[35m"
)

// defaultStaleAfter is how long an unread, actionable item may sit before it
// earns the DUE marker, used when RenderOpts leaves StaleAfter unset.
const defaultStaleAfter = 24 * time.Hour

// RenderOpts controls output for a given terminal/pipe.
type RenderOpts struct {
	Color      bool          // emit ANSI colors
	Hyperlinks bool          // emit OSC 8 hyperlinks (else append a raw URL)
	HideSeq    bool          // drop the leading seq column (TUI navigates by selection, not number)
	Now        time.Time     // reference time for relative timestamps
	StaleAfter time.Duration // DUE threshold; zero falls back to defaultStaleAfter
}

// Render sorts events oldest-first (newest at the bottom, log-style) and
// returns the full timeline block.
func Render(events []Event, o RenderOpts) string {
	sort.SliceStable(events, func(i, j int) bool {
		return events[i].TS.Before(events[j].TS)
	})
	var b strings.Builder
	for _, e := range events {
		row := renderRow(e, o)
		// Fallback link when the terminal can't render OSC 8 (e.g. piped output).
		if !o.Hyperlinks && e.URL != "" {
			row += "  " + colorize(e.URL, colDim, o.Color)
		}
		b.WriteString(row)
		b.WriteByte('\n')
	}
	return b.String()
}

// RenderRow renders a single event as one line — no trailing newline and no
// raw-URL fallback — for interactive callers like the TUI, which navigate by
// selection rather than clicking. o.Color and o.Hyperlinks apply as usual.
func RenderRow(e Event, o RenderOpts) string { return renderRow(e, o) }

func renderRow(e Event, o RenderOpts) string {
	// SEQ (stable local number for `watchgit open N`), dim, right-aligned in 4.
	// The TUI hides it — it opens the selected row, so the number is just noise.
	seq := ""
	if !o.HideSeq {
		seq = colorize(padLeft(fmt.Sprintf("%d", e.Seq), 4), colDim, o.Color) + " "
	}

	// GUTTER: your state — DUE (needs you, aging), NEW (unread), blank (read).
	label, gcolor := gutterMark(e, o.Now, o.staleAfter())
	gutter := colorize(padRight(label, 3), gcolor, o.Color)

	// TIME (relative, right-aligned in 5).
	age := padLeft(relative(o.Now.Sub(e.TS)), 5)

	// BADGE (glyph + label, colored, padded to visible width 8).
	badge := e.Kind.Badge()
	badgeCell := colorize(padRight(badge.Glyph+" "+badge.Label, 8), badge.Color, o.Color)

	// REPO#NUM (OSC 8 link to the PR), padded to 18; #num always preserved.
	refText := refLabel(e.Repo, e.Number, 18)
	ref := hyperlink(e.URL, refText, o) + strings.Repeat(" ", pad(refText, 18))

	// AUTHOR, padded to 11; dim em-dash when it's the viewer's own PR.
	authorText := e.Author
	if e.IsMine || authorText == "" {
		authorText = "—"
	} else {
		authorText = "@" + authorText
	}
	authorText = truncate(authorText, 11)
	author := colorizeIf(e.IsMine, padRight(authorText, 11), colDim, o.Color)

	row := fmt.Sprintf("%s%s %s  %s  %s  %s  %s",
		seq, gutter, age, badgeCell, ref, author, e.Detail)

	// Read/history rows dim as a freshness cue.
	if !e.Unread && o.Color {
		row = string(colDim) + row + string(colReset)
	}
	return row
}

// staleAfter resolves the DUE threshold, defaulting when the caller left it unset.
func (o RenderOpts) staleAfter() time.Duration {
	if o.StaleAfter > 0 {
		return o.StaleAfter
	}
	return defaultStaleAfter
}

// gutterMark returns the gutter tag and its color for an event's state:
// "DUE" (unread, actionable, aging past staleAfter), "NEW" (unread), "" (read).
func gutterMark(e Event, now time.Time, staleAfter time.Duration) (string, color) {
	if e.Unread && e.Actionable && now.Sub(e.TS) > staleAfter {
		return "DUE", colYellow
	}
	if e.Unread {
		return "NEW", colBlue
	}
	return "", ""
}

// Relative renders a compact human age ("now", "5m", "3h", "2d") for reuse
// outside the timeline renderer (e.g. the menu-bar app).
func Relative(d time.Duration) string { return relative(d) }

func relative(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// hyperlink wraps text in an OSC 8 escape when enabled and a URL exists.
func hyperlink(url, text string, o RenderOpts) string {
	if !o.Hyperlinks || url == "" {
		return text
	}
	return "\x1b]8;;" + url + "\x1b\\" + text + "\x1b]8;;\x1b\\"
}

func colorize(s string, c color, on bool) string {
	if !on {
		return s
	}
	return string(c) + s + string(colReset)
}

func colorizeIf(cond bool, s string, c color, on bool) string {
	if cond {
		return colorize(s, c, on)
	}
	return s
}

// visibleWidth counts runes (glyphs here are single-width).
func visibleWidth(s string) int { return utf8.RuneCountInString(s) }

func pad(s string, w int) int {
	if n := w - visibleWidth(s); n > 0 {
		return n
	}
	return 0
}

func padRight(s string, w int) string { return s + strings.Repeat(" ", pad(s, w)) }

func padLeft(s string, w int) string { return strings.Repeat(" ", pad(s, w)) + s }

// ShortRef renders "name#num" (repo short name, no owner) without truncation,
// for use outside the fixed-width timeline (e.g. notification titles).
func ShortRef(repo string, number int) string {
	name := repo
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if number > 0 {
		return fmt.Sprintf("%s#%d", name, number)
	}
	return name
}

// refLabel renders the repo (short name only) + #num within w columns, always
// keeping the #num suffix intact and truncating the name if needed.
func refLabel(repo string, number, w int) string {
	name := repo
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	suffix := ""
	if number > 0 {
		suffix = fmt.Sprintf("#%d", number)
	}
	if avail := w - visibleWidth(suffix); visibleWidth(name) > avail {
		name = truncate(name, avail)
	}
	return name + suffix
}

func truncate(s string, w int) string {
	if visibleWidth(s) <= w {
		return s
	}
	r := []rune(s)
	return string(r[:w-1]) + "…"
}
