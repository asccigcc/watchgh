// Command wgh-menu is a macOS menu-bar viewer for the watchgh timeline.
// It reads the same SQLite store the daemon fills, shows recent events in the
// status-bar dropdown with an unread badge, and opens an item (marking it read
// here and on GitHub) by delegating to the `wgh` CLI. It never polls
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

	"watchgh/internal/config"
	"watchgh/internal/daemon"
	"watchgh/internal/ingest"
	"watchgh/internal/store"
	"watchgh/internal/timeline"

	"github.com/caseymrm/menuet/v2"
)

const (
	badgeTick = 5 * time.Second
	iconName  = "menubar" // template PNG in the bundle's Resources
)

// st is the shared read handle on the store; WAL mode lets it read while the
// daemon writes, and SetMaxOpenConns(1) serializes our own concurrent reads.
var st *store.Store

// cfg holds the same user thresholds the CLI reads — menu row cap and the DUE
// staleness window — so both surfaces agree. Every field has a default.
var cfg = config.Defaults()

func main() {
	path, err := store.DefaultPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, "wgh-menu:", err)
		os.Exit(1)
	}
	st, err = store.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wgh-menu:", err)
		os.Exit(1)
	}
	defer st.Close()
	_ = ingest.Backfill(st) // one-off cleanup of pre-fix rows

	if c, err := config.Load(); err != nil {
		fmt.Fprintln(os.Stderr, "wgh-menu: config:", err, "— using defaults")
	} else {
		cfg = c
	}

	go refreshBadge()

	app := menuet.App()
	app.Name = "watchgh"
	app.Label = "com.watchgh.menu" // also lets menuet manage "Start at Login"
	app.Children = menuItems
	app.RunApplication() // menuet appends "Start at Login" and "Quit" itself
}

// refreshBadge keeps the menu-bar title's unread count current.
func refreshBadge() {
	for {
		setState()
		time.Sleep(badgeTick)
	}
}

// setState draws the menu-bar item: the eye icon plus the unread count (icon
// alone when nothing is unread). menuet replaces the whole state each call, so
// every updater goes through here to keep the icon.
func setState() {
	menuet.App().SetMenuState(&menuet.MenuState{Image: iconName, Title: badgeTitle()})
}

func badgeTitle() string {
	n, err := st.UnreadCount()
	if err != nil || n == 0 {
		return ""
	}
	return fmt.Sprintf(" %d", n) // leading space separates it from the icon
}

// menuItems builds the dropdown fresh each time it opens: unread events only,
// newest first, each clickable to open, then an "open all unread" action.
func menuItems() []menuet.MenuItem {
	all, err := st.List()
	if err != nil {
		return append([]menuet.MenuItem{menuet.Regular{Text: "⚠ " + err.Error()}}, daemonItems()...)
	}
	// The menu is an inbox, not a history: show only what you haven't visited
	// (new + still-unread-but-aging). Read items live in `wgh`. This
	// also keeps the dropdown in step with the badge, which counts unread only.
	var events []timeline.Event
	for _, e := range all {
		if e.Unread {
			events = append(events, e)
		}
	}
	if len(events) == 0 {
		return append([]menuet.MenuItem{menuet.Regular{Text: "✓ all caught up"}}, daemonItems()...)
	}
	sort.SliceStable(events, func(i, j int) bool {
		return events[i].TS.After(events[j].TS) // newest at the top
	})

	now := time.Now()
	var items []menuet.MenuItem
	shown := make([]int64, 0, cfg.MenuRows) // only the visible rows' seqs
	for i, e := range events {
		if i >= cfg.MenuRows {
			break
		}
		shown = append(shown, e.Seq)
		seq := e.Seq
		items = append(items, menuet.Regular{
			Runs:     rowRuns(e, now),
			Subtitle: []menuet.TextRun{{Text: e.Detail + " · " + timeline.Relative(now.Sub(e.TS))}},
			Clicked:  func() { openItem(seq) },
		})
	}

	// Open only the visible batch, not all N: opening them marks them read, they
	// drop off, and the next batch surfaces on the next open — so a big backlog
	// pages through 5 at a time instead of spawning dozens of browser tabs.
	more := len(events) - len(shown)
	if more > 0 {
		items = append(items, menuet.Regular{
			Text:  fmt.Sprintf("…%d more — run `wgh`", more),
			Color: menuet.LabelTertiary,
		})
	}
	label := fmt.Sprintf("Open all (%d)", len(shown))
	if more > 0 {
		label = fmt.Sprintf("Open next %d", len(shown))
	}
	batch := shown
	items = append(items,
		menuet.Separator{},
		menuet.Regular{Text: label, Clicked: func() { openAll(batch) }})
	return append(items, daemonItems()...)
}

// daemonItems surfaces whether the background poller is alive, with a one-click
// Start when it isn't. Without this the menu can silently show a stale timeline
// because nothing is polling. Stop/restart stay in the CLI — this is a viewer,
// and a dead poller is the only daemon state worth acting on from here.
func daemonItems() []menuet.MenuItem {
	s, err := daemon.Query()
	if err != nil {
		return nil
	}
	if s.Running {
		return []menuet.MenuItem{
			menuet.Separator{},
			menuet.Regular{Text: "Daemon: running ✓", Color: menuet.SystemGreen},
		}
	}
	return []menuet.MenuItem{
		menuet.Separator{},
		menuet.Regular{Text: "Daemon: not running", Color: menuet.SystemOrange},
		menuet.Regular{Text: "Start background poller", Clicked: startDaemon},
	}
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
	case e.Unread && e.Actionable && now.Sub(e.TS) > cfg.StaleAfter:
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

// startDaemon (re)installs the LaunchAgent via the CLI rather than calling
// daemon.Install here: install points the plist at os.Executable, which must be
// the wgh poller, not this viewer binary.
func startDaemon() {
	_ = exec.Command(wghBin(), "daemon", "install").Run()
	setState()
	menuet.App().MenuChanged()
}

func openItem(seq int64) {
	_ = exec.Command(wghBin(), "open", strconv.FormatInt(seq, 10)).Run()
	setState()
	menuet.App().MenuChanged()
}

func openAll(seqs []int64) {
	for _, seq := range seqs {
		_ = exec.Command(wghBin(), "open", strconv.FormatInt(seq, 10)).Run()
	}
	setState()
	menuet.App().MenuChanged()
}

// wghBin locates the CLI: prefer the copy next to this binary, then PATH.
func wghBin() string {
	if self, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(self), "wgh")
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	if p, err := exec.LookPath("wgh"); err == nil {
		return p
	}
	return "wgh"
}
