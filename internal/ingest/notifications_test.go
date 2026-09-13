package ingest

import (
	"testing"
	"time"

	"watchgh/internal/github"
	"watchgh/internal/timeline"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		reason     string
		kind       timeline.Kind
		actionable bool
	}{
		{"review_requested", timeline.KindReviewRequested, true},
		{"assign", timeline.KindAssigned, true},
		{"comment", timeline.KindCommented, false},
		{"mention", timeline.KindCommented, true},
		{"author", timeline.KindOther, false},
		{"manual", timeline.KindOther, false},
		{"ci_activity", timeline.KindOther, false},
		{"subscribed", timeline.KindOther, false},
	}
	for _, c := range cases {
		k, detail, a := classify(c.reason)
		if k != c.kind || a != c.actionable {
			t.Errorf("classify(%q) = (%v,%v), want (%v,%v)", c.reason, k, a, c.kind, c.actionable)
		}
		// The detail must be a human phrase, never the raw reason token.
		if detail == c.reason {
			t.Errorf("classify(%q) leaked the raw reason as detail %q", c.reason, detail)
		}
	}
}

func TestDetailBackfillMapsLeakedReasons(t *testing.T) {
	m := DetailBackfill()
	if got := m["author"]; got != "activity on your PR" {
		t.Errorf("author -> %q, want the mapped phrase", got)
	}
	// Every entry must translate the token, never echo it back.
	for from, to := range m {
		if from == to {
			t.Errorf("backfill for %q still leaks the raw token", from)
		}
	}
}

func TestFromNotificationMinePII(t *testing.T) {
	var n github.Notification
	n.ID = "1"
	n.Reason = "comment"
	n.UpdatedAt = time.Now()
	n.Repository.FullName = "acme/api"

	var s github.Subject
	s.Number = 7
	s.User.Login = "me"

	e := FromNotification(n, s, "me")
	if !e.IsMine {
		t.Error("expected IsMine when subject author == viewer")
	}
	if e.Author != "" {
		t.Errorf("expected empty author for own PR, got %q", e.Author)
	}
}

func TestFromNotificationIDIsStableThreadID(t *testing.T) {
	// The event id must be the bare thread id (not thread + "@" + updated_at), so
	// each thread owns exactly one row and later polls upsert onto it rather than
	// minting a duplicate per update.
	var n github.Notification
	n.ID = "42"
	n.Reason = "review_requested"
	n.UpdatedAt = time.Now()

	e := FromNotification(n, github.Subject{}, "me")
	if e.ID != "42" || e.ThreadID != "42" {
		t.Errorf("id/thread = %q/%q, want both %q", e.ID, e.ThreadID, "42")
	}
}
