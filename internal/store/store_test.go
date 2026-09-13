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

func TestUpsertResurfacesOnNewerActivity(t *testing.T) {
	s := testStore(t)
	old := ev("a", "t1", true)
	old.TS = time.Now()
	s.Upsert(bg, old)

	seq := func() int64 { got, _ := s.List(bg); return got[0].Seq }()
	if err := s.MarkRead(bg, seq); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.List(bg); got[0].Unread {
		t.Fatal("expected read after MarkRead")
	}

	// Same id, no newer activity (ts unchanged): the local read must stick.
	s.Upsert(bg, old)
	if got, _ := s.List(bg); got[0].Unread {
		t.Error("a refresh with no new activity should not re-surface a read item")
	}

	// Same id, later ts: genuine new activity re-surfaces it as unread, in place.
	fresh := old
	fresh.TS = old.TS.Add(time.Hour)
	s.Upsert(bg, fresh)
	again, _ := s.List(bg)
	if len(again) != 1 {
		t.Fatalf("re-surface must reuse the row, got %d rows", len(again))
	}
	if !again[0].Unread {
		t.Error("new activity should clear read_at and re-surface as unread")
	}
}

func TestCollapseNotificationThreads(t *testing.T) {
	s := testStore(t)
	base := time.Now()
	// Three legacy per-update rows for one thread, plus a lone row for another.
	for i, id := range []string{"t1@a", "t1@b", "t1@c"} {
		e := ev(id, "t1", true)
		e.TS = base.Add(time.Duration(i) * time.Hour)
		e.Detail = id
		s.Upsert(bg, e)
	}
	other := ev("t2@x", "t2", true)
	s.Upsert(bg, other)

	if err := s.CollapseNotificationThreads(bg); err != nil {
		t.Fatal(err)
	}

	got, _ := s.List(bg)
	if len(got) != 2 {
		t.Fatalf("want one row per thread (2), got %d", len(got))
	}
	byThread := map[string]timeline.Event{}
	for _, e := range got {
		byThread[e.ThreadID] = e
	}
	// The t1 survivor is the latest version and its id is now the bare thread id.
	if s := byThread["t1"]; s.ID != "t1" || s.Detail != "t1@c" {
		t.Errorf("t1 survivor id=%q detail=%q, want id t1 and latest detail t1@c", s.ID, s.Detail)
	}
	if s := byThread["t2"]; s.ID != "t2" {
		t.Errorf("t2 id=%q, want normalized to bare thread id t2", s.ID)
	}

	// Idempotent: a second run leaves the collapsed rows untouched.
	if err := s.CollapseNotificationThreads(bg); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.List(bg); len(again) != 2 {
		t.Fatalf("second run changed row count to %d, want 2", len(again))
	}
}

func TestUpsertRefreshesKindOnTransition(t *testing.T) {
	// A tracked-PR lane row keeps a stable id and transitions between kinds
	// (failed→passed); the upsert must refresh kind, not just detail/ts.
	s := testStore(t)
	e := timeline.Event{ID: "acme/api#1:ci", Source: "graphql", TS: time.Now(),
		Kind: timeline.KindCIFailed, Repo: "acme/api", Number: 1, Detail: "CI failed", Unread: true}
	s.Upsert(bg, e)
	e.TS = e.TS.Add(time.Hour)
	e.Kind = timeline.KindCIPassed
	e.Detail = "passed"
	s.Upsert(bg, e)

	got, _ := s.List(bg)
	if len(got) != 1 || got[0].Kind != timeline.KindCIPassed || got[0].Detail != "passed" {
		t.Fatalf("transition should update the single row's kind/detail, got %+v", got)
	}
}

func TestCollapseGraphqlLanes(t *testing.T) {
	s := testStore(t)
	start := time.Now()
	gql := func(id string, kind timeline.Kind, ts time.Time, repo string, num int, detail string) timeline.Event {
		return timeline.Event{ID: id, Source: "graphql", TS: ts, Kind: kind,
			Repo: repo, Number: num, Detail: detail, Unread: true}
	}
	// Legacy per-transition rows for one PR: two "ci" versions + one "merge".
	s.Upsert(bg, gql("acme/api#1:ci:sha1:FAILURE@1", timeline.KindCIFailed, start, "acme/api", 1, "old"))
	s.Upsert(bg, gql("acme/api#1:ci:sha2:SUCCESS@2", timeline.KindCIPassed, start.Add(time.Hour), "acme/api", 1, "passed"))
	s.Upsert(bg, gql("acme/api#1:merge:BLOCKED@3", timeline.KindBlocked, start.Add(2*time.Hour), "acme/api", 1, "blocked"))
	// A second PR's lone ci row.
	s.Upsert(bg, gql("acme/api#2:ci:x@4", timeline.KindCIPassed, start, "acme/api", 2, "passed"))

	if err := s.CollapseGraphqlLanes(bg); err != nil {
		t.Fatal(err)
	}
	got, _ := s.List(bg)
	byID := map[string]timeline.Event{}
	for _, e := range got {
		byID[e.ID] = e
	}
	if len(got) != 3 {
		t.Fatalf("want 3 lane rows (PR1 ci+merge, PR2 ci), got %d: %v", len(got), byID)
	}
	if e := byID["acme/api#1:ci"]; e.Detail != "passed" {
		t.Errorf("ci survivor detail = %q, want the latest 'passed'", e.Detail)
	}
	if _, ok := byID["acme/api#1:merge"]; !ok {
		t.Error("merge lane should survive independently of the ci lane")
	}
	if _, ok := byID["acme/api#2:ci"]; !ok {
		t.Error("the second PR's ci lane should survive")
	}

	// Idempotent: a second run touches nothing.
	if err := s.CollapseGraphqlLanes(bg); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.List(bg); len(again) != 3 {
		t.Fatalf("second run changed row count to %d, want 3", len(again))
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
