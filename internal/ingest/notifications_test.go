package ingest

import (
	"testing"
	"time"

	"watchgit/internal/github"
	"watchgit/internal/timeline"
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
		{"subscribed", timeline.KindOther, false},
	}
	for _, c := range cases {
		k, _, a := classify(c.reason)
		if k != c.kind || a != c.actionable {
			t.Errorf("classify(%q) = (%v,%v), want (%v,%v)", c.reason, k, a, c.kind, c.actionable)
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
