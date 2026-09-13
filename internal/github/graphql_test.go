package github

import "testing"

func TestToReviewState(t *testing.T) {
	mk := func(head, reviewOID, state string, hasReview bool) reviewNode {
		n := reviewNode{Number: 7}
		n.Repository.NameWithOwner = "acme/api"
		if head != "" {
			n.Commits.Nodes = []struct {
				Commit struct {
					Oid string `json:"oid"`
				} `json:"commit"`
			}{{Commit: struct {
				Oid string `json:"oid"`
			}{Oid: head}}}
		}
		if hasReview {
			n.ViewerLatestReview = &struct {
				State  string `json:"state"`
				Commit struct {
					Oid string `json:"oid"`
				} `json:"commit"`
			}{State: state, Commit: struct {
				Oid string `json:"oid"`
			}{Oid: reviewOID}}
		}
		return n
	}

	// Reviewed the current head -> AtHead, carrying the verdict through.
	if rs, ok := mk("abc", "abc", "APPROVED", true).toReviewState(); !ok || !rs.AtHead || rs.State != "APPROVED" {
		t.Errorf("review at head: ok=%v AtHead=%v State=%q, want true/true/APPROVED", ok, rs.AtHead, rs.State)
	}
	// Reviewed an older commit -> stale.
	if rs, ok := mk("def", "abc", "CHANGES_REQUESTED", true).toReviewState(); !ok || rs.AtHead {
		t.Errorf("stale review: ok=%v AtHead=%v, want true/false", ok, rs.AtHead)
	}
	// Never reviewed -> no verdict.
	if _, ok := mk("abc", "", "", false).toReviewState(); ok {
		t.Error("unreviewed PR should produce no ReviewState")
	}
	// Missing head commit -> no verdict.
	if _, ok := mk("", "abc", "APPROVED", true).toReviewState(); ok {
		t.Error("PR without a head commit should produce no ReviewState")
	}
}
