// Command watchgit shows GitHub activity you care about as an arrival-ordered
// timeline: notifications (reviews/assigns/comments) plus tracked-PR CI and
// merge-state transitions, with a live watch mode and desktop notifications.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"watchgit/internal/config"
	"watchgit/internal/daemon"
	"watchgit/internal/github"
	"watchgit/internal/ingest"
	"watchgit/internal/notify"
	"watchgit/internal/store"
	"watchgit/internal/timeline"
	"watchgit/internal/tracker"
)

// cfg holds the user-tunable thresholds, loaded once at startup. Every field
// has a default, so a missing config file is fine.
var cfg = config.Defaults()

func main() {
	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
		args = args[1:]
	}

	if c, err := config.Load(); err != nil {
		fmt.Fprintln(os.Stderr, "watchgit: config:", err, "— using defaults")
	} else {
		cfg = c
	}

	// SIGTERM as well as SIGINT: launchd stops the daemon with SIGTERM on unload.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd {
	case "":
		// Bare `watchgit` opens the interactive timeline when attached to a
		// terminal; piped or redirected, it prints like `list` so scripts work.
		if isTerminal(os.Stdout) && isTerminal(os.Stdin) {
			err = runTUI(ctx)
		} else {
			err = runList(ctx)
		}
	case "list":
		err = runList(ctx)
	case "watch":
		err = runWatch(ctx)
	case "daemon":
		err = runDaemon(ctx, args)
	case "open":
		err = runMark(ctx, args, true)
	case "read":
		err = runMark(ctx, args, false)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "watchgit: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "watchgit:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`watchgit — a GitHub activity timeline

Usage:
  watchgit               Interactive timeline (arrows/⏎/r/q); prints like list when piped
  watchgit list          Print the stored timeline (polls once first)
  watchgit watch         Stream new events live with desktop notifications
  watchgit open <n>      Open item <n> in the browser and mark it read (here + GitHub)
  watchgit read <n>      Mark item <n> read without opening
  watchgit daemon <cmd>  Background poller: install | uninstall | status
  watchgit help          Show this help

The daemon runs the poll loop continuously via launchd, so desktop
notifications fire even with no terminal open; list/watch just read the store.`)
}

func runList(ctx context.Context) error {
	c, st, viewer, err := setup(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	if _, err := syncNotifications(ctx, c, st, viewer); err != nil {
		return err
	}
	if _, err := syncTracked(ctx, c, st, viewer); err != nil {
		fmt.Fprintln(os.Stderr, dim("⚠ tracked-PR poll failed: "+err.Error()))
	}
	if err := st.Prune(cfg.Retention); err != nil {
		return fmt.Errorf("pruning: %w", err)
	}

	stored, err := st.List()
	if err != nil {
		return fmt.Errorf("loading timeline: %w", err)
	}
	out := timeline.Render(stored, renderOpts())
	if out == "" {
		fmt.Println("✓ all caught up — nothing to show")
		return nil
	}
	fmt.Print(out)
	return nil
}

func runWatch(ctx context.Context) error {
	c, st, viewer, err := setup(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	w := newWatcher(c, st, viewer, func(m string) { fmt.Fprintln(os.Stderr, dim("⚠ "+m)) })
	fmt.Print(timeline.Render(w.baseline(ctx), renderOpts()))
	fmt.Println(dim("— watching, Ctrl+C to stop —"))

	for {
		select {
		case <-ctx.Done():
			fmt.Println(dim("— stopped —"))
			return nil
		case <-time.After(w.interval):
		}
		if fresh := w.tick(ctx); len(fresh) > 0 {
			fmt.Print(timeline.Render(fresh, renderOpts()))
			notifyActionable(fresh)
		}
	}
}

// runDaemon dispatches the `daemon` subcommands. `run` is the headless poll loop
// launchd invokes; install/uninstall/status manage the LaunchAgent.
func runDaemon(ctx context.Context, args []string) error {
	sub := "status"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "run":
		return runDaemonLoop(ctx)
	case "install":
		if err := daemon.Install(); err != nil {
			return err
		}
		logPath, _ := daemon.LogPath()
		fmt.Println("✓ watchgit daemon installed and started")
		fmt.Println(dim("  polls in the background and posts desktop notifications, no terminal needed"))
		fmt.Println(dim("  log: " + logPath))
		return nil
	case "uninstall":
		if err := daemon.Uninstall(); err != nil {
			return err
		}
		fmt.Println("✓ watchgit daemon stopped and removed")
		return nil
	case "status":
		s, err := daemon.Status()
		if err != nil {
			return err
		}
		fmt.Println(s)
		return nil
	default:
		return fmt.Errorf("unknown daemon command %q (want run, install, uninstall, or status)", sub)
	}
}

// runDaemonLoop is the always-on poller. It has no terminal: it logs events with
// timestamps (launchd captures this to the log file) and fires notifications.
func runDaemonLoop(ctx context.Context) error {
	lg := log.New(os.Stdout, "", log.LstdFlags)
	c, st, viewer, err := setup(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	w := newWatcher(c, st, viewer, func(m string) { lg.Println("warn:", m) })
	w.baseline(ctx) // silent: no alerts on the pre-existing backlog
	lg.Printf("started; watching as %s", viewer)

	for {
		select {
		case <-ctx.Done():
			lg.Println("stopped")
			return nil
		case <-time.After(w.interval):
		}
		fresh := w.tick(ctx)
		for _, e := range fresh {
			b := e.Kind.Badge()
			lg.Printf("%s %s — %s", b.Label, timeline.ShortRef(e.Repo, e.Number), e.Detail)
		}
		notifyActionable(fresh)
	}
}

// watcher holds the poll-loop state shared by `watch` and the daemon.
type watcher struct {
	c            *github.Client
	st           *store.Store
	viewer       string
	interval     time.Duration
	lastModified string
	warn         func(msg string)
}

func newWatcher(c *github.Client, st *store.Store, viewer string, warn func(string)) *watcher {
	return &watcher{c: c, st: st, viewer: viewer, interval: cfg.PollFloor, warn: warn}
}

// baseline runs the initial silent sync (seeding tracked-PR state without
// alerting on the pre-existing backlog) and returns the stored timeline so far.
func (w *watcher) baseline(ctx context.Context) []timeline.Event {
	syncNotifications(ctx, w.c, w.st, w.viewer)
	syncTracked(ctx, w.c, w.st, w.viewer)
	w.st.Prune(cfg.Retention)
	stored, _ := w.st.List()
	return stored
}

// tick runs one poll cycle and returns freshly-arrived events.
func (w *watcher) tick(ctx context.Context) []timeline.Event {
	var fresh []timeline.Event

	// Notifications: conditional GET as a cheap change detector.
	changed, lm, poll, err := w.c.CheckNotifications(ctx, w.lastModified)
	if err != nil {
		if ctx.Err() == nil {
			w.warn("poll failed, retrying: " + err.Error())
		}
	} else {
		if poll > 0 {
			w.interval = max(poll, cfg.PollFloor)
		}
		if changed {
			w.lastModified = lm
			if nf, err := syncNotifications(ctx, w.c, w.st, w.viewer); err == nil {
				fresh = append(fresh, nf...)
			} else {
				w.warn("notifications sync failed: " + err.Error())
			}
		}
	}

	// Tracked PRs (CI/merge) have no notification signal — poll every tick.
	if tf, err := syncTracked(ctx, w.c, w.st, w.viewer); err == nil {
		fresh = append(fresh, tf...)
	} else {
		w.warn("tracked-PR poll failed: " + err.Error())
	}

	w.st.Prune(cfg.Retention)
	return fresh
}

// syncNotifications polls the notifications feed into the store and returns the
// newly-inserted events (with assigned seq).
func syncNotifications(ctx context.Context, c *github.Client, st *store.Store, viewer string) ([]timeline.Event, error) {
	notes, err := c.Notifications(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetching notifications: %w", err)
	}
	notes = keepSignal(notes)
	events := enrichAll(ctx, c, notes, viewer)

	fresh, err := persist(st, events)
	if err != nil {
		return nil, err
	}
	if err := st.ReconcileNotifications(activeThreadIDs(notes)); err != nil {
		return nil, fmt.Errorf("reconciling read state: %w", err)
	}
	return fresh, nil
}

// syncTracked polls open/assigned PRs and emits CI/merge transition events.
func syncTracked(ctx context.Context, c *github.Client, st *store.Store, viewer string) ([]timeline.Event, error) {
	prs, err := c.TrackedPRs(ctx)
	if err != nil {
		return nil, err
	}
	mineKeys := make(map[string]bool)
	var events []timeline.Event
	for _, pr := range prs {
		key := fmt.Sprintf("%s#%d", pr.Repo, pr.Number)
		if pr.Author == viewer { // keep the roster of my open PRs (incl. drafts)
			mineKeys[key] = true
			_ = st.SetOpenPR(toOpenPR(pr, key))
		}
		if pr.IsDraft { // don't nag about a work-in-progress
			continue
		}
		prev, existed, err := st.GetPRState(key)
		if err != nil {
			continue
		}
		evs, next := tracker.Diff(prev, existed, pr, viewer, time.Now())
		if err := st.SetPRState(next); err != nil {
			continue
		}
		events = append(events, evs...)
	}
	_ = st.ReconcileOpenPRs(mineKeys) // drop PRs that have since merged/closed
	return persist(st, events)
}

// toOpenPR maps a fetched PR to its roster row.
func toOpenPR(pr github.TrackedPR, key string) store.OpenPR {
	return store.OpenPR{
		Key: key, Repo: pr.Repo, Number: pr.Number, Title: pr.Title, URL: pr.URL,
		Author: pr.Author, IsDraft: pr.IsDraft, CIState: pr.CIState,
		MergeState: pr.MergeState, UpdatedAt: pr.UpdatedAt,
	}
}

// persist upserts events and returns the subset that were newly inserted, each
// reloaded so it carries its assigned seq for display.
func persist(st *store.Store, events []timeline.Event) ([]timeline.Event, error) {
	existing, err := st.ExistingIDs(ids(events))
	if err != nil {
		return nil, fmt.Errorf("checking store: %w", err)
	}
	if err := st.UpsertAll(events); err != nil {
		return nil, fmt.Errorf("saving events: %w", err)
	}
	var fresh []timeline.Event
	for _, e := range events {
		if existing[e.ID] {
			continue
		}
		if stored, err := st.GetByID(e.ID); err == nil {
			fresh = append(fresh, stored)
		}
	}
	return fresh, nil
}

// notifyActionable fires one desktop notification per new event. By default it
// only notifies actionable events; setting actionable_only_notify = false in the
// config makes it notify every fresh event.
func notifyActionable(events []timeline.Event) {
	for _, e := range events {
		if cfg.NotifyActionableOnly && !e.Actionable {
			continue
		}
		badge := e.Kind.Badge()
		title := fmt.Sprintf("%s · %s", badge.Label, timeline.ShortRef(e.Repo, e.Number))
		_ = notify.Send(title, e.Detail, e.URL)
	}
}

func runMark(ctx context.Context, args []string, open bool) error {
	if len(args) != 1 {
		return fmt.Errorf("expected an item number, e.g. `watchgit open 42`")
	}
	seq, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid item number %q", args[0])
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()

	e, err := st.Get(seq)
	if err != nil {
		return fmt.Errorf("no item #%d in the timeline", seq)
	}
	if err := applyMark(ctx, st, e, open); err != nil {
		fmt.Fprintln(os.Stderr, "watchgit:", err)
	}
	fmt.Printf("✓ #%d marked read\n", seq)
	return nil
}

// applyMark opens the event's URL (when open) and marks it read locally, plus on
// GitHub for notification-backed items. Shared by the `open`/`read` commands and
// the TUI. A returned error means a best-effort step (browser or GitHub sync)
// failed — the local read still succeeded — so callers surface it as a warning.
func applyMark(ctx context.Context, st *store.Store, e timeline.Event, open bool) error {
	if open && e.URL != "" {
		if err := exec.Command("open", e.URL).Run(); err != nil {
			return fmt.Errorf("could not open browser: %w", err)
		}
	}
	if err := st.MarkRead(e.Seq); err != nil {
		return fmt.Errorf("marking read: %w", err)
	}
	if e.Source == "notification" && e.ThreadID != "" {
		c, err := github.New()
		if err != nil {
			return fmt.Errorf("marked read locally, but GitHub sync failed: %w", err)
		}
		if err := c.MarkThreadRead(ctx, e.ThreadID); err != nil {
			return fmt.Errorf("marked read locally, but GitHub sync failed: %w", err)
		}
	}
	return nil
}

// setup opens the API client and store and resolves the viewer login — the
// common preamble for list, watch, and the daemon.
func setup(ctx context.Context) (*github.Client, *store.Store, string, error) {
	c, err := github.New()
	if err != nil {
		return nil, nil, "", err
	}
	st, err := openStore()
	if err != nil {
		return nil, nil, "", err
	}
	_ = ingest.Backfill(st) // one-off cleanup of pre-fix rows
	viewer, err := c.Viewer(ctx)
	if err != nil {
		st.Close()
		return nil, nil, "", fmt.Errorf("fetching viewer: %w", err)
	}
	return c, st, viewer.Login, nil
}

func openStore() (*store.Store, error) {
	path, err := store.DefaultPath()
	if err != nil {
		return nil, err
	}
	return store.Open(path)
}

func renderOpts() timeline.RenderOpts {
	tty := isTerminal(os.Stdout)
	return timeline.RenderOpts{
		Color:      tty && os.Getenv("NO_COLOR") == "",
		Hyperlinks: tty,
		Now:        time.Now(),
		StaleAfter: cfg.StaleAfter,
	}
}

// enrichAll fetches each notification's subject concurrently (bounded) to
// recover author/URL/number, then maps to events.
func enrichAll(ctx context.Context, c *github.Client, notes []github.Notification, viewer string) []timeline.Event {
	events := make([]timeline.Event, len(notes))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup

	for i, n := range notes {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, n github.Notification) {
			defer wg.Done()
			defer func() { <-sem }()
			var s github.Subject
			if n.Subject.Type == "PullRequest" || n.Subject.Type == "Issue" {
				s, _ = c.EnrichSubject(ctx, n.Subject.URL)
			}
			events[i] = ingest.FromNotification(n, s, viewer)
		}(i, n)
	}
	wg.Wait()
	return events
}

// keepSignal drops low-signal "subscribed" watching threads.
func keepSignal(notes []github.Notification) []github.Notification {
	out := notes[:0]
	for _, n := range notes {
		if n.Reason == "subscribed" {
			continue
		}
		out = append(out, n)
	}
	return out
}

func ids(events []timeline.Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.ID
	}
	return out
}

func activeThreadIDs(notes []github.Notification) map[string]bool {
	set := make(map[string]bool, len(notes))
	for _, n := range notes {
		set[n.ID] = true
	}
	return set
}

func dim(s string) string {
	if isTerminal(os.Stdout) && os.Getenv("NO_COLOR") == "" {
		return "\x1b[2m" + s + "\x1b[0m"
	}
	return s
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
