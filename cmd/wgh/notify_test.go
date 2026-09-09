package main

import (
	"testing"

	"watchgh/internal/store"
	"watchgh/internal/timeline"
)

func TestBacklogCategorizes(t *testing.T) {
	events := []timeline.Event{
		{Kind: timeline.KindReviewRequested, Unread: true, Actionable: true},
		{Kind: timeline.KindReviewRequested, Unread: true, Actionable: true},
		{Kind: timeline.KindAssigned, Unread: true, Actionable: true},
		{Kind: timeline.KindChangesRequested, Unread: true, Actionable: true},
		{Kind: timeline.KindCommented, Unread: true, Actionable: true},
		// Read or non-actionable events don't count toward the backlog.
		{Kind: timeline.KindReviewRequested, Unread: false, Actionable: true},
		{Kind: timeline.KindCommented, Unread: true, Actionable: false},
	}
	prs := []store.OpenPR{
		{CIState: "FAILURE", MergeState: "CLEAN"},
		{CIState: "ERROR", MergeState: "BLOCKED"}, // counts toward both CI and blocked
		{CIState: "SUCCESS", MergeState: "DIRTY"},
		{CIState: "SUCCESS", MergeState: "CLEAN"},                  // healthy: counts nowhere
		{CIState: "FAILURE", MergeState: "BLOCKED", IsDraft: true}, // WIP: skipped entirely
	}

	b := backlog(events, prs)
	want := map[string]int{"review": 2, "assign": 1, "comments": 2, "ci": 2, "blocked": 2}
	for id, n := range want {
		if b[id] != n {
			t.Errorf("backlog[%q] = %d, want %d", id, b[id], n)
		}
	}
}

func TestBacklogPresentAsZeroWhenEmpty(t *testing.T) {
	// Every bucket must appear (as zero) so a cleared category persists a zero
	// baseline instead of leaving a stale high-water mark behind.
	b := backlog(nil, nil)
	for _, bk := range notifyBuckets {
		if _, ok := b[bk.id]; !ok {
			t.Errorf("bucket %q missing from empty backlog", bk.id)
		}
		if b[bk.id] != 0 {
			t.Errorf("empty backlog[%q] = %d, want 0", bk.id, b[bk.id])
		}
	}
}

func TestBucketPhrasePluralizes(t *testing.T) {
	review := notifyBuckets[0]
	if got := review.phrase(1); got != "1 review requested" {
		t.Errorf("phrase(1) = %q", got)
	}
	if got := review.phrase(3); got != "3 reviews requested" {
		t.Errorf("phrase(3) = %q", got)
	}
}
