package main

import (
	"testing"

	"watchgh/internal/timeline"
)

func TestPRRefsOfDedupesAndSkipsNumberless(t *testing.T) {
	ev := func(repo string, number int) timeline.Event {
		return timeline.Event{Repo: repo, Number: number}
	}
	events := []timeline.Event{
		ev("acme/api", 1),
		ev("acme/api", 1), // duplicate PR -> one ref
		ev("acme/api", 2),
		ev("acme/api", 0), // no PR number -> skipped
		ev("", 3),         // no repo -> skipped
	}
	got := prRefsOf(events)
	want := map[string]bool{"acme/api#1": true, "acme/api#2": true}
	if len(got) != len(want) {
		t.Fatalf("got %d refs, want %d: %+v", len(got), len(want), got)
	}
	for _, r := range got {
		if !want[r.Key()] {
			t.Errorf("unexpected ref %q", r.Key())
		}
	}
}
