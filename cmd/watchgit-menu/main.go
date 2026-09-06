// Command watchgit-menu is a macOS menu-bar viewer for the watchgit timeline.
// It reads the same SQLite store the daemon fills, shows recent events in the
// status-bar dropdown with an unread badge, and opens an item (marking it read
// here and on GitHub) by delegating to the `watchgit` CLI. It never polls
// GitHub itself — the launchd daemon is the poller; this is a pure viewer.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"watchgit/internal/store"
	"watchgit/internal/timeline"

	"github.com/caseymrm/menuet/v2"
)

const (
	maxRows    = 25               // cap on dropdown rows
	staleAfter = 24 * time.Hour   // unread + actionable past this earns a "DUE" pill
	badgeTick  = 5 * time.Second  // how often the menu-bar count refreshes
)

// st is the shared read handle on the store; WAL mode lets it read while the
// daemon writes, and SetMaxOpenConns(1) serializes our own concurrent reads.
var st *store.Store

func main() {
	path, err := store.DefaultPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, "watchgit-menu:", err)
		os.Exit(1)
	}
	st, err = store.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "watchgit-menu:", err)
		os.Exit(1)
	}
	defer st.Close()

	go refreshBadge()

	app := menuet.App()
	app.Name = "watchgit"
	app.Label = "com.watchgit.menu" // also lets menuet manage "Start at Login"
	app.Children = menuItems
	app.RunApplication() // menuet appends "Start at Login" and "Quit" itself
}

// refreshBadge keeps the menu-bar title's unread count current.
func refreshBadge() {
	for {
		menuet.App().SetMenuState(&menuet.MenuState{Title: badgeTitle()})
		time.Sleep(badgeTick)
	}
}

func badgeTitle() string {
	n, err := st.UnreadCount()
	if err != nil || n == 0 {
		return "◆"
	}
	return fmt.Sprintf("◆ %d", n)
}

// menuItems builds the dropdown fresh each time it opens: newest events first,
// each clickable to open, then an "open all unread" action.
func menuItems() []menuet.MenuItem {
	events, err := st.List()
	if err != nil {
		return []menuet.MenuItem{menuet.Regular{Text: "⚠ " + err.Error()}}
	}
	if len(events) == 0 {
		return []menuet.MenuItem{menuet.Regular{Text: "✓ all caught up"}}
	}
	sort.SliceStable(events, func(i, j int) bool {
		return events[i].TS.After(events[j].TS) // newest at the top
	})

	now := time.Now()
	var items []menuet.MenuItem
	var unread []int64
	for i, e := range events {
		if i >= maxRows {
			break
		}
		if e.Unread {
			unread = append(unread, e.Seq)
		}
		seq := e.Seq
		items = append(items, menuet.Regular{
			Runs:     rowRuns(e, now),
			Subtitle: []menuet.TextRun{{Text: e.Detail + " · " + timeline.Relative(now.Sub(e.TS))}},
			Clicked:  func() { openItem(seq) },
		})
	}

	if len(unread) > 0 {
		u := unread
		items = append(items,
			menuet.Separator{},
			menuet.Regular{
				Text:    fmt.Sprintf("Open all unread (%d)", len(u)),
				Clicked: func() { openAll(u) },
			})
	}
	return items
}

// rowRuns styles one row: an optional NEW/DUE pill, the kind badge in its
// color, then the repo#num. The detail and age live in the Subtitle line.
func rowRuns(e timeline.Event, now time.Time) []menuet.TextRun {
	var runs []menuet.TextRun
	if pill, col, ok := statusPill(e, now); ok {
		runs = append(runs,
			menuet.TextRun{Text: pill, Color: col, Badge: true},
			menuet.TextRun{Text: " "})
	}
	b := e.Kind.Badge()
	runs = append(runs,
		menuet.TextRun{Text: b.Glyph + " " + b.Label, Color: kindColor(e.Kind), FontWeight: weight(e.Unread)},
		menuet.TextRun{Text: "  " + timeline.ShortRef(e.Repo, e.Number), FontWeight: weight(e.Unread)},
	)
	return runs
}

// statusPill mirrors the CLI gutter: DUE (unread, actionable, aging), NEW (unread).
func statusPill(e timeline.Event, now time.Time) (string, menuet.Color, bool) {
	switch {
	case e.Unread && e.Actionable && now.Sub(e.TS) > staleAfter:
		return "DUE", menuet.Yellow, true
	case e.Unread:
		return "NEW", menuet.Blue, true
	default:
		return "", menuet.Color{}, false
	}
}

func kindColor(k timeline.Kind) menuet.Color {
	switch k {
	case timeline.KindCIPassed, timeline.KindApproved, timeline.KindUnblocked:
		return menuet.SystemGreen
	case timeline.KindCIFailed:
		return menuet.SystemRed
	case timeline.KindBlocked:
		return menuet.SystemOrange
	case timeline.KindReviewRequested, timeline.KindAssigned:
		return menuet.SystemBlue
	case timeline.KindChangesRequested, timeline.KindCommented:
		return menuet.SystemPurple
	default:
		return menuet.LabelSecondary
	}
}

func weight(unread bool) menuet.FontWeight {
	if unread {
		return menuet.WeightBold
	}
	return menuet.WeightRegular
}

func openItem(seq int64) {
	_ = exec.Command(watchgitBin(), "open", strconv.FormatInt(seq, 10)).Run()
	menuet.App().SetMenuState(&menuet.MenuState{Title: badgeTitle()})
	menuet.App().MenuChanged()
}

func openAll(seqs []int64) {
	for _, seq := range seqs {
		_ = exec.Command(watchgitBin(), "open", strconv.FormatInt(seq, 10)).Run()
	}
	menuet.App().SetMenuState(&menuet.MenuState{Title: badgeTitle()})
	menuet.App().MenuChanged()
}

// watchgitBin locates the CLI: prefer the copy next to this binary, then PATH.
func watchgitBin() string {
	if self, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(self), "watchgit")
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	if p, err := exec.LookPath("watchgit"); err == nil {
		return p
	}
	return "watchgit"
}
