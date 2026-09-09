// Command wgh shows GitHub activity you care about as an arrival-ordered
// timeline: notifications (reviews/assigns/comments) plus tracked-PR CI and
// merge-state transitions. Opening wgh installs a launchd background poller
// that keeps GitHub in sync and posts desktop notifications even when no window
// is open; `wgh stop` tears it down.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"watchgh/internal/agent"
	"watchgh/internal/config"
	"watchgh/internal/github"
	"watchgh/internal/ingest"
	"watchgh/internal/store"
	"watchgh/internal/timeline"
	"watchgh/internal/tracker"
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
		fmt.Fprintln(os.Stderr, "wgh: config:", err, "— using defaults")
	} else {
		cfg = c
	}

	// Handle SIGTERM as well as SIGINT so the TUI restores the terminal and
	// closes the store cleanly however it's asked to stop.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd {
	case "":
		// Bare `wgh` opens the interactive timeline when attached to a
		// terminal; piped or redirected, it prints the plain timeline so
		// scripts keep working.
		if isTerminal(os.Stdout) && isTerminal(os.Stdin) {
			err = runTUI(ctx)
		} else {
			err = runList(ctx)
		}
	case "open":
		err = runMark(ctx, args, true)
	case "read":
		err = runMark(ctx, args, false)
	case "stop":
		err = runStop()
	case "__poll":
		// Hidden: the headless loop launchd runs as the background poller.
		err = runPoll(ctx)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "wgh: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "wgh:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`wgh — a GitHub activity timeline

Usage:
  wgh               Interactive timeline (arrows/⏎/r/q); prints plain when piped
  wgh open <n>      Open item <n> in the browser and mark it read (here + GitHub)
  wgh read <n>      Mark item <n> read without opening
  wgh stop          Stop the background poller (it restarts next time you open wgh)
  wgh help          Show this help

Opening wgh installs a launchd background poller that keeps GitHub in sync and
posts desktop notifications for actionable events, even when no window is open.`)
}

// runStop tears down the background poller. Stopping one that isn't running is
// a no-op, not an error.
func runStop() error {
	if err := agent.Stop(); err != nil {
		return err
	}
	fmt.Println("✓ background poller stopped")
	return nil
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
	if err := syncReviews(ctx, c, st); err != nil {
		fmt.Fprintln(os.Stderr, dim("⚠ review-state poll failed: "+err.Error()))
	}
	if err := st.Prune(ctx, cfg.Retention); err != nil {
		return fmt.Errorf("pruning: %w", err)
	}

	stored, err := st.List(ctx)
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

// syncNotifications polls the notifications feed into the store and returns the
// newly-inserted events (with assigned seq).
func syncNotifications(ctx context.Context, c *github.Client, st *store.Store, viewer string) ([]timeline.Event, error) {
	notes, err := c.Notifications(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetching notifications: %w", err)
	}
	notes = keepSignal(notes)
	events := enrichAll(ctx, c, notes, viewer)

	fresh, err := persist(ctx, st, events)
	if err != nil {
		return nil, err
	}
	if err := st.ReconcileNotifications(ctx, activeThreadIDs(notes)); err != nil {
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
	// Store writes here are best-effort per PR — one failing row shouldn't abort
	// the rest — but the first error is kept and returned so it's surfaced (footer
	// warning in the TUI, stderr in the poller/list) rather than silently dropped.
	var storeErr error
	note := func(err error) {
		if err != nil && storeErr == nil {
			storeErr = err
		}
	}
	for _, pr := range prs {
		key := fmt.Sprintf("%s#%d", pr.Repo, pr.Number)
		if pr.Author == viewer { // keep the roster of my open PRs (incl. drafts)
			mineKeys[key] = true
			note(st.SetOpenPR(ctx, toOpenPR(pr, key)))
		}
		if pr.IsDraft { // don't nag about a work-in-progress
			continue
		}
		prev, existed, err := st.GetPRState(ctx, key)
		if err != nil {
			note(err)
			continue
		}
		evs, next := tracker.Diff(prev, existed, pr, viewer, time.Now())
		if err := st.SetPRState(ctx, next); err != nil {
			note(err)
			continue
		}
		events = append(events, evs...)
	}
	note(st.ReconcileOpenPRs(ctx, mineKeys)) // drop PRs that have since merged/closed
	fresh, err := persist(ctx, st, events)
	if err != nil {
		return nil, err
	}
	return fresh, storeErr
}

// syncReviews reconciles review-request items against whether the viewer has
// actually reviewed each PR's current head: it auto-resolves requests already
// reviewed and re-surfaces ones whose review went stale after new commits. It
// emits no new events, so — unlike the notification and tracked-PR syncs — it
// returns nothing but an error.
func syncReviews(ctx context.Context, c *github.Client, st *store.Store) error {
	states, err := c.ReviewStates(ctx)
	if err != nil {
		return err
	}
	verdicts := make([]store.ReviewState, len(states))
	for i, rs := range states {
		verdicts[i] = store.ReviewState{Repo: rs.Repo, Number: rs.Number, AtHead: rs.AtHead}
	}
	return st.ReconcileReviewRequests(ctx, verdicts)
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
func persist(ctx context.Context, st *store.Store, events []timeline.Event) ([]timeline.Event, error) {
	existing, err := st.ExistingIDs(ctx, ids(events))
	if err != nil {
		return nil, fmt.Errorf("checking store: %w", err)
	}
	if err := st.UpsertAll(ctx, events); err != nil {
		return nil, fmt.Errorf("saving events: %w", err)
	}
	var fresh []timeline.Event
	for _, e := range events {
		if existing[e.ID] {
			continue
		}
		if stored, err := st.GetByID(ctx, e.ID); err == nil {
			fresh = append(fresh, stored)
		}
	}
	return fresh, nil
}

func runMark(ctx context.Context, args []string, open bool) error {
	if len(args) != 1 {
		return fmt.Errorf("expected an item number, e.g. `wgh open 42`")
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

	e, err := st.Get(ctx, seq)
	if err != nil {
		return fmt.Errorf("no item #%d in the timeline", seq)
	}
	if err := applyMark(ctx, st, e, open); err != nil {
		fmt.Fprintln(os.Stderr, "wgh:", err)
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
		octx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := exec.CommandContext(octx, "open", e.URL).Run(); err != nil {
			return fmt.Errorf("could not open browser: %w", err)
		}
	}
	if err := st.MarkRead(ctx, e.Seq); err != nil {
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
// preamble for the one-shot piped list, which needs the viewer up front.
func setup(ctx context.Context) (*github.Client, *store.Store, string, error) {
	c, st, err := setupLocal(ctx)
	if err != nil {
		return nil, nil, "", err
	}
	viewer, err := c.Viewer(ctx)
	if err != nil {
		st.Close()
		return nil, nil, "", fmt.Errorf("fetching viewer: %w", err)
	}
	return c, st, viewer.Login, nil
}

// setupLocal opens the API client and store with no network round-trip — the
// fast path for the TUI, which draws from the store first and resolves the
// viewer in a background sync.
func setupLocal(ctx context.Context) (*github.Client, *store.Store, error) {
	c, err := github.New()
	if err != nil {
		return nil, nil, err
	}
	st, err := openStore()
	if err != nil {
		return nil, nil, err
	}
	_ = ingest.Backfill(ctx, st) // one-off cleanup of pre-fix rows
	return c, st, nil
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
