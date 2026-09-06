// The interactive timeline: bare `watchgit` on an alt-screen, arrow keys to
// move, Enter to open (marking read), r to mark read, R to re-sync, q to quit.
// It's a pure viewer over the store — the daemon (or an initial one-shot sync)
// fills it — that re-reads on a ticker so daemon-fed events appear live.
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

	"watchgit/internal/github"
	"watchgit/internal/store"
	"watchgit/internal/timeline"

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
	reverse    = "\x1b[7m"
	reset      = "\x1b[0m"
	dimSeq     = "\x1b[2m"
)

// refreshTick is how often the viewer re-reads the store to pick up events the
// daemon wrote. Cheap: a single indexed query against a local SQLite file.
const refreshTick = 2 * time.Second

func runTUI(ctx context.Context) error {
	c, st, viewer, err := setup(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	// Freshen once at launch so the timeline is useful even with no daemon.
	syncNotifications(ctx, c, st, viewer)
	syncTracked(ctx, c, st, viewer)
	st.Prune(cfg.Retention)

	return (&tui{ctx: ctx, st: st, c: c, viewer: viewer}).run()
}

type tui struct {
	ctx    context.Context
	st     *store.Store
	c      *github.Client
	viewer string

	all    []timeline.Event // every stored event, newest first (unfiltered)
	events []timeline.Event // the active tab's filtered view
	active int              // current tab index
	pos    []int            // remembered selection index, per tab
	sel    int              // index of the selected row (0 = top = newest)
	top    int              // index of the first visible row (scroll offset)
	rows   int              // terminal height
	cols   int              // terminal width
	status string           // footer message (last action, sync state, warnings)
}

// tabDefs are the timeline lenses, switched with 1-4 / Tab. Together they
// partition the store: every unread item lands in exactly one of Inbox/Mine/CI
// by (mine?, CI?), and Read collects everything already handled.
var tabDefs = []struct {
	name string
	show func(timeline.Event) bool
}{
	{"Inbox", func(e timeline.Event) bool { return e.Unread && !e.IsMine && !isCI(e) }},
	{"Read", func(e timeline.Event) bool { return !e.Unread }},
	{"Mine", func(e timeline.Event) bool { return e.Unread && e.IsMine && !isCI(e) }},
	{"CI", func(e timeline.Event) bool { return e.Unread && isCI(e) }},
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
	t.draw()

	keys := make(chan keyEvent, 16)
	go readKeys(os.Stdin, keys)

	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

	refreshed := make(chan struct{}, 1)
	tick := time.NewTicker(refreshTick)
	defer tick.Stop()

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
		case <-refreshed:
			t.reload()
			t.draw()
		case k, ok := <-keys:
			if !ok {
				return nil
			}
			if t.handle(k, refreshed) {
				return nil
			}
			t.draw()
		}
	}
}

// handle applies a keypress and reports whether the TUI should quit.
func (t *tui) handle(k keyEvent, refreshed chan<- struct{}) bool {
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
		t.status = "syncing…"
		go t.sync(refreshed)
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

// count is how many stored events fall under tab i (for the tab-bar badges).
func (t *tui) count(i int) int {
	n := 0
	for _, e := range t.all {
		if tabDefs[i].show(e) {
			n++
		}
	}
	return n
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
		t.status = "⚠ " + err.Error()
	} else if open {
		t.status = "opened " + timeline.ShortRef(e.Repo, e.Number)
	} else {
		t.status = "marked read " + timeline.ShortRef(e.Repo, e.Number)
	}
	t.reload()
}

// sync polls GitHub once (off the input path) and signals a redraw when done.
func (t *tui) sync(refreshed chan<- struct{}) {
	_, nerr := syncNotifications(t.ctx, t.c, t.st, t.viewer)
	_, terr := syncTracked(t.ctx, t.c, t.st, t.viewer)
	t.st.Prune(cfg.Retention)
	switch {
	case nerr != nil:
		t.status = "⚠ sync: " + nerr.Error()
	case terr != nil:
		t.status = "⚠ tracked-PR sync: " + terr.Error()
	default:
		t.status = "synced " + time.Now().Format("15:04:05")
	}
	select {
	case refreshed <- struct{}{}:
	default:
	}
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
	t.applyFilter()
	return before != signature(t.all)
}

// applyFilter recomputes the visible slice for the active tab and clamps sel.
func (t *tui) applyFilter() {
	show := tabDefs[t.active].show
	view := make([]timeline.Event, 0, len(t.all))
	for _, e := range t.all {
		if show(e) {
			view = append(view, e)
		}
	}
	t.events = view
	t.clampSel()
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
	h := t.rows - 2 // header + footer
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

// header draws the tab bar: the active tab reversed, each with its live count.
func (t *tui) header() string {
	var b strings.Builder
	for i, d := range tabDefs {
		seg := fmt.Sprintf(" %d %s %d ", i+1, d.name, t.count(i))
		if i == t.active {
			b.WriteString(reverse + seg + reset)
		} else {
			b.WriteString(dimSeq + seg + reset)
		}
	}
	return truncateANSI(b.String(), t.cols)
}

func (t *tui) footer() string {
	pos := ""
	if len(t.events) > 0 {
		pos = fmt.Sprintf(" [%d/%d]", t.sel+1, len(t.events))
	}
	line := fmt.Sprintf(" %s%s · @%s", t.status, pos, t.viewer)
	return dimSeq + truncateANSI(line, t.cols) + reset
}

// rowText renders one timeline row to fit the width. The selected row is drawn
// plain and reverse-video across the full width; others reuse the colored CLI
// renderer, so the two surfaces stay pixel-identical.
func (t *tui) rowText(e timeline.Event, selected bool) string {
	o := timeline.RenderOpts{Now: time.Now(), StaleAfter: cfg.StaleAfter, Color: !selected}
	row := timeline.RenderRow(e, o)
	if selected {
		return reverse + padANSI(row, t.cols) + reset
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
