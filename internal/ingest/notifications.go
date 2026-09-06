// Package ingest maps raw GitHub payloads into normalized timeline events,
// per the design's event-taxonomy mapping table.
package ingest

import (
	"watchgit/internal/github"
	"watchgit/internal/timeline"
)

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
		ID:         n.ID + "@" + n.UpdatedAt.UTC().Format("20060102T150405Z"),
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
	default:
		return timeline.KindOther, reason, false
	}
}
