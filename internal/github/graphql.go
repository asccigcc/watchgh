package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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

// trackedQuery pages one search (author:@me or assignee:@me) 50 nodes at a time;
// searchNodes walks pageInfo.endCursor until GitHub reports no next page.
const trackedQuery = `
query($q: String!, $after: String) {
  search(query: $q, type: ISSUE, first: 50, after: $after) {
    pageInfo { hasNextPage endCursor }
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
// Both searches paginate fully, so a viewer with more than one page of open or
// assigned PRs is tracked in full rather than silently truncated at 50.
func (c *Client) TrackedPRs(ctx context.Context) ([]TrackedPR, error) {
	mine, err := searchNodes[prNode](ctx, c, trackedQuery, "is:open is:pr author:@me")
	if err != nil {
		return nil, err
	}
	assigned, err := searchNodes[prNode](ctx, c, trackedQuery, "is:open is:pr assignee:@me")
	if err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	var out []TrackedPR
	for _, group := range [][]prNode{mine, assigned} {
		for _, n := range group {
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
	}
	return out, nil
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
	AtHead bool   // the viewer's latest review is against the current head commit
	State  string // the viewer's latest review verdict: APPROVED, CHANGES_REQUESTED, COMMENTED, …
}

const reviewedQuery = `
query($q: String!, $after: String) {
  search(query: $q, type: ISSUE, first: 50, after: $after) {
    pageInfo { hasNextPage endCursor }
    nodes { ...reviewed }
  }
}
fragment reviewed on PullRequest {
  number
  repository { nameWithOwner }
  commits(last: 1) { nodes { commit { oid } } }
  viewerLatestReview { state commit { oid } }
}`

// ReviewStates fetches the viewer's open PRs they've reviewed and reports, for
// each, whether the latest review is still against the head commit. This is what
// lets watchgh auto-resolve a review request once you've reviewed the current
// head and re-surface it when new commits land on top of your review. It pages
// through every reviewed PR, so a heavy reviewer's requests still auto-resolve
// past the first 50.
func (c *Client) ReviewStates(ctx context.Context) ([]ReviewState, error) {
	nodes, err := searchNodes[reviewNode](ctx, c, reviewedQuery, "is:open is:pr reviewed-by:@me")
	if err != nil {
		return nil, err
	}
	var out []ReviewState
	for _, n := range nodes {
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
		State  string `json:"state"`
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
		State:  n.ViewerLatestReview.State,
	}, true
}

// PRRef identifies a pull request by its repo and number — the pair stored
// rows carry, and all PRStates needs to look up a PR's lifecycle state.
type PRRef struct {
	Repo   string // owner/name
	Number int
}

// Key is the "owner/name#number" string used to key a PR across the store and
// the github client, so a lookup result maps back to the row that asked for it.
func (r PRRef) Key() string { return fmt.Sprintf("%s#%d", r.Repo, r.Number) }

// PRStates looks up the lifecycle state (OPEN, CLOSED, MERGED) of each PR,
// keyed by PRRef.Key(). It batches the lookups into aliased repository queries
// (50 per request) so watchgh can reap rows for PRs that have since merged or
// closed without a per-PR round trip. A PR that can't be resolved (deleted, or
// access lost) is simply omitted rather than failing the batch, so a single
// dead reference never blocks reaping the rest.
func (c *Client) PRStates(ctx context.Context, refs []PRRef) (map[string]string, error) {
	out := make(map[string]string, len(refs))
	const batch = 50
	for start := 0; start < len(refs); start += batch {
		end := start + batch
		if end > len(refs) {
			end = len(refs)
		}
		if err := c.prStatesBatch(ctx, refs[start:end], out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// prStatesBatch resolves one ≤50-PR chunk, writing resolved states into out.
func (c *Client) prStatesBatch(ctx context.Context, chunk []PRRef, out map[string]string) error {
	var head, body strings.Builder
	head.WriteString("query(")
	vars := map[string]any{}
	valid := make([]PRRef, 0, len(chunk))
	for _, r := range chunk {
		owner, name, ok := splitRepo(r.Repo)
		if !ok {
			continue
		}
		i := len(valid)
		valid = append(valid, r)
		if i > 0 {
			head.WriteString(", ")
		}
		fmt.Fprintf(&head, "$o%d:String!, $n%d:String!, $p%d:Int!", i, i, i)
		fmt.Fprintf(&body, " a%d: repository(owner:$o%d, name:$n%d){ pullRequest(number:$p%d){ state } }", i, i, i, i)
		vars[fmt.Sprintf("o%d", i)] = owner
		vars[fmt.Sprintf("n%d", i)] = name
		vars[fmt.Sprintf("p%d", i)] = r.Number
	}
	if len(valid) == 0 {
		return nil
	}
	head.WriteString(") {")
	query := head.String() + body.String() + " }"

	var resp struct {
		Data map[string]*struct {
			PullRequest *struct {
				State string `json:"state"`
			} `json:"pullRequest"`
		} `json:"data"`
		// Errors are ignored on purpose: GitHub returns partial data with a
		// per-node error for an unresolvable PR, and a null node just means
		// "unknown" — we only reap on an explicit MERGED/CLOSED, never on absence.
	}
	if err := c.graphql(ctx, query, vars, &resp); err != nil {
		return err
	}
	for i, r := range valid {
		if node := resp.Data[fmt.Sprintf("a%d", i)]; node != nil && node.PullRequest != nil {
			out[r.Key()] = node.PullRequest.State
		}
	}
	return nil
}

// splitRepo splits an "owner/name" repo into its parts, reporting ok=false for
// anything that isn't exactly one slash-separated pair.
func splitRepo(repo string) (owner, name string, ok bool) {
	i := strings.IndexByte(repo, '/')
	if i <= 0 || i == len(repo)-1 || strings.IndexByte(repo[i+1:], '/') >= 0 {
		return "", "", false
	}
	return repo[:i], repo[i+1:], true
}

// pageInfo is the cursor slice of a GraphQL connection.
type pageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

// gqlError is one entry of a GraphQL response's top-level errors array.
type gqlError struct {
	Message string `json:"message"`
}

// searchNodes runs a paginated `search` query for the given search string and
// returns every node across all pages. query must declare $q and $after and
// select `search { pageInfo { hasNextPage endCursor } nodes { … } }`. It is a
// free function (not a method) because Go methods can't take type parameters.
func searchNodes[N any](ctx context.Context, c *Client, query, q string) ([]N, error) {
	var all []N
	after := ""
	for {
		var resp struct {
			Data struct {
				Search struct {
					PageInfo pageInfo `json:"pageInfo"`
					Nodes    []N      `json:"nodes"`
				} `json:"search"`
			} `json:"data"`
			Errors []gqlError `json:"errors"`
		}
		vars := map[string]any{"q": q}
		if after != "" {
			vars["after"] = after
		}
		if err := c.graphql(ctx, query, vars, &resp); err != nil {
			return nil, err
		}
		if len(resp.Errors) > 0 {
			return nil, fmt.Errorf("graphql: %s", resp.Errors[0].Message)
		}
		all = append(all, resp.Data.Search.Nodes...)
		pi := resp.Data.Search.PageInfo
		if !pi.HasNextPage || pi.EndCursor == "" {
			return all, nil
		}
		after = pi.EndCursor
	}
}

// graphql POSTs a query (with optional variables) and decodes the response into
// out.
func (c *Client) graphql(ctx context.Context, query string, vars map[string]any, out any) error {
	payload := map[string]any{"query": query}
	if len(vars) > 0 {
		payload["variables"] = vars
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/graphql", bytes.NewReader(body))
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
		if rl := rateLimitError(resp); rl != nil {
			return rl
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("graphql: %s: %s", resp.Status, b)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
