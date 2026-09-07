package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"watchgh/internal/timeline"
)

// bg is the context every store call in these tests threads through; the store
// itself imposes no deadline, so a plain background context is all they need.
var bg = context.Background()

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func ev(id, threadID string, unread bool) timeline.Event {
	return timeline.Event{
		ID: id, ThreadID: threadID, Source: "notification",
		TS: time.Now(), Kind: timeline.KindReviewRequested,
		Repo: "acme/api", Number: 1, Unread: unread, Actionable: true,
	}
}

func TestUpsertDedupesAndAssignsStableSeq(t *testing.T) {
	s := testStore(t)
	if err := s.Upsert(bg, ev("a", "t1", true)); err != nil {
		t.Fatal(err)
	}
	first, _ := s.List(bg)
	if len(first) != 1 {
		t.Fatalf("want 1 event, got %d", len(first))
	}
	seq := first[0].Seq

	// Re-upsert same id: no duplicate, same seq.
	if err := s.Upsert(bg, ev("a", "t1", true)); err != nil {
		t.Fatal(err)
	}
	again, _ := s.List(bg)
	if len(again) != 1 || again[0].Seq != seq {
		t.Fatalf("dedupe failed: %d rows, seq %d != %d", len(again), again[0].Seq, seq)
	}
}

func TestMarkReadClearsUnread(t *testing.T) {
	s := testStore(t)
	s.Upsert(bg, ev("a", "t1", true))
	got, _ := s.List(bg)
	if !got[0].Unread {
		t.Fatal("expected unread before MarkRead")
	}
	if err := s.MarkRead(bg, got[0].Seq); err != nil {
		t.Fatal(err)
	}
	got, _ = s.List(bg)
	if got[0].Unread {
		t.Fatal("expected read after MarkRead")
	}
}

func TestReconcileMarksAbsentThreadsRead(t *testing.T) {
	s := testStore(t)
	s.Upsert(bg, ev("a", "t1", true))
	s.Upsert(bg, ev("b", "t2", true))

	// Only t1 is still unread on GitHub; t2 was read there.
	if err := s.ReconcileNotifications(bg, map[string]bool{"t1": true}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.List(bg)
	byRepo := map[string]bool{}
	for _, e := range got {
		byRepo[e.ID] = e.Unread
	}
	if !byRepo["a"] {
		t.Error("t1 should stay unread")
	}
	if byRepo["b"] {
		t.Error("t2 should be reconciled to read")
	}
}

func TestReconcileNotificationsEmptyActiveMarksAllRead(t *testing.T) {
	s := testStore(t)
	s.Upsert(bg, ev("a", "t1", true))
	s.Upsert(bg, ev("b", "t2", true))

	// An empty active set means nothing is unread on GitHub anymore.
	if err := s.ReconcileNotifications(bg, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.List(bg)
	for _, e := range got {
		if e.Unread {
			t.Errorf("event %s should be reconciled to read", e.ID)
		}
	}
}

func TestOpenPRRosterUpsertsAndReconciles(t *testing.T) {
	s := testStore(t)
	pr := func(key, title string) OpenPR {
		return OpenPR{Key: key, Repo: "acme/api", Title: title, UpdatedAt: time.Now()}
	}
	if err := s.SetOpenPR(bg, pr("acme/api#1", "one")); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOpenPR(bg, pr("acme/api#2", "two")); err != nil {
		t.Fatal(err)
	}
	got, _ := s.OpenPRs(bg)
	if len(got) != 2 {
		t.Fatalf("want 2 roster rows, got %d", len(got))
	}

	// #2 has since closed: only #1 comes back in the next poll.
	if err := s.ReconcileOpenPRs(bg, map[string]bool{"acme/api#1": true}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.OpenPRs(bg)
	if len(got) != 1 || got[0].Key != "acme/api#1" {
		t.Fatalf("reconcile kept the wrong rows: %+v", got)
	}

	// No open PRs in the poll at all: the whole roster clears.
	if err := s.ReconcileOpenPRs(bg, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.OpenPRs(bg); len(got) != 0 {
		t.Fatalf("empty poll should clear the roster, got %+v", got)
	}
}

func TestBackfillDetailsRewritesOnlyMatchingTokens(t *testing.T) {
	s := testStore(t)
	raw := ev("raw", "t1", true)
	raw.Detail = "author"
	kept := ev("kept", "t2", true)
	kept.Detail = "new comment"
	if err := s.UpsertAll(bg, []timeline.Event{raw, kept}); err != nil {
		t.Fatal(err)
	}

	if err := s.BackfillDetails(bg, map[string]string{"author": "activity on your PR"}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.List(bg)
	by := map[string]string{}
	for _, e := range got {
		by[e.ID] = e.Detail
	}
	if by["raw"] != "activity on your PR" {
		t.Errorf("raw token not rewritten: %q", by["raw"])
	}
	if by["kept"] != "new comment" {
		t.Errorf("non-matching detail was touched: %q", by["kept"])
	}
}

func TestReconcileReviewRequestsResolvesAndResurfaces(t *testing.T) {
	s := testStore(t)
	// ev() builds a KindReviewRequested row on acme/api#1.
	s.Upsert(bg, ev("a", "t1", true))

	// Reviewed at head -> auto-resolve (drops from the Inbox).
	if err := s.ReconcileReviewRequests(bg, []ReviewState{{Repo: "acme/api", Number: 1, AtHead: true}}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.List(bg)
	if got[0].Unread {
		t.Fatal("reviewed-at-head request should be marked read")
	}

	// New commits since the review -> re-surface as unread.
	if err := s.ReconcileReviewRequests(bg, []ReviewState{{Repo: "acme/api", Number: 1, AtHead: false}}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.List(bg)
	if !got[0].Unread {
		t.Fatal("stale review should re-surface the request as unread")
	}
}

func TestReconcileReviewRequestsLeavesOtherKindsAlone(t *testing.T) {
	s := testStore(t)
	// A comment (not a review request) on the same PR must be untouched.
	e := ev("c", "t1", true)
	e.Kind = timeline.KindCommented
	s.Upsert(bg, e)

	if err := s.ReconcileReviewRequests(bg, []ReviewState{{Repo: "acme/api", Number: 1, AtHead: true}}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.List(bg)
	if !got[0].Unread {
		t.Fatal("a non-review-request event should not be auto-resolved")
	}
}

func TestPruneKeepsUnreadRemovesOldRead(t *testing.T) {
	s := testStore(t)
	s.Upsert(bg, ev("keep", "t1", true))  // unread -> must survive
	s.Upsert(bg, ev("drop", "t2", false)) // github-read -> prunable

	// Force drop's last_seen into the past.
	old := time.Now().Add(-30 * 24 * time.Hour).Unix()
	if _, err := s.db.Exec(`UPDATE events SET last_seen=? WHERE id='drop'`, old); err != nil {
		t.Fatal(err)
	}
	if err := s.Prune(bg, 7*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	got, _ := s.List(bg)
	if len(got) != 1 || got[0].ID != "keep" {
		t.Fatalf("prune removed the wrong rows: %+v", got)
	}
}
