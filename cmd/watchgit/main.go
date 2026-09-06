// Command watchgit shows GitHub activity you care about as an arrival-ordered
// timeline: notifications (reviews/assigns/comments) plus tracked-PR CI and
// merge-state transitions, with a live watch mode and desktop notifications.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"time"

	"watchgit/internal/github"
	"watchgit/internal/ingest"
	"watchgit/internal/notify"
	"watchgit/internal/store"
	"watchgit/internal/timeline"
	"watchgit/internal/tracker"
)

const (
	pruneAfter  = 7 * 24 * time.Hour
	pollFloor   = 60 * time.Second
	pollDefault = 60 * time.Second
)

func main() {
	args := os.Args[1:]
	cmd := "list"
	if len(args) > 0 {
		cmd = args[0]
		args = args[1:]
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var err error
	switch cmd {
	case "list":
		err = runList(ctx)
	case "watch":
		err = runWatch(ctx)
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
  watchgit [list]     Print the stored timeline (polls once first)
  watchgit watch      Stream new events live with desktop notifications
  watchgit open <n>   Open item <n> in the browser and mark it read (here + GitHub)
  watchgit read <n>   Mark item <n> read without opening
  watchgit help       Show this help`)
}

func runList(ctx context.Context) error {
	c, err := github.New()
	if err != nil {
		return err
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()

	viewer, err := c.Viewer(ctx)
	if err != nil {
		return fmt.Errorf("fetching viewer: %w", err)
	}
	if _, err := syncNotifications(ctx, c, st, viewer.Login); err != nil {
		return err
	}
	if _, err := syncTracked(ctx, c, st, viewer.Login); err != nil {
		fmt.Fprintln(os.Stderr, dim("⚠ tracked-PR poll failed: "+err.Error()))
	}
	if err := st.Prune(pruneAfter); err != nil {
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
	c, err := github.New()
	if err != nil {
		return err
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()

	viewer, err := c.Viewer(ctx)
	if err != nil {
		return fmt.Errorf("fetching viewer: %w", err)
	}

	// Initial sync establishes the baseline (and seeds tracked-PR state) without
	// alerting on the pre-existing backlog.
	syncNotifications(ctx, c, st, viewer.Login)
	syncTracked(ctx, c, st, viewer.Login)
	st.Prune(pruneAfter)
	if stored, err := st.List(); err == nil {
		fmt.Print(timeline.Render(stored, renderOpts()))
	}
	fmt.Println(dim("— watching, Ctrl+C to stop —"))

	interval := pollDefault
	var lastModified string
	for {
		select {
		case <-ctx.Done():
			fmt.Println(dim("— stopped —"))
			return nil
		case <-time.After(interval):
		}

		var fresh []timeline.Event

		// Notifications: conditional GET as a cheap change detector.
		changed, lm, poll, err := c.CheckNotifications(ctx, lastModified)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			fmt.Fprintln(os.Stderr, dim("⚠ poll failed, retrying: "+err.Error()))
		} else {
			if poll > 0 {
				interval = max(poll, pollFloor)
			}
			if changed {
				lastModified = lm
				if nf, err := syncNotifications(ctx, c, st, viewer.Login); err == nil {
					fresh = append(fresh, nf...)
				} else {
					fmt.Fprintln(os.Stderr, dim("⚠ notifications sync failed: "+err.Error()))
				}
			}
		}

		// Tracked PRs (CI/merge) have no notification signal — poll every tick.
		if tf, err := syncTracked(ctx, c, st, viewer.Login); err == nil {
			fresh = append(fresh, tf...)
		} else {
			fmt.Fprintln(os.Stderr, dim("⚠ tracked-PR poll failed: "+err.Error()))
		}

		st.Prune(pruneAfter)
		if len(fresh) > 0 {
			fmt.Print(timeline.Render(fresh, renderOpts()))
			notifyActionable(fresh)
		}
	}
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
	var events []timeline.Event
	for _, pr := range prs {
		if pr.IsDraft { // don't nag about a work-in-progress
			continue
		}
		key := fmt.Sprintf("%s#%d", pr.Repo, pr.Number)
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
	return persist(st, events)
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

// notifyActionable fires one desktop notification per new actionable event.
func notifyActionable(events []timeline.Event) {
	for _, e := range events {
		if !e.Actionable {
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
	if open && e.URL != "" {
		if err := exec.Command("open", e.URL).Run(); err != nil {
			fmt.Fprintln(os.Stderr, "watchgit: could not open browser:", err)
		}
	}
	if err := st.MarkRead(seq); err != nil {
		return fmt.Errorf("marking read: %w", err)
	}
	if e.Source == "notification" && e.ThreadID != "" {
		c, err := github.New()
		if err != nil {
			return err
		}
		if err := c.MarkThreadRead(ctx, e.ThreadID); err != nil {
			fmt.Fprintln(os.Stderr, "watchgit: marked read locally, but GitHub sync failed:", err)
		}
	}
	fmt.Printf("✓ #%d marked read\n", seq)
	return nil
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
