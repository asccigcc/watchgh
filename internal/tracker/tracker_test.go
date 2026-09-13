package tracker

import (
	"testing"
	"time"

	"watchgh/internal/github"
	"watchgh/internal/store"
	"watchgh/internal/timeline"
)

func pr(ci, merge, author string) github.TrackedPR {
	return github.TrackedPR{
		Number: 1, Repo: "acme/api", URL: "u", Author: author,
		HeadSHA: "sha1", CIState: ci, MergeState: merge, CheckCount: 3,
	}
}

func TestFirstSightingSeedsSilently(t *testing.T) {
	evs, next := Diff(store.PRState{}, false, pr("FAILURE", "DIRTY", "me"), "me", time.Now())
	if len(evs) != 0 {
		t.Fatalf("first sighting should emit nothing, got %d", len(evs))
	}
	if next.CIState != "FAILURE" || next.MergeState != "DIRTY" {
		t.Fatalf("state not seeded: %+v", next)
	}
}

func TestCITransitionEmitsOnce(t *testing.T) {
	prev := store.PRState{Key: "acme/api#1", HeadSHA: "sha1", CIState: "PENDING", MergeState: "CLEAN"}
	evs, next := Diff(prev, true, pr("SUCCESS", "CLEAN", "me"), "me", time.Now())
	if len(evs) != 1 || evs[0].Kind != timeline.KindCIPassed {
		t.Fatalf("want one CIPassed event, got %+v", evs)
	}
	// No further event once state is recorded and unchanged.
	evs2, _ := Diff(next, true, pr("SUCCESS", "CLEAN", "me"), "me", time.Now())
	if len(evs2) != 0 {
		t.Fatalf("stable state should not re-emit, got %+v", evs2)
	}
}

func TestCIFailedActionableOnlyForMyPR(t *testing.T) {
	prev := store.PRState{HeadSHA: "sha1", CIState: "PENDING", MergeState: "CLEAN"}

	mine, _ := Diff(prev, true, pr("FAILURE", "CLEAN", "me"), "me", time.Now())
	if len(mine) != 1 || mine[0].Kind != timeline.KindCIFailed || !mine[0].Actionable {
		t.Fatalf("my failed CI should be actionable: %+v", mine)
	}
	theirs, _ := Diff(prev, true, pr("FAILURE", "CLEAN", "someone"), "me", time.Now())
	if len(theirs) != 1 || theirs[0].Actionable {
		t.Fatalf("others' failed CI should not be actionable: %+v", theirs)
	}
}

func TestNewHeadShaReemitsTerminalCI(t *testing.T) {
	prev := store.PRState{HeadSHA: "sha1", CIState: "SUCCESS", MergeState: "CLEAN"}
	p := pr("SUCCESS", "CLEAN", "me")
	p.HeadSHA = "sha2" // a re-run / new commit
	evs, _ := Diff(prev, true, p, "me", time.Now())
	if len(evs) != 1 || evs[0].Kind != timeline.KindCIPassed {
		t.Fatalf("new head sha should re-emit CI: %+v", evs)
	}
}

func TestBlockedAndUnblocked(t *testing.T) {
	clean := store.PRState{HeadSHA: "sha1", CIState: "SUCCESS", MergeState: "CLEAN"}
	blocked, next := Diff(clean, true, pr("SUCCESS", "DIRTY", "me"), "me", time.Now())
	if len(blocked) != 1 || blocked[0].Kind != timeline.KindBlocked || !blocked[0].Actionable {
		t.Fatalf("CLEAN→DIRTY should emit actionable blocked: %+v", blocked)
	}
	if blocked[0].Detail != "merge conflict" {
		t.Errorf("DIRTY detail = %q", blocked[0].Detail)
	}
	unblocked, _ := Diff(next, true, pr("SUCCESS", "CLEAN", "me"), "me", time.Now())
	if len(unblocked) != 1 || unblocked[0].Kind != timeline.KindUnblocked || unblocked[0].Actionable {
		t.Fatalf("DIRTY→CLEAN should emit non-actionable unblocked: %+v", unblocked)
	}
}

func TestEventIDStablePerLane(t *testing.T) {
	// Two CI transitions on the same PR must share one id (the "ci" lane), so the
	// store keeps a single up-to-date row per lane rather than a growing history;
	// the merge lane carries its own stable id.
	prev := store.PRState{HeadSHA: "sha1", CIState: "PENDING", MergeState: "CLEAN"}
	first, next := Diff(prev, true, pr("FAILURE", "CLEAN", "me"), "me", time.Now())
	p2 := pr("SUCCESS", "CLEAN", "me")
	p2.HeadSHA = "sha2" // a re-run on a new head
	second, _ := Diff(next, true, p2, "me", time.Now().Add(time.Minute))
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("want one CI event each, got %d and %d", len(first), len(second))
	}
	if first[0].ID != "acme/api#1:ci" || second[0].ID != first[0].ID {
		t.Errorf("ci ids = %q, %q; want both %q", first[0].ID, second[0].ID, "acme/api#1:ci")
	}
	blocked, _ := Diff(next, true, pr("SUCCESS", "BLOCKED", "me"), "me", time.Now())
	last := blocked[len(blocked)-1] // ci event precedes the merge event
	if last.ID != "acme/api#1:merge" {
		t.Errorf("merge id = %q, want acme/api#1:merge", last.ID)
	}
}

func TestUnknownMergeStateIgnored(t *testing.T) {
	prev := store.PRState{HeadSHA: "sha1", CIState: "SUCCESS", MergeState: "CLEAN"}
	evs, next := Diff(prev, true, pr("SUCCESS", "UNKNOWN", "me"), "me", time.Now())
	if len(evs) != 0 {
		t.Fatalf("UNKNOWN merge should not emit: %+v", evs)
	}
	if next.MergeState != "CLEAN" {
		t.Errorf("UNKNOWN should preserve prior merge state, got %q", next.MergeState)
	}
}
