package github

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSplitRepo(t *testing.T) {
	cases := []struct {
		in          string
		owner, name string
		ok          bool
	}{
		{"acme/api", "acme", "api", true},
		{"acme", "", "", false},  // no slash
		{"acme/", "", "", false}, // empty name
		{"/api", "", "", false},  // empty owner
		{"a/b/c", "", "", false}, // nested path, not a repo
	}
	for _, tc := range cases {
		owner, name, ok := splitRepo(tc.in)
		if ok != tc.ok || owner != tc.owner || name != tc.name {
			t.Errorf("splitRepo(%q) = (%q,%q,%v), want (%q,%q,%v)",
				tc.in, owner, name, ok, tc.owner, tc.name, tc.ok)
		}
	}
}

func TestPRStatesResolvesAndToleratesNulls(t *testing.T) {
	// Aliases a0/a1/a2 map to the three requested PRs; the middle one resolves to
	// null (deleted / access lost) and must be omitted, not fail the batch.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":{
			"a0":{"pullRequest":{"state":"MERGED"}},
			"a1":{"pullRequest":null},
			"a2":{"pullRequest":{"state":"OPEN"}}
		}}`)
	}))
	defer srv.Close()

	refs := []PRRef{{"acme/api", 1}, {"acme/api", 2}, {"acme/web", 3}}
	got, err := testClient(srv).PRStates(context.Background(), refs)
	if err != nil {
		t.Fatal(err)
	}
	if got["acme/api#1"] != "MERGED" || got["acme/web#3"] != "OPEN" {
		t.Errorf("resolved states = %v, want #1 MERGED and #3 OPEN", got)
	}
	if _, ok := got["acme/api#2"]; ok {
		t.Errorf("null PR should be omitted, got %q", got["acme/api#2"])
	}
}

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
