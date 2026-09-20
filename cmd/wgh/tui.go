// The interactive timeline: bare `wgh` on an alt-screen, arrow keys to
// move, Enter to open (marking read), r to mark read, R to re-sync, q to quit.
// The view re-reads the store on a ticker so events written by the launchd
// background poller appear live, and refreshes on launch and R. When that
// poller isn't running the view self-polls GitHub (and notifies) instead, so a
// single actor always owns notifications.
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

	"watchgh/internal/agent"
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
	styOK     = "\x1b[32m"          // green — the "poller live" status dot
)

// refreshTick is how often the viewer re-reads the store to pick up events the
// background poll wrote. Cheap: a single indexed query against a local SQLite file.
const refreshTick = 2 * time.Second

// statusLinger is how long a transient footer confirmation ("opened …") stays up
// before the bar reverts to the keybinding menu.
const statusLinger = 10 * time.Second

// menuHint is the idle footer — the keybinding legend shown whenever no transient
// status is up.
const menuHint = "1-4/⇥ tabs · ↑↓ move · ⏎ open · r read · R sync · q quit"

func runTUI(ctx context.Context) error {
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	_ = ingest.Backfill(ctx, st) // one-off cleanup of pre-fix rows

	// Only the store is opened up front (local, instant). The GitHub client —
	// including resolving the token via `gh auth token` — the viewer login, and
	// the first sync all happen in the background (see run), so nothing off the
	// machine gates the first paint.
	t := &tui{model: model{ctx: ctx, st: st}}

	// Ensure the launchd background poller is installed and running so
	// notifications keep flowing after this window closes. Best-effort: if it
	// can't be set up, the view still self-polls while it's open (see run).
	if err := agent.Ensure(); err != nil {
		t.status = "⚠ background poller: " + err.Error()
	}
	t.daemonUp = agent.Running()
	return t.run()
}

// model is the store-backed data the view draws: every stored event, the
// open-PR roster, and the per-tab badge counts. It owns reading from the store
// and recomputing these; the surrounding tui adds only view and input state, so
// the two concerns — what to show vs. how it's shown — live apart.
type model struct {
	ctx context.Context
	st  *store.Store

	all    []timeline.Event // every stored event, newest first (unfiltered)
	prs    []timeline.Event // Mine tab: one synthesized row per open PR
	ci     []timeline.Event // CI tab: one row per tracked PR, its latest CI/merge event
	counts []int            // per-tab row count for the tab-bar badges (recomputed on load)
}

type tui struct {
	model
	c      *github.Client
	viewer string

	events   []timeline.Event // the active tab's visible rows
	active   int              // current tab index
	pos      []int            // remembered selection index, per tab
	sel      int              // index of the selected row (0 = top = newest)
	top      int              // index of the first visible row (scroll offset)
	rows     int              // terminal height
	cols     int              // terminal width
	status   string           // footer message (last action + warnings)
	syncing  bool             // a background sync is in flight (shown in the title bar)
	daemonUp bool             // the launchd poller is running (it owns notifications)

	statusTimer *time.Timer // reverts a transient status to menuHint after statusLinger
}

// mineTab is the index of "My PRs" in tabDefs; it is sourced from the open-PR
// roster (one row per open PR) rather than from a filter over stored events.
const mineTab = 1

// ciTab is the index of the "CI" tab; like Mine it is collapsed to one row per
// PR (the latest CI/merge event) rather than a running history of transitions.
const ciTab = 3

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
	t.counts = make([]int, len(tabDefs))
	// A stopped timer, ready to Reset when a transient status is shown. Created
	// before the first reload so setStatus is safe to call from anywhere below.
	t.statusTimer = time.NewTimer(time.Hour)
	t.statusTimer.Stop()
	t.resize()
	t.reload()
	if t.status == "" { // keep any launch warning (e.g. poller install failed)
		t.status = menuHint
	}
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

	// When the launchd poller is up it owns polling and notifications, so the
	// view just re-reads the store it writes (via tick) and refreshes on launch
	// and R. Only when the poller is down does the view self-poll GitHub — and
	// then it notifies, so a single actor is ever the notifier. The ticker always
	// runs; each tick re-samples whether the poller is live and self-polls only
	// while it's down, so a poller that starts or dies mid-session can never make
	// the view double-notify or fall silent.
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
			if up := agent.Running(); up != t.daemonUp {
				t.daemonUp = up
				t.draw() // reflect the footer dot when the poller starts/stops
			}
			if !t.daemonUp && !t.syncing { // self-poll only while the poller is down
				t.syncing = true
				go t.sync(t.c, t.viewer, true, syncDone)
				t.draw()
			}
		case res := <-syncDone:
			t.applySync(res)
			t.draw()
		case <-t.statusTimer.C:
			t.status = menuHint // a transient confirmation has lingered long enough
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
		if !t.syncing { // ignore R while a sync is already in flight
			t.syncing = true
			go t.sync(t.c, t.viewer, false, syncDone)
		}
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

// count is how many rows tab i holds (for the tab-bar badges), served from the
// cache recount fills on load rather than rebuilding a filtered slice per frame.
func (m *model) count(i int) int { return m.counts[i] }

// recount refreshes the per-tab badge counts without allocating a filtered slice
// per tab: Mine and CI are their collapsed roster lengths, the rest count matches
// over stored events.
func (m *model) recount() {
	for i, d := range tabDefs {
		switch i {
		case mineTab:
			m.counts[i] = len(m.prs)
			continue
		case ciTab:
			m.counts[i] = len(m.ci)
			continue
		}
		n := 0
		for _, e := range m.all {
			if d.show(e) {
				n++
			}
		}
		m.counts[i] = n
	}
}

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
		t.setStatus("⚠ " + err.Error())
	} else if open {
		t.setStatus("opened " + timeline.ShortRef(e.Repo, e.Number))
	} else {
		t.setStatus("marked read " + timeline.ShortRef(e.Repo, e.Number))
	}
	t.reload()
}

// setStatus shows a transient footer message and schedules a revert to menuHint
// after statusLinger, so an action's confirmation doesn't sit in the bar forever.
// The drain guards a Reset against a fire already queued on the channel; safe
// because setStatus only runs on the main loop, never concurrently with its read.
func (t *tui) setStatus(msg string) {
	t.status = msg
	if !t.statusTimer.Stop() {
		select {
		case <-t.statusTimer.C:
		default:
		}
	}
	t.statusTimer.Reset(statusLinger)
}

// syncResult carries a background sync back to the main loop, which owns every
// tui field — so the sync goroutine never writes the view directly. The first
// run emits two: an early partial the moment the client and viewer login are
// resolved (so the footer stops showing @… without waiting out the full poll),
// then the final one when the notification/PR/review sync completes.
type syncResult struct {
	client  *github.Client // the built client (nil unless this run created it)
	viewer  string         // the resolved login (empty when already known or on failure)
	warn    string         // footer warning; empty on success (a clean sync stays silent)
	partial bool           // client/viewer only — the full sync is still running
}

// sync polls GitHub once off the input path — building the client (which
// resolves the token) on the first run when it's still nil, and resolving the
// viewer login likewise — and reports the outcome on done for the main loop to
// fold in. The store it writes is what the next reload picks up. When notify is
// set it posts desktop notifications for freshly-arrived actionable events.
func (t *tui) sync(c *github.Client, viewer string, notify bool, done chan<- syncResult) {
	fresh := false // did this run resolve the client or viewer for the first time?
	if c == nil {
		nc, err := github.New()
		if err != nil {
			done <- syncResult{warn: "⚠ " + err.Error()}
			return
		}
		c, fresh = nc, true
	}
	if viewer == "" {
		v, err := c.Viewer(t.ctx)
		if err != nil {
			done <- syncResult{client: c, warn: "⚠ sync: " + err.Error()}
			return
		}
		viewer, fresh = v.Login, true
	}
	// Paint the resolved client and login right away — the footer shows @… until
	// this lands — rather than holding them behind the poll below. Buffered(1)
	// done plus the send-blocks-until-received handoff guarantees the main loop
	// sees this partial before the final result.
	if fresh {
		done <- syncResult{client: c, viewer: viewer, partial: true}
	}
	_, nerr := syncNotifications(t.ctx, c, t.st, viewer)
	_, terr := syncTracked(t.ctx, c, t.st, viewer)
	rerr := syncReviews(t.ctx, c, t.st)
	t.st.Prune(t.ctx, cfg.Retention)
	// Only the timer-driven self-poll (when the launchd poller is down) notifies:
	// on launch we'd alert on the whole backlog, and on a manual R you're already
	// looking at the screen. The silent passes still refresh the baseline so the
	// next real alert compares against the true standing backlog.
	notifyBacklog(t.ctx, t.st, notify)
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

// applySync folds a background sync into the view. A partial result only caches
// the built client and resolved login (painting the footer immediately) and
// leaves the syncing marker up. The final result clears the marker, surfaces any
// warning (a clean sync stays silent), and re-reads the store.
func (t *tui) applySync(res syncResult) {
	if res.client != nil {
		t.c = res.client
	}
	if res.viewer != "" {
		t.viewer = res.viewer
	}
	if res.partial {
		return // client/viewer painted; the full sync is still in flight
	}
	t.syncing = false
	if res.warn != "" {
		t.status = res.warn
	}
	t.daemonUp = agent.Running() // reflect a poller started/stopped elsewhere
	t.reload()
}

// reload re-reads the store into the model and refreshes the visible slice,
// reporting whether the visible set changed so the ticker only repaints when
// there's something new. A store error surfaces in the footer.
func (t *tui) reload() bool {
	changed, err := t.model.load()
	if err != nil {
		t.status = "⚠ " + err.Error()
		return false
	}
	t.applyFilter()
	return changed
}

// load re-reads the store (newest first), rebuilds the roster and badge counts,
// and reports whether the event set changed since the last load.
func (m *model) load() (bool, error) {
	stored, err := m.st.List(m.ctx)
	if err != nil {
		return false, err
	}
	sort.SliceStable(stored, func(i, j int) bool {
		return stored[i].TS.After(stored[j].TS)
	})
	before := signature(m.all)
	m.all = stored
	m.prs = m.roster()
	m.ci = m.ciRoster()
	m.recount()
	return before != signature(m.all), nil
}

// tabRows returns the row set backing tab i: the open-PR roster for Mine, the
// collapsed latest-per-PR set for CI, else the tab's filter over all stored events.
func (m *model) tabRows(i int) []timeline.Event {
	switch i {
	case mineTab:
		return m.prs
	case ciTab:
		return m.ci
	}
	show := tabDefs[i].show
	out := make([]timeline.Event, 0, len(m.all))
	for _, e := range m.all {
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

// roster builds one row per open PR: its latest activity event when there
// is one, otherwise a synthesized status row so silent PRs still appear.
func (m *model) roster() []timeline.Event {
	prs, err := m.st.OpenPRs(m.ctx)
	if err != nil {
		return nil
	}
	latest := latestMineByPR(m.all)
	out := make([]timeline.Event, 0, len(prs))
	for _, pr := range prs {
		if e, ok := latest[pr.Key]; ok {
			e.CIState = pr.CIState // carry current CI onto the activity row so the glyph reflects now, not the event
			out = append(out, e)
		} else {
			out = append(out, synthPR(pr))
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TS.After(out[j].TS) })
	return out
}

// ciRoster collapses the CI tab to one row per tracked PR: the latest CI/merge
// (graphql) event for each repo#num. The tab reflects each PR's current build and
// merge status, not a running history, so earlier transitions fold away.
func (m *model) ciRoster() []timeline.Event {
	latest := make(map[string]timeline.Event)
	for _, e := range m.all {
		if !isCI(e) {
			continue
		}
		k := fmt.Sprintf("%s#%d", e.Repo, e.Number)
		if cur, ok := latest[k]; !ok || e.TS.After(cur.TS) {
			latest[k] = e
		}
	}
	out := make([]timeline.Event, 0, len(latest))
	for _, e := range latest {
		out = append(out, e)
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

// synthPR renders an open PR that has no timeline event yet as a status row.
// The badge carries the merge/attention state (blocked vs. plain open PR); CI
// health rides its own glyph column (Event.CIState), so a passing-but-blocked PR
// shows both. Attention states (blocked, CI failed) pop as unread; healthy PRs
// stay quiet.
func synthPR(pr store.OpenPR) timeline.Event {
	e := timeline.Event{
		Source: "graphql", Repo: pr.Repo, Number: pr.Number, URL: pr.URL,
		TS: pr.UpdatedAt, IsMine: true, Detail: pr.Title, Kind: timeline.KindOpenPR,
		CIState: pr.CIState,
	}
	switch {
	case pr.MergeState == "BLOCKED" || pr.MergeState == "DIRTY":
		e.Kind, e.Unread, e.Actionable = timeline.KindBlocked, true, true
	case pr.CIState == "FAILURE" || pr.CIState == "ERROR":
		e.Unread, e.Actionable = true, true // red CI glyph carries the why; flag for attention
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
		// Parenthesize the count so it can't be read as part of the tab index
		// ("1 Inbox 9"), and drop it at zero so an empty tab is just its name.
		seg := fmt.Sprintf(" %d %s ", i+1, d.name)
		if n := t.count(i); n > 0 {
			seg = fmt.Sprintf(" %d %s (%d) ", i+1, d.name, n)
		}
		if i == t.active {
			b.WriteString(styTab + seg + reset)
		} else {
			b.WriteString(dimSeq + seg + reset)
		}
	}
	return truncateANSI(b.String(), t.cols)
}

// footer is a status bar: keybinds + position on the blue chrome background,
// with the poller status dot and viewer login trailing plain (no background) at
// the right edge.
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

	// Background-poller state as a status dot (the systemctl convention): a
	// green ● live when the launchd poller is running, a dim ○ off when not.
	glyph, word, dot := "○", "off ", dimSeq
	if t.daemonUp {
		glyph, word, dot = "●", "live", styOK
	}

	// Width is measured on the plain text; the colored dot is rendered
	// separately so padANSI's rune count stays accurate.
	plain := fmt.Sprintf(" %s %s @%s ", glyph, word, login)
	uw := utf8.RuneCountInString(plain)
	if uw > t.cols {
		uw = t.cols
	}
	user := dot + glyph + reset + dimSeq + fmt.Sprintf(" %s @%s ", word, login) + reset

	bar := styChrome + padANSI(status, t.cols-uw) + reset
	return bar + dimSeq + " " + reset + truncateANSI(user, uw-1)
}

// rowText renders one timeline row to fit the width. The selected row is drawn
// plain on the grey highlight background across the full width; others reuse the
// colored CLI renderer.
func (t *tui) rowText(e timeline.Event, selected bool) string {
	o := timeline.RenderOpts{Now: time.Now(), StaleAfter: cfg.StaleAfter, Color: !selected, LeadWithPR: true, ShowCI: t.active == mineTab}
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

// readKeys parses stdin into key events until the stream closes. A trailing
// incomplete escape sequence (a CSI split across two reads under load) is held
// in carry and prepended to the next read, so an arrow key straddling a buffer
// boundary isn't misdecoded as a lone Escape (quit).
func readKeys(f *os.File, out chan<- keyEvent) {
	defer close(out)
	buf := make([]byte, 16)
	var carry []byte
	for {
		n, err := f.Read(buf)
		if err != nil {
			return
		}
		data := buf[:n]
		if len(carry) > 0 {
			data = append(carry, data...)
		}
		events, leftover := parseKeys(data)
		for _, k := range events {
			out <- k
		}
		// Copy leftover: data may alias buf, which the next Read overwrites.
		carry = append([]byte(nil), leftover...)
	}
}

// parseKeys decodes a byte buffer into key events. It returns any trailing bytes
// that form an incomplete escape sequence (ESC [ … with no final byte yet) so
// the caller can hold them for the next read rather than misfire a quit.
func parseKeys(b []byte) (events []keyEvent, carry []byte) {
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c == 0x1b:
			ev, n, complete := parseEsc(b[i:])
			if !complete {
				return events, b[i:] // hold the incomplete tail for the next read
			}
			if ev.kind != keyNone {
				events = append(events, ev)
			}
			i += n
		case c == '\r' || c == '\n':
			events = append(events, keyEvent{keyOpen})
			i++
		case c == 0x03: // Ctrl-C
			events = append(events, keyEvent{keyQuit})
			i++
		case c == '\t': // cycle to the next tab
			events = append(events, keyEvent{keyTabNext})
			i++
		default:
			events = append(events, runeKey(rune(c)))
			i++
		}
	}
	return events, nil
}

// parseEsc decodes the escape sequence at the start of b (b[0] is ESC). It
// reports the event, bytes consumed, and whether the sequence was complete. A
// bare ESC — or ESC followed by a non-CSI byte — is a real Escape keypress
// (quit), keeping the key responsive; only a started-but-unfinished CSI reports
// complete=false so readKeys can wait for the rest.
func parseEsc(b []byte) (ev keyEvent, n int, complete bool) {
	if len(b) == 1 || b[1] != '[' {
		return keyEvent{keyQuit}, 1, true // lone Escape (consume just the ESC)
	}
	// CSI: ESC [ params… final, where final is any byte in 0x40–0x7e.
	for i := 2; i < len(b); i++ {
		if b[i] >= 0x40 && b[i] <= 0x7e {
			return csiKey(b[2:i], b[i]), i + 1, true
		}
	}
	return keyEvent{keyNone}, 0, false // unfinished CSI; hold for the next read
}

// csiKey maps a CSI sequence's parameter bytes and final byte to a key. It
// handles both the letter-final arrows/Home/End and the tilde-final navigation
// keys (PageUp/Down, Home/End), ignoring any modifier parameters (e.g. "1;5").
func csiKey(params []byte, final byte) keyEvent {
	switch final {
	case 'A':
		return keyEvent{keyUp}
	case 'B':
		return keyEvent{keyDown}
	case 'H':
		return keyEvent{keyTop}
	case 'F':
		return keyEvent{keyBottom}
	case '~':
		switch csiParam(params) {
		case 1, 7: // Home
			return keyEvent{keyTop}
		case 4, 8: // End
			return keyEvent{keyBottom}
		case 5:
			return keyEvent{keyPageUp}
		case 6:
			return keyEvent{keyPageDown}
		}
	}
	return keyEvent{keyNone}
}

// csiParam reads the leading numeric parameter of a CSI sequence (the part
// before any ';' modifier), returning -1 when there isn't one.
func csiParam(params []byte) int {
	n := -1
	for _, c := range params {
		if c < '0' || c > '9' {
			break
		}
		if n < 0 {
			n = 0
		}
		n = n*10 + int(c-'0')
	}
	return n
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
	b.Grow(len(s))
	vis := 0
	inEsc := false // inside an ESC…m sequence, which copies verbatim at zero width
	for _, r := range s {
		if inEsc {
			b.WriteRune(r)
			if r == 'm' {
				inEsc = false
			}
			continue
		}
		if r == 0x1b {
			inEsc = true
			b.WriteRune(r)
			continue
		}
		if vis >= w {
			b.WriteString(reset)
			return b.String()
		}
		b.WriteRune(r)
		vis++
	}
	return b.String()
}

// padANSI pads (or truncates) plain text to exactly w cells, so a reverse-video
// selection bar spans the full row. Callers pass color-free text.
func padANSI(s string, w int) string {
	n := utf8.RuneCountInString(s)
	if n > w {
		count := 0
		for i := range s { // range yields the byte offset of each rune start
			if count == w {
				return s[:i]
			}
			count++
		}
		return s
	}
	return s + strings.Repeat(" ", w-n)
}
