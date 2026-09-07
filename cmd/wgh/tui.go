// The interactive timeline: bare `wgh` on an alt-screen, arrow keys to
// move, Enter to open (marking read), r to mark read, R to re-sync, q to quit.
// A background goroutine polls GitHub on a ticker (and on launch and R), writing
// to the store and posting desktop notifications for actionable events; the view
// re-reads the store on its own ticker so new events appear live.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"watchgh/internal/github"
	"watchgh/internal/ingest"
	"watchgh/internal/store"
	"watchgh/internal/timeline"

	"golang.org/x/sys/unix"
)

// Terminal control sequences.
const (
	altEnter   = "\x1b[?1049h" // switch to the alternate screen buffer
	altExit    = "\x1b[?1049l"
	curHide    = "\x1b[?25l"
	curShow    = "\x1b[?25h"
	cursorHome = "\x1b[H"
	clearEOL   = "\x1b[K" // erase to end of line
	clearEOS   = "\x1b[J" // erase to end of screen
	reset      = "\x1b[0m"
	dimSeq     = "\x1b[2m"

	// Three distinct bar styles (256-color bg + bright-white fg so they read on
	// both light and dark terminals): the title/footer chrome, the selected
	// row, and the active tab each get their own hue.
	styChrome = "\x1b[48;5;24;97m"  // blue — title bar + footer status bar
	stySelect = "\x1b[48;5;238;97m" // grey — the selected row
	styTab    = "\x1b[48;5;53;97m"  // magenta — the active tab chip
)

// refreshTick is how often the viewer re-reads the store to pick up events the
// background poll wrote. Cheap: a single indexed query against a local SQLite file.
const refreshTick = 2 * time.Second

func runTUI(ctx context.Context) error {
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	_ = ingest.Backfill(st) // one-off cleanup of pre-fix rows

	// Only the store is opened up front (local, instant). The GitHub client —
	// including resolving the token via `gh auth token` — the viewer login, and
	// the first sync all happen in the background (see run), so nothing off the
	// machine gates the first paint.
	return (&tui{ctx: ctx, st: st}).run()
}

type tui struct {
	ctx    context.Context
	st     *store.Store
	c      *github.Client
	viewer string

	all     []timeline.Event // every stored event, newest first (unfiltered)
	prs     []timeline.Event // Mine tab: one synthesized row per open PR
	events  []timeline.Event // the active tab's visible rows
	active  int              // current tab index
	pos     []int            // remembered selection index, per tab
	sel     int              // index of the selected row (0 = top = newest)
	top     int              // index of the first visible row (scroll offset)
	rows    int              // terminal height
	cols    int              // terminal width
	status  string           // footer message (last action + warnings)
	syncing bool             // a background sync is in flight (shown in the title bar)
}

// mineTab is the index of "My PRs" in tabDefs; it is sourced from the open-PR
// roster (one row per open PR) rather than from a filter over stored events.
const mineTab = 1

// tabDefs are the timeline lenses, switched with 1-4 / Tab, ordered by priority:
// what others need from you, your own PRs, then history and CI detail.
// Inbox/Read/CI are filters over stored events: CI holds every CI/merge event,
// then the non-mine human activity splits into unread (Inbox) and handled
// (Read). "My PRs" is special — its rows come from the open-PR roster (see
// tabRows), so its filter never matches and mine notification events fold into
// their PR's roster row instead.
var tabDefs = []struct {
	name string
	show func(timeline.Event) bool
}{
	{"Inbox", func(e timeline.Event) bool { return e.Unread && !e.IsMine && !isCI(e) }},
	{"My PRs", func(timeline.Event) bool { return false }},
	{"Read", func(e timeline.Event) bool { return !e.Unread && !e.IsMine && !isCI(e) }},
	{"CI", func(e timeline.Event) bool { return isCI(e) }},
}

// isCI marks the tracked-PR engine's events (CI status + blocked/clean), which
// carry Source "graphql"; notification-backed items are the human activity.
func isCI(e timeline.Event) bool { return e.Source == "graphql" }

func (t *tui) run() error {
	fd := int(os.Stdin.Fd())
	old, err := makeRaw(fd)
	if err != nil {
		return fmt.Errorf("entering raw mode: %w", err)
	}
	defer restoreTerm(fd, old)

	fmt.Fprint(os.Stdout, altEnter+curHide)
	defer fmt.Fprint(os.Stdout, curShow+altExit)

	t.pos = make([]int, len(tabDefs))
	t.resize()
	t.reload()
	t.status = "1-4/⇥ tabs · ↑↓ move · ⏎ open · r read · R sync · q quit"
	t.syncing = true // the launch sync (kicked off below) is already in flight
	t.draw()

	keys := make(chan keyEvent, 16)
	go readKeys(os.Stdin, keys)

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

	// Freshen in the background now that the store has already been drawn; the
	// first sync also builds the GitHub client (resolving the token) and the
	// viewer login the footer shows. Silent: no notifications on the backlog.
	syncDone := make(chan syncResult, 1)
	go t.sync(t.c, t.viewer, false, syncDone)

	tick := time.NewTicker(refreshTick)
	defer tick.Stop()

	// Poll GitHub in the background on its own cadence so an open wgh stays
	// fresh and fires desktop notifications without a separate daemon.
	poll := time.NewTicker(cfg.PollFloor)
	defer poll.Stop()

	for {
		select {
		case <-t.ctx.Done():
			return nil
		case <-winch:
			t.resize()
			t.draw()
		case <-tick.C:
			if t.reload() {
				t.draw()
			}
		case <-poll.C:
			if !t.syncing { // skip if the previous poll is still running
				t.syncing = true
				go t.sync(t.c, t.viewer, true, syncDone)
				t.draw()
			}
		case res := <-syncDone:
			t.applySync(res)
			t.draw()
		case k, ok := <-keys:
			if !ok {
				return nil
			}
			if t.handle(k, syncDone) {
				return nil
			}
			t.draw()
		}
	}
}

// handle applies a keypress and reports whether the TUI should quit.
func (t *tui) handle(k keyEvent, syncDone chan<- syncResult) bool {
	switch k.kind {
	case keyQuit:
		return true
	case keyUp:
		t.move(-1)
	case keyDown:
		t.move(1)
	case keyTop:
		t.sel = 0
	case keyBottom:
		t.sel = len(t.events) - 1
	case keyPageUp:
		t.move(-t.bodyHeight())
	case keyPageDown:
		t.move(t.bodyHeight())
	case keyOpen:
		t.act(true)
	case keyRead:
		t.act(false)
	case keySync:
		t.syncing = true
		go t.sync(t.c, t.viewer, false, syncDone)
	case keyTab1:
		t.switchTab(0)
	case keyTab2:
		t.switchTab(1)
	case keyTab3:
		t.switchTab(2)
	case keyTab4:
		t.switchTab(3)
	case keyTabNext:
		t.cycleTab()
	}
	return false
}

// switchTab saves the current selection, activates tab i, and restores its own.
func (t *tui) switchTab(i int) {
	if i < 0 || i >= len(tabDefs) || i == t.active {
		return
	}
	t.pos[t.active] = t.sel
	t.active = i
	t.sel = t.pos[i]
	t.applyFilter()
}

func (t *tui) cycleTab() { t.switchTab((t.active + 1) % len(tabDefs)) }

// count is how many rows tab i holds (for the tab-bar badges).
func (t *tui) count(i int) int { return len(t.tabRows(i)) }

func (t *tui) move(delta int) {
	t.sel += delta
	if t.sel < 0 {
		t.sel = 0
	}
	if t.sel >= len(t.events) {
		t.sel = len(t.events) - 1
	}
}

// act opens (or just marks read) the selected event, then reloads.
func (t *tui) act(open bool) {
	if t.sel < 0 || t.sel >= len(t.events) {
		return
	}
	e := t.events[t.sel]
	if err := applyMark(t.ctx, t.st, e, open); err != nil {
		t.status = "⚠ " + err.Error()
	} else if open {
		t.status = "opened " + timeline.ShortRef(e.Repo, e.Number)
	} else {
		t.status = "marked read " + timeline.ShortRef(e.Repo, e.Number)
	}
	t.reload()
}

// syncResult carries a finished background sync back to the main loop, which
// owns every tui field — so the sync goroutine never writes the view directly.
type syncResult struct {
	client *github.Client // the built client (nil unless this run created it)
	viewer string         // the resolved login (empty when already known or on failure)
	warn   string         // footer warning; empty on success (a clean sync stays silent)
}

// sync polls GitHub once off the input path — building the client (which
// resolves the token) on the first run when it's still nil, and resolving the
// viewer login likewise — and reports the outcome on done for the main loop to
// fold in. The store it writes is what the next reload picks up. When notify is
// set it posts desktop notifications for freshly-arrived actionable events.
func (t *tui) sync(c *github.Client, viewer string, notify bool, done chan<- syncResult) {
	if c == nil {
		nc, err := github.New()
		if err != nil {
			done <- syncResult{warn: "⚠ " + err.Error()}
			return
		}
		c = nc
	}
	if viewer == "" {
		v, err := c.Viewer(t.ctx)
		if err != nil {
			done <- syncResult{client: c, warn: "⚠ sync: " + err.Error()}
			return
		}
		viewer = v.Login
	}
	nf, nerr := syncNotifications(t.ctx, c, t.st, viewer)
	tf, terr := syncTracked(t.ctx, c, t.st, viewer)
	rerr := syncReviews(t.ctx, c, t.st)
	t.st.Prune(cfg.Retention)
	// Only the timer-driven poll notifies: on launch we'd alert on the whole
	// backlog, and on a manual R you're already looking at the screen.
	if notify {
		notifyActionable(append(nf, tf...))
	}
	done <- syncResult{client: c, viewer: viewer, warn: syncWarning(nerr, terr, rerr)}
}

// syncWarning returns the first sync failure as a footer message, or "" when
// every poll succeeded — a clean sync leaves the footer menu untouched.
func syncWarning(nerr, terr, rerr error) string {
	switch {
	case nerr != nil:
		return "⚠ sync: " + nerr.Error()
	case terr != nil:
		return "⚠ tracked-PR sync: " + terr.Error()
	case rerr != nil:
		return "⚠ review-state sync: " + rerr.Error()
	default:
		return ""
	}
}

// applySync folds a finished background sync into the view: clear the syncing
// marker, cache the built client and resolved viewer login so later syncs reuse
// them, surface any warning (a clean sync stays silent), and re-read the store.
func (t *tui) applySync(res syncResult) {
	t.syncing = false
	if res.client != nil {
		t.c = res.client
	}
	if res.viewer != "" {
		t.viewer = res.viewer
	}
	if res.warn != "" {
		t.status = res.warn
	}
	t.reload()
}

// reload re-reads the store (newest first) and reports whether the visible set
// changed, so the ticker only repaints when there's something new.
func (t *tui) reload() bool {
	stored, err := t.st.List()
	if err != nil {
		t.status = "⚠ " + err.Error()
		return false
	}
	sort.SliceStable(stored, func(i, j int) bool {
		return stored[i].TS.After(stored[j].TS)
	})
	before := signature(t.all)
	t.all = stored
	t.prs = t.mineRoster()
	t.applyFilter()
	return before != signature(t.all)
}

// tabRows returns the row set backing tab i: the open-PR roster for Mine, else
// the tab's filter applied over all stored events.
func (t *tui) tabRows(i int) []timeline.Event {
	if i == mineTab {
		return t.prs
	}
	show := tabDefs[i].show
	out := make([]timeline.Event, 0, len(t.all))
	for _, e := range t.all {
		if show(e) {
			out = append(out, e)
		}
	}
	return out
}

// applyFilter recomputes the visible slice for the active tab and clamps sel.
func (t *tui) applyFilter() {
	t.events = t.tabRows(t.active)
	t.clampSel()
}

// mineRoster builds one row per open PR: its latest activity event when there
// is one, otherwise a synthesized status row so silent PRs still appear.
func (t *tui) mineRoster() []timeline.Event {
	prs, err := t.st.OpenPRs()
	if err != nil {
		return nil
	}
	latest := latestMineByPR(t.all)
	out := make([]timeline.Event, 0, len(prs))
	for _, pr := range prs {
		if e, ok := latest[pr.Key]; ok {
			out = append(out, e)
		} else {
			out = append(out, synthPR(pr))
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TS.After(out[j].TS) })
	return out
}

// latestMineByPR indexes the most recent event on each of my PRs by repo#num.
func latestMineByPR(all []timeline.Event) map[string]timeline.Event {
	m := make(map[string]timeline.Event)
	for _, e := range all {
		if !e.IsMine {
			continue
		}
		k := fmt.Sprintf("%s#%d", e.Repo, e.Number)
		if cur, ok := m[k]; !ok || e.TS.After(cur.TS) {
			m[k] = e
		}
	}
	return m
}

// synthPR renders an open PR that has no timeline event yet as a status row:
// the CI/merge state picks the badge; attention states (blocked, CI failed) pop
// as unread while healthy PRs stay quiet.
func synthPR(pr store.OpenPR) timeline.Event {
	e := timeline.Event{
		Source: "graphql", Repo: pr.Repo, Number: pr.Number, URL: pr.URL,
		TS: pr.UpdatedAt, IsMine: true, Detail: pr.Title, Kind: timeline.KindOpenPR,
	}
	switch {
	case pr.MergeState == "BLOCKED" || pr.MergeState == "DIRTY":
		e.Kind, e.Unread, e.Actionable = timeline.KindBlocked, true, true
	case pr.CIState == "FAILURE" || pr.CIState == "ERROR":
		e.Kind, e.Unread, e.Actionable = timeline.KindCIFailed, true, true
	case pr.CIState == "SUCCESS":
		e.Kind = timeline.KindCIPassed
	}
	return e
}

func (t *tui) clampSel() {
	if t.sel >= len(t.events) {
		t.sel = len(t.events) - 1
	}
	if t.sel < 0 {
		t.sel = 0
	}
}

// signature is a cheap fingerprint of what's on screen: count, newest seq, and
// unread tally. If it's unchanged, a ticker reload need not repaint.
func signature(events []timeline.Event) string {
	unread := 0
	var newest int64
	for _, e := range events {
		if e.Unread {
			unread++
		}
		if e.Seq > newest {
			newest = e.Seq
		}
	}
	return fmt.Sprintf("%d/%d/%d", len(events), newest, unread)
}

func (t *tui) bodyHeight() int {
	h := t.rows - 3 // title + tab bar + footer
	if h < 1 {
		return 1
	}
	return h
}

func (t *tui) draw() {
	body := t.bodyHeight()
	// Keep the selection within the viewport.
	if t.sel < t.top {
		t.top = t.sel
	}
	if t.sel >= t.top+body {
		t.top = t.sel - body + 1
	}
	if max := len(t.events) - body; t.top > max {
		t.top = max
	}
	if t.top < 0 {
		t.top = 0
	}

	var b strings.Builder
	b.WriteString(cursorHome)
	b.WriteString(t.titleBar() + clearEOL + "\r\n")
	b.WriteString(t.header() + clearEOL + "\r\n")

	if len(t.events) == 0 {
		b.WriteString("  ✓ nothing in this tab" + clearEOL + "\r\n")
		for i := 1; i < body; i++ {
			b.WriteString(clearEOL + "\r\n")
		}
	} else {
		for i := 0; i < body; i++ {
			idx := t.top + i
			if idx < len(t.events) {
				b.WriteString(t.rowText(t.events[idx], idx == t.sel))
			}
			b.WriteString(clearEOL + "\r\n")
		}
	}

	b.WriteString(t.footer() + clearEOL)
	b.WriteString(clearEOS)
	fmt.Fprint(os.Stdout, b.String())
}

// titleBar is the app name, a full-width blue bar across the top. A background
// sync shows a "⟳ syncing" marker here so the footer's keybinding menu stays put.
func (t *tui) titleBar() string {
	title := " ◆ watchgh — GitHub activity timeline"
	if t.syncing {
		title += "  ⟳ syncing"
	}
	return styChrome + padANSI(title, t.cols) + reset
}

// header draws the tab bar: the active tab reversed, each with its live count.
func (t *tui) header() string {
	var b strings.Builder
	for i, d := range tabDefs {
		seg := fmt.Sprintf(" %d %s %d ", i+1, d.name, t.count(i))
		if i == t.active {
			b.WriteString(styTab + seg + reset)
		} else {
			b.WriteString(dimSeq + seg + reset)
		}
	}
	return truncateANSI(b.String(), t.cols)
}

// footer is a status bar: keybinds + position on the blue chrome background,
// with the viewer login trailing plain (no background) at the right edge.
func (t *tui) footer() string {
	pos := ""
	if len(t.events) > 0 {
		pos = fmt.Sprintf(" [%d/%d]", t.sel+1, len(t.events))
	}
	status := fmt.Sprintf(" %s%s", t.status, pos)
	login := t.viewer
	if login == "" {
		login = "…" // not resolved yet; the first background sync fills it in
	}
	user := fmt.Sprintf(" @%s ", login)

	uw := utf8.RuneCountInString(user)
	if uw > t.cols {
		uw = t.cols
	}
	bar := styChrome + padANSI(status, t.cols-uw) + reset
	return bar + dimSeq + padANSI(user, uw) + reset
}

// rowText renders one timeline row to fit the width. The selected row is drawn
// plain on the grey highlight background across the full width; others reuse the
// colored CLI renderer.
func (t *tui) rowText(e timeline.Event, selected bool) string {
	o := timeline.RenderOpts{Now: time.Now(), StaleAfter: cfg.StaleAfter, Color: !selected, LeadWithPR: true}
	row := timeline.RenderRow(e, o)
	if selected {
		return stySelect + padANSI(row, t.cols) + reset
	}
	return truncateANSI(row, t.cols)
}

// --- terminal I/O ---------------------------------------------------------

func makeRaw(fd int) (*unix.Termios, error) {
	t, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if err != nil {
		return nil, err
	}
	old := *t
	raw := *t
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP |
		unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Lflag &^= unix.ECHO | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8
	// VMIN=1/VTIME=0: block until at least one byte arrives. A timed read
	// (VTIME>0) would return 0 bytes when idle, which Go's os.File.Read reports
	// as io.EOF — closing the key channel and quitting the TUI on its own.
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, unix.TIOCSETA, &raw); err != nil {
		return nil, err
	}
	return &old, nil
}

func restoreTerm(fd int, t *unix.Termios) {
	if t != nil {
		_ = unix.IoctlSetTermios(fd, unix.TIOCSETA, t)
	}
}

func (t *tui) resize() {
	ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ)
	if err != nil || ws.Row == 0 {
		t.rows, t.cols = 24, 80 // sane fallback
		return
	}
	t.rows, t.cols = int(ws.Row), int(ws.Col)
}

type keyKind int

const (
	keyNone keyKind = iota
	keyUp
	keyDown
	keyTop
	keyBottom
	keyPageUp
	keyPageDown
	keyOpen
	keyRead
	keySync
	keyTab1
	keyTab2
	keyTab3
	keyTab4
	keyTabNext
	keyQuit
)

type keyEvent struct{ kind keyKind }

// readKeys parses stdin into key events until the stream closes.
func readKeys(f *os.File, out chan<- keyEvent) {
	defer close(out)
	buf := make([]byte, 16)
	for {
		n, err := f.Read(buf)
		if err != nil {
			return
		}
		for _, k := range parseKeys(buf[:n]) {
			out <- k
		}
	}
}

func parseKeys(b []byte) []keyEvent {
	var out []keyEvent
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case c == 0x1b && i+2 < len(b) && b[i+1] == '[':
			out = append(out, csiKey(b[i+2]))
			i += 2
		case c == 0x1b: // lone Escape
			out = append(out, keyEvent{keyQuit})
		case c == '\r' || c == '\n':
			out = append(out, keyEvent{keyOpen})
		case c == 0x03: // Ctrl-C
			out = append(out, keyEvent{keyQuit})
		case c == '\t': // cycle to the next tab
			out = append(out, keyEvent{keyTabNext})
		default:
			out = append(out, runeKey(rune(c)))
		}
	}
	return out
}

func csiKey(final byte) keyEvent {
	switch final {
	case 'A':
		return keyEvent{keyUp}
	case 'B':
		return keyEvent{keyDown}
	case 'H':
		return keyEvent{keyTop}
	case 'F':
		return keyEvent{keyBottom}
	case '5': // PageUp arrives as ESC [ 5 ~ ; the ~ is harmlessly dropped
		return keyEvent{keyPageUp}
	case '6':
		return keyEvent{keyPageDown}
	default:
		return keyEvent{keyNone}
	}
}

func runeKey(r rune) keyEvent {
	switch r {
	case 'k':
		return keyEvent{keyUp}
	case 'j':
		return keyEvent{keyDown}
	case 'g':
		return keyEvent{keyTop}
	case 'G':
		return keyEvent{keyBottom}
	case 'r':
		return keyEvent{keyRead}
	case 'R':
		return keyEvent{keySync}
	case 'q':
		return keyEvent{keyQuit}
	case '1':
		return keyEvent{keyTab1}
	case '2':
		return keyEvent{keyTab2}
	case '3':
		return keyEvent{keyTab3}
	case '4':
		return keyEvent{keyTab4}
	default:
		return keyEvent{keyNone}
	}
}

// --- width-aware string helpers (ANSI escapes count as zero width) ---------

// truncateANSI cuts s to w visible cells, copying escape sequences verbatim and
// closing with a reset so a clipped color can't bleed into the next line.
func truncateANSI(s string, w int) string {
	if w <= 0 {
		return ""
	}
	var b strings.Builder
	vis := 0
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		if rs[i] == 0x1b {
			j := i
			for j < len(rs) && rs[j] != 'm' {
				j++
			}
			if j < len(rs) {
				j++
			}
			b.WriteString(string(rs[i:j]))
			i = j - 1
			continue
		}
		if vis >= w {
			b.WriteString(reset)
			return b.String()
		}
		b.WriteRune(rs[i])
		vis++
	}
	return b.String()
}

// padANSI pads (or truncates) plain text to exactly w cells, so a reverse-video
// selection bar spans the full row. Callers pass color-free text.
func padANSI(s string, w int) string {
	n := utf8.RuneCountInString(s)
	if n > w {
		return string([]rune(s)[:w])
	}
	return s + strings.Repeat(" ", w-n)
}
