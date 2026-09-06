package github

import "testing"

func TestToReviewState(t *testing.T) {
	mk := func(head, reviewOID string, hasReview bool) reviewNode {
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
				Commit struct {
					Oid string `json:"oid"`
				} `json:"commit"`
			}{Commit: struct {
				Oid string `json:"oid"`
			}{Oid: reviewOID}}
		}
		return n
	}

	// Reviewed the current head -> AtHead.
	if rs, ok := mk("abc", "abc", true).toReviewState(); !ok || !rs.AtHead {
		t.Errorf("review at head: ok=%v AtHead=%v, want true/true", ok, rs.AtHead)
	}
	// Reviewed an older commit -> stale.
	if rs, ok := mk("def", "abc", true).toReviewState(); !ok || rs.AtHead {
		t.Errorf("stale review: ok=%v AtHead=%v, want true/false", ok, rs.AtHead)
	}
	// Never reviewed -> no verdict.
	if _, ok := mk("abc", "", false).toReviewState(); ok {
		t.Error("unreviewed PR should produce no ReviewState")
	}
	// Missing head commit -> no verdict.
	if _, ok := mk("", "abc", true).toReviewState(); ok {
		t.Error("PR without a head commit should produce no ReviewState")
	}
}
