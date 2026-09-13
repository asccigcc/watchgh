// Package tracker diffs a tracked PR's current CI/merge status against its
// last-seen state and emits timeline events only on transitions.
package tracker

import (
	"fmt"
	"time"

	"watchgh/internal/github"
	"watchgh/internal/store"
	"watchgh/internal/timeline"
)

// Diff compares prev against pr and returns any transition events plus the next
// state to persist. The first time a PR is seen (existed=false) it seeds state
// silently — a first sighting is not a transition. Drafts are handled upstream.
func Diff(prev store.PRState, existed bool, pr github.TrackedPR, viewer string, now time.Time) ([]timeline.Event, store.PRState) {
	next := store.PRState{
		Key:        key(pr),
		HeadSHA:    pr.HeadSHA,
		CIState:    pr.CIState,
		MergeState: prev.MergeState, // preserve unless we get a definite reading
	}
	if known(pr.MergeState) {
		next.MergeState = pr.MergeState
	}
	if !existed {
		return nil, next
	}

	isMine := pr.Author != "" && pr.Author == viewer
	var events []timeline.Event

	// CI: emit when the rollup is terminal and differs from what we last saw
	// for this commit (covers transitions and re-runs on a new head SHA).
	if terminal(pr.CIState) && !(prev.HeadSHA == pr.HeadSHA && prev.CIState == pr.CIState) {
		events = append(events, ciEvent(pr, isMine, now))
	}

	// Merge: emit on a definite change into/out of an attention state.
	if known(pr.MergeState) && pr.MergeState != prev.MergeState {
		switch {
		case attention(pr.MergeState):
			events = append(events, blockedEvent(pr, isMine, now))
		case pr.MergeState == "CLEAN" && attention(prev.MergeState):
			events = append(events, unblockedEvent(pr, isMine, now))
		}
	}
	return events, next
}

func key(pr github.TrackedPR) string { return fmt.Sprintf("%s#%d", pr.Repo, pr.Number) }

// terminal reports whether a CI rollup state is finished.
func terminal(state string) bool {
	return state == "SUCCESS" || state == "FAILURE" || state == "ERROR"
}

// attention reports whether a merge state needs the author's action.
func attention(state string) bool {
	return state == "DIRTY" || state == "BLOCKED"
}

// known filters out transient/absent merge states that would cause flapping.
func known(state string) bool {
	return state != "" && state != "UNKNOWN"
}

func ciEvent(pr github.TrackedPR, isMine bool, now time.Time) timeline.Event {
	kind := timeline.KindCIPassed
	detail := "passed"
	if pr.CheckCount > 0 {
		detail = fmt.Sprintf("passed ·%d checks", pr.CheckCount)
	}
	if pr.CIState != "SUCCESS" {
		kind = timeline.KindCIFailed
		detail = "CI failed"
	}
	e := base(pr, isMine, now, "ci")
	e.Kind = kind
	e.Detail = detail
	e.Actionable = kind == timeline.KindCIFailed && isMine
	return e
}

func blockedEvent(pr github.TrackedPR, isMine bool, now time.Time) timeline.Event {
	detail := "blocked"
	if pr.MergeState == "DIRTY" {
		detail = "merge conflict"
	}
	e := base(pr, isMine, now, "merge")
	e.Kind = timeline.KindBlocked
	e.Detail = detail
	e.Actionable = true
	return e
}

func unblockedEvent(pr github.TrackedPR, isMine bool, now time.Time) timeline.Event {
	e := base(pr, isMine, now, "merge")
	e.Kind = timeline.KindUnblocked
	e.Detail = "unblocked"
	return e
}

// base builds the common event fields. The id is stable per (PR, channel) — one
// "ci" row and one "merge" row per PR — so a later transition upserts onto that
// same row (refreshing kind/detail/ts and re-surfacing it as unread) instead of
// minting a fresh row per change, which used to pile up unbounded, never-pruned
// history. Diff only calls this on a real transition, so the row's content
// always reflects the latest status.
func base(pr github.TrackedPR, isMine bool, now time.Time, channel string) timeline.Event {
	author := pr.Author
	if isMine {
		author = ""
	}
	return timeline.Event{
		ID:     fmt.Sprintf("%s:%s", key(pr), channel),
		Source: "graphql",
		TS:     now,
		Repo:   pr.Repo,
		Number: pr.Number,
		Author: author,
		URL:    pr.URL,
		Unread: true,
		IsMine: isMine,
	}
}
