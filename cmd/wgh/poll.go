package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"watchgh/internal/github"
	"watchgh/internal/store"
)

// runPoll is the headless background poller launchd runs as `wgh __poll`. It
// polls GitHub on the configured cadence, writes to the store, and posts a
// desktop notification per freshly-arrived actionable event. The first pass is
// silent so a cold start doesn't alert on the entire backlog; the store then
// dedupes across restarts, so only genuinely new events notify thereafter.
func runPoll(ctx context.Context) error {
	c, st, viewer, err := setup(ctx)
	if err != nil {
		return err
	}
	defer st.Close()

	tick := time.NewTicker(cfg.PollFloor)
	defer tick.Stop()

	notify := false // baseline pass stays silent
	for {
		pollOnce(ctx, c, st, viewer, notify)
		notify = true
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// pollOnce runs the GitHub syncs, reaps merged/closed PRs, prunes, and refreshes the desktop
// notification backlog. When notify is set it alerts on categories that grew
// since the last pass; when it's clear (the cold-start pass) it only records the
// baseline so we don't announce the whole standing backlog at once. Failures are
// logged (launchd routes them to the poller log) but never abort the loop.
func pollOnce(ctx context.Context, c *github.Client, st *store.Store, viewer string, notify bool) {
	if _, err := syncNotifications(ctx, c, st, viewer); err != nil {
		fmt.Fprintln(os.Stderr, "poll: notifications:", err)
	}
	if _, err := syncTracked(ctx, c, st, viewer); err != nil {
		fmt.Fprintln(os.Stderr, "poll: tracked:", err)
	}
	if err := syncReviews(ctx, c, st); err != nil {
		fmt.Fprintln(os.Stderr, "poll: reviews:", err)
	}
	if err := syncClosed(ctx, c, st); err != nil {
		fmt.Fprintln(os.Stderr, "poll: closed:", err)
	}
	_ = st.Prune(ctx, cfg.Retention)
	notifyBacklog(ctx, st, notify)
}
