package main

import (
	"context"
	"fmt"
	"strings"

	"watchgh/internal/notify"
	"watchgh/internal/store"
	"watchgh/internal/timeline"
)

// notifyBucket is one coalesced desktop-notification category: a stable id (its
// terminal-notifier -group, which drives in-place replacement) and the
// singular/plural banner templates the current count fills. The count is a
// backlog — how many items of this kind currently await you — so a bucket
// re-alerts only when its count grows, and its banner reads as a running total.
type notifyBucket struct {
	id   string
	one  string // fmt template for a count of 1
	many string // fmt template for 2+
}

// notifyBuckets are the categories that raise a desktop banner, highest-signal
// first. Review/assign/comments are unread-actionable notification events; CI and
// blocked are the current state of your open PRs (the roster), so they clear on
// their own as CI turns green or a PR unblocks — no "read" needed.
var notifyBuckets = []notifyBucket{
	{"review", "%d review requested", "%d reviews requested"},
	{"assign", "%d PR assigned to you", "%d PRs assigned to you"},
	{"ci", "%d PR failing CI", "%d PRs failing CI"},
	{"blocked", "%d PR blocked", "%d PRs blocked"},
	{"comments", "%d PR needs your reply", "%d PRs need your reply"},
}

// phrase renders the bucket's banner text for a count.
func (b notifyBucket) phrase(n int) string {
	f := b.many
	if n == 1 {
		f = b.one
	}
	return fmt.Sprintf(f, n)
}

// group is the terminal-notifier group id for a bucket — namespaced so it can't
// collide with another tool's notifications.
func (b notifyBucket) group() string { return "watchgh." + b.id }

// notifyBacklog recomputes the per-category backlog from the store, alerts on any
// category that grew since the last pass, and records the new baseline. When send
// is false it only refreshes the baseline (silently) — used on a cold start so we
// don't alert on the whole standing backlog at once. Best-effort throughout: a
// store read error just skips this pass.
func notifyBacklog(ctx context.Context, st *store.Store, send bool) {
	events, err := st.List(ctx)
	if err != nil {
		return
	}
	prs, err := st.OpenPRs(ctx)
	if err != nil {
		return
	}
	cur := backlog(events, prs)
	if send {
		if prev, err := st.NotifyCounts(ctx); err == nil {
			emitBacklog(prev, cur)
		}
	}
	_ = st.SetNotifyCounts(ctx, cur)
}

// backlog counts the items currently awaiting you in each notification category.
// Notification-sourced categories count unread + actionable events (matching the
// timeline's own attention model); CI and blocked count your open, non-draft PRs
// by current state, so flapping transition events can't double-count them. Every
// bucket is present (zero when empty) so a cleared category persists as a zero
// baseline rather than a stale high-water mark.
func backlog(events []timeline.Event, prs []store.OpenPR) map[string]int {
	b := make(map[string]int, len(notifyBuckets))
	for _, bk := range notifyBuckets {
		b[bk.id] = 0
	}
	for _, e := range events {
		if !e.Unread || !e.Actionable {
			continue
		}
		switch e.Kind {
		case timeline.KindReviewRequested:
			b["review"]++
		case timeline.KindAssigned:
			b["assign"]++
		case timeline.KindCommented, timeline.KindChangesRequested:
			b["comments"]++
		}
	}
	for _, pr := range prs {
		if pr.IsDraft { // work-in-progress: don't nag
			continue
		}
		switch pr.CIState {
		case "FAILURE", "ERROR":
			b["ci"]++
		}
		switch pr.MergeState {
		case "BLOCKED", "DIRTY":
			b["blocked"]++
		}
	}
	return b
}

// emitBacklog posts one banner per category whose backlog grew. With
// terminal-notifier each posts to its own group (replacing that category's prior
// banner in place, and clearing it when the category empties). Without it, the
// grown categories fold into a single osascript summary, since a plain banner can
// neither replace nor be removed.
func emitBacklog(prev, cur map[string]int) {
	if notify.HasGrouping() {
		for _, bk := range notifyBuckets {
			switch {
			case cur[bk.id] > prev[bk.id]:
				_ = notify.SendGroup(bk.phrase(cur[bk.id]), "Open wgh to review", bk.group())
			case cur[bk.id] == 0 && prev[bk.id] > 0:
				_ = notify.RemoveGroup(bk.group())
			}
		}
		return
	}
	var parts []string
	for _, bk := range notifyBuckets {
		if cur[bk.id] > prev[bk.id] {
			parts = append(parts, bk.phrase(cur[bk.id]))
		}
	}
	if len(parts) > 0 {
		_ = notify.Send("watchgh", strings.Join(parts, " · "), "")
	}
}
