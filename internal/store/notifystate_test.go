package store

import (
	"path/filepath"
	"testing"
)

func TestNotifyCountsRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "n.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// A fresh store has no baseline: every category reads as zero (absent).
	got, err := s.NotifyCounts(bg)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("fresh store baseline = %v, want empty", got)
	}

	if err := s.SetNotifyCounts(bg, map[string]int{"review": 3, "ci": 2, "blocked": 0}); err != nil {
		t.Fatal(err)
	}
	got, err = s.NotifyCounts(bg)
	if err != nil {
		t.Fatal(err)
	}
	if got["review"] != 3 || got["ci"] != 2 || got["blocked"] != 0 {
		t.Errorf("counts = %v, want review 3, ci 2, blocked 0", got)
	}

	// A later write is an upsert: a category that dropped lowers its baseline.
	if err := s.SetNotifyCounts(bg, map[string]int{"review": 1, "ci": 2}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.NotifyCounts(bg)
	if got["review"] != 1 {
		t.Errorf("review after drop = %d, want 1", got["review"])
	}
}
