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

// pollOnce runs the three GitHub syncs, prunes, and — when notify is set —
// posts a desktop notification per freshly-arrived actionable event. Failures
// are logged (launchd routes them to the poller log) but never abort the loop.
func pollOnce(ctx context.Context, c *github.Client, st *store.Store, viewer string, notify bool) {
	nf, err := syncNotifications(ctx, c, st, viewer)
	if err != nil {
		fmt.Fprintln(os.Stderr, "poll: notifications:", err)
	}
	tf, err := syncTracked(ctx, c, st, viewer)
	if err != nil {
		fmt.Fprintln(os.Stderr, "poll: tracked:", err)
	}
	if err := syncReviews(ctx, c, st); err != nil {
		fmt.Fprintln(os.Stderr, "poll: reviews:", err)
	}
	_ = st.Prune(ctx, cfg.Retention)
	if notify {
		notifyActionable(append(nf, tf...))
	}
}
