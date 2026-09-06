package store

import (
	"path/filepath"
	"testing"
	"time"

	"watchgit/internal/timeline"
)

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
	if err := s.Upsert(ev("a", "t1", true)); err != nil {
		t.Fatal(err)
	}
	first, _ := s.List()
	if len(first) != 1 {
		t.Fatalf("want 1 event, got %d", len(first))
	}
	seq := first[0].Seq

	// Re-upsert same id: no duplicate, same seq.
	if err := s.Upsert(ev("a", "t1", true)); err != nil {
		t.Fatal(err)
	}
	again, _ := s.List()
	if len(again) != 1 || again[0].Seq != seq {
		t.Fatalf("dedupe failed: %d rows, seq %d != %d", len(again), again[0].Seq, seq)
	}
}

func TestMarkReadClearsUnread(t *testing.T) {
	s := testStore(t)
	s.Upsert(ev("a", "t1", true))
	got, _ := s.List()
	if !got[0].Unread {
		t.Fatal("expected unread before MarkRead")
	}
	if err := s.MarkRead(got[0].Seq); err != nil {
		t.Fatal(err)
	}
	got, _ = s.List()
	if got[0].Unread {
		t.Fatal("expected read after MarkRead")
	}
}

func TestReconcileMarksAbsentThreadsRead(t *testing.T) {
	s := testStore(t)
	s.Upsert(ev("a", "t1", true))
	s.Upsert(ev("b", "t2", true))

	// Only t1 is still unread on GitHub; t2 was read there.
	if err := s.ReconcileNotifications(map[string]bool{"t1": true}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.List()
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

func TestPruneKeepsUnreadRemovesOldRead(t *testing.T) {
	s := testStore(t)
	s.Upsert(ev("keep", "t1", true))  // unread -> must survive
	s.Upsert(ev("drop", "t2", false)) // github-read -> prunable

	// Force drop's last_seen into the past.
	old := time.Now().Add(-30 * 24 * time.Hour).Unix()
	if _, err := s.db.Exec(`UPDATE events SET last_seen=? WHERE id='drop'`, old); err != nil {
		t.Fatal(err)
	}
	if err := s.Prune(7 * 24 * time.Hour); err != nil {
		t.Fatal(err)
	}
	got, _ := s.List()
	if len(got) != 1 || got[0].ID != "keep" {
		t.Fatalf("prune removed the wrong rows: %+v", got)
	}
}
