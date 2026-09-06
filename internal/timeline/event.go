// Package timeline defines the normalized Event that every ingestion source
// maps into, and how events render as compact single-line timeline rows.
package timeline

import "time"

// Kind is the event taxonomy from the design's mapping table.
type Kind int

const (
	KindOther Kind = iota
	KindReviewRequested
	KindAssigned
	KindCommented
	KindChangesRequested
	KindApproved
	KindStateChange
	KindCIPassed
	KindCIFailed
	KindBlocked
	KindUnblocked
	KindOpenPR // a tracked open PR with no fresher signal (Mine-roster row)
)

// Event is the source-agnostic timeline row. Both the notifications poller and
// (later) the tracked-PR GraphQL poller produce these.
type Event struct {
	Seq        int64     // stable local number, assigned by the store; 0 if unstored
	ID         string    // stable dedupe key
	ThreadID   string    // GitHub notification thread id (for mark-read PATCH); "" if none
	Source     string    // "notification" | "graphql"
	TS         time.Time // arrival / updated time
	Kind       Kind
	Repo       string // owner/name
	Number     int    // PR/issue number, 0 if unknown
	Author     string // PR author login; empty when it's the viewer
	Detail     string // actor + action, or CI summary
	URL        string // web URL for the PR (OSC 8 target)
	Unread     bool
	Actionable bool // drives the stale "!" gutter and retention protection
	IsMine     bool // author == viewer -> AUTHOR renders as dim em-dash
}

// Badge describes the colored badge cell for a Kind.
type Badge struct {
	Glyph string
	Label string
	Color color
}

func (k Kind) Badge() Badge {
	switch k {
	case KindReviewRequested:
		return Badge{"◆", "review", colBlue}
	case KindAssigned:
		return Badge{"→", "assign", colBlue}
	case KindCommented:
		return Badge{"●", "review", colMagenta}
	case KindChangesRequested:
		return Badge{"●", "review", colMagenta}
	case KindApproved:
		return Badge{"✓", "appr", colGreen}
	case KindStateChange:
		return Badge{"⌦", "closed", colDim}
	case KindCIPassed:
		return Badge{"✓", "CI", colGreen}
	case KindCIFailed:
		return Badge{"✗", "CI", colRed}
	case KindBlocked:
		return Badge{"⊘", "block", colYellow}
	case KindUnblocked:
		return Badge{"✓", "block", colGreen}
	case KindOpenPR:
		return Badge{"⎇", "PR", colBlue}
	default:
		return Badge{"·", "note", colDim}
	}
}
