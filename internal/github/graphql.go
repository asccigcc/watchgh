package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// TrackedPR is a normalized open PR we watch for CI/merge transitions.
type TrackedPR struct {
	Number     int
	URL        string
	Title      string
	IsDraft    bool
	Author     string // login
	Repo       string // owner/name
	MergeState string // CLEAN, DIRTY, BLOCKED, BEHIND, UNSTABLE, UNKNOWN, ...
	HeadSHA    string
	CIState    string // SUCCESS, FAILURE, ERROR, PENDING, EXPECTED, or "" (no checks)
	CheckCount int
	UpdatedAt  time.Time // last PR update, for the Mine-roster row's age
}

const trackedQuery = `
query {
  mine: search(query: "is:open is:pr author:@me", type: ISSUE, first: 50) {
    nodes { ...pr }
  }
  assigned: search(query: "is:open is:pr assignee:@me", type: ISSUE, first: 50) {
    nodes { ...pr }
  }
}
fragment pr on PullRequest {
  number url title isDraft updatedAt
  author { login }
  repository { nameWithOwner }
  mergeStateStatus
  commits(last: 1) {
    nodes { commit { oid statusCheckRollup { state contexts { totalCount } } } }
  }
}`

// TrackedPRs fetches the viewer's open + assigned PRs, deduped by repo#number.
func (c *Client) TrackedPRs(ctx context.Context) ([]TrackedPR, error) {
	var resp struct {
		Data struct {
			Mine     searchResult `json:"mine"`
			Assigned searchResult `json:"assigned"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := c.graphql(ctx, trackedQuery, &resp); err != nil {
		return nil, err
	}
	if len(resp.Errors) > 0 {
		return nil, fmt.Errorf("graphql: %s", resp.Errors[0].Message)
	}

	seen := map[string]bool{}
	var out []TrackedPR
	for _, n := range append(resp.Data.Mine.Nodes, resp.Data.Assigned.Nodes...) {
		if n.Number == 0 { // non-PullRequest node
			continue
		}
		pr := n.toTrackedPR()
		key := fmt.Sprintf("%s#%d", pr.Repo, pr.Number)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, pr)
	}
	return out, nil
}

type searchResult struct {
	Nodes []prNode `json:"nodes"`
}

type prNode struct {
	Number    int    `json:"number"`
	URL       string `json:"url"`
	Title     string `json:"title"`
	IsDraft   bool   `json:"isDraft"`
	UpdatedAt string `json:"updatedAt"`
	Author    struct {
		Login string `json:"login"`
	} `json:"author"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	MergeStateStatus string `json:"mergeStateStatus"`
	Commits          struct {
		Nodes []struct {
			Commit struct {
				Oid               string `json:"oid"`
				StatusCheckRollup *struct {
					State    string `json:"state"`
					Contexts struct {
						TotalCount int `json:"totalCount"`
					} `json:"contexts"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

func (n prNode) toTrackedPR() TrackedPR {
	pr := TrackedPR{
		Number:     n.Number,
		URL:        n.URL,
		Title:      n.Title,
		IsDraft:    n.IsDraft,
		Author:     n.Author.Login,
		Repo:       n.Repository.NameWithOwner,
		MergeState: n.MergeStateStatus,
	}
	if t, err := time.Parse(time.RFC3339, n.UpdatedAt); err == nil {
		pr.UpdatedAt = t
	}
	if len(n.Commits.Nodes) > 0 {
		commit := n.Commits.Nodes[0].Commit
		pr.HeadSHA = commit.Oid
		if commit.StatusCheckRollup != nil {
			pr.CIState = commit.StatusCheckRollup.State
			pr.CheckCount = commit.StatusCheckRollup.Contexts.TotalCount
		}
	}
	return pr
}

// ReviewState reports, for a PR the viewer has already reviewed, whether that
// latest review covers the PR's current head commit. PRs the viewer has never
// reviewed produce no ReviewState (they stay ordinary review-request items).
type ReviewState struct {
	Repo   string // owner/name
	Number int
	AtHead bool // the viewer's latest review is against the current head commit
}

const reviewedQuery = `
query {
  reviewedBy: search(query: "is:open is:pr reviewed-by:@me", type: ISSUE, first: 50) {
    nodes { ...reviewed }
  }
}
fragment reviewed on PullRequest {
  number
  repository { nameWithOwner }
  commits(last: 1) { nodes { commit { oid } } }
  viewerLatestReview { commit { oid } }
}`

// ReviewStates fetches the viewer's open PRs they've reviewed and reports, for
// each, whether the latest review is still against the head commit. This is what
// lets watchgit auto-resolve a review request once you've reviewed the current
// head and re-surface it when new commits land on top of your review.
func (c *Client) ReviewStates(ctx context.Context) ([]ReviewState, error) {
	var resp struct {
		Data struct {
			ReviewedBy struct {
				Nodes []reviewNode `json:"nodes"`
			} `json:"reviewedBy"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := c.graphql(ctx, reviewedQuery, &resp); err != nil {
		return nil, err
	}
	if len(resp.Errors) > 0 {
		return nil, fmt.Errorf("graphql: %s", resp.Errors[0].Message)
	}

	var out []ReviewState
	for _, n := range resp.Data.ReviewedBy.Nodes {
		if rs, ok := n.toReviewState(); ok {
			out = append(out, rs)
		}
	}
	return out, nil
}

type reviewNode struct {
	Number     int `json:"number"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				Oid string `json:"oid"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
	ViewerLatestReview *struct {
		Commit struct {
			Oid string `json:"oid"`
		} `json:"commit"`
	} `json:"viewerLatestReview"`
}

// toReviewState maps a node to a ReviewState, reporting ok=false for a
// non-PullRequest node or one missing the review/head data needed to judge it.
func (n reviewNode) toReviewState() (ReviewState, bool) {
	if n.Number == 0 || n.ViewerLatestReview == nil || len(n.Commits.Nodes) == 0 {
		return ReviewState{}, false
	}
	head := n.Commits.Nodes[0].Commit.Oid
	if head == "" {
		return ReviewState{}, false
	}
	return ReviewState{
		Repo:   n.Repository.NameWithOwner,
		Number: n.Number,
		AtHead: n.ViewerLatestReview.Commit.Oid == head,
	}, true
}

// graphql POSTs a query and decodes the response into out.
func (c *Client) graphql(ctx context.Context, query string, out any) error {
	body, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+"/graphql", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	// mergeStateStatus lives behind the merge-info preview.
	req.Header.Set("Accept", "application/vnd.github.merge-info-preview+json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("graphql: %s: %s", resp.Status, b)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
