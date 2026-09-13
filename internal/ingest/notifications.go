// Package ingest maps raw GitHub payloads into normalized timeline events,
// per the design's event-taxonomy mapping table.
package ingest

import (
	"context"

	"watchgh/internal/github"
	"watchgh/internal/store"
	"watchgh/internal/timeline"
)

// Backfill runs the one-off cleanup that rewrites events still holding a raw
// GitHub reason token as their detail. Idempotent; safe to call at every start.
func Backfill(ctx context.Context, st *store.Store) error {
	if err := st.CollapseNotificationThreads(ctx); err != nil {
		return err
	}
	return st.BackfillDetails(ctx, DetailBackfill())
}

// FromNotification maps a notification thread (enriched with its subject) into
// a timeline event. viewer is the authenticated login, used for "is this mine?".
func FromNotification(n github.Notification, s github.Subject, viewer string) timeline.Event {
	kind, detail, actionable := classify(n.Reason)

	isMine := s.User.Login != "" && s.User.Login == viewer
	author := s.User.Login
	if isMine {
		author = ""
	}

	return timeline.Event{
		// The id is the stable thread id: one row per PR thread. Re-surfacing a
		// re-read thread on new activity is the store's job (upsert clears read_at
		// when a later ts arrives), not something we encode into a per-update id —
		// that only piled up duplicate rows.
		ID:         n.ID,
		ThreadID:   n.ID,
		Source:     "notification",
		TS:         n.UpdatedAt,
		Kind:       kind,
		Repo:       n.Repository.FullName,
		Number:     s.Number,
		Author:     author,
		Detail:     detail,
		URL:        s.HTMLURL,
		Unread:     n.Unread,
		Actionable: actionable,
		IsMine:     isMine,
	}
}

// leakyReasons are the GitHub notification reasons the classifier used to echo
// verbatim as the row detail (they fell through to the old default case). Any
// stored event whose detail equals one of these is a pre-fix leak.
var leakyReasons = []string{
	"author", "manual", "ci_activity", "subscribed", "approval_requested",
	"invitation", "security_alert", "security_advisory_credit",
	"member_feature_requested", "your_activity",
}

// DetailBackfill maps each leaked raw-reason token to the phrase classify now
// produces, for a one-time store cleanup. classify stays the single source of
// truth: tokens whose detail is unchanged (none, post-fix) are skipped.
func DetailBackfill() map[string]string {
	m := make(map[string]string, len(leakyReasons))
	for _, r := range leakyReasons {
		if _, detail, _ := classify(r); detail != r {
			m[r] = detail
		}
	}
	return m
}

// classify turns a notification `reason` into (kind, detail, actionable).
// Path-A (notification-backed) subset only; CI/blocked come from GraphQL later.
func classify(reason string) (timeline.Kind, string, bool) {
	switch reason {
	case "review_requested":
		return timeline.KindReviewRequested, "review requested", true
	case "assign":
		return timeline.KindAssigned, "assigned to you", true
	case "comment":
		return timeline.KindCommented, "new comment", false
	case "mention", "team_mention":
		return timeline.KindCommented, "mentioned you", true
	case "state_change":
		return timeline.KindStateChange, "state changed", false
	case "author":
		// You created the thread; GitHub fired the coarse "author" reason for
		// some activity it didn't classify further.
		return timeline.KindOther, "activity on your PR", false
	case "manual":
		return timeline.KindOther, "activity on a thread you follow", false
	case "ci_activity":
		return timeline.KindOther, "workflow run finished", false
	default:
		// Never surface the raw reason token; a readable phrase beats "author".
		return timeline.KindOther, "new activity", false
	}
}
