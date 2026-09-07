package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testClient wires a Client to a test server's URL, so get/graphql hit the stub
// instead of api.github.com.
func testClient(srv *httptest.Server) *Client {
	return &Client{http: srv.Client(), token: "t", baseURL: srv.URL}
}

// gqlRequest is the decoded body of a GraphQL POST, enough to read $after.
type gqlRequest struct {
	Variables struct {
		After string `json:"after"`
	} `json:"variables"`
}

func TestTrackedPRsPaginates(t *testing.T) {
	// Each search (mine, assigned) returns two pages: page 1 has hasNextPage
	// true + a cursor, page 2 closes it. The stub keys off the $after variable.
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, _ := io.ReadAll(r.Body)
		var req gqlRequest
		json.Unmarshal(body, &req)

		node := func(repo string, num int) string {
			return `{"number":` + itoa(num) + `,"url":"u","title":"t","isDraft":false,` +
				`"updatedAt":"2026-01-01T00:00:00Z","author":{"login":"a"},` +
				`"repository":{"nameWithOwner":"` + repo + `"},"mergeStateStatus":"CLEAN",` +
				`"commits":{"nodes":[]}}`
		}
		if req.Variables.After == "" { // page 1
			io.WriteString(w, `{"data":{"search":{"pageInfo":{"hasNextPage":true,"endCursor":"C1"},"nodes":[`+node("o/r", 1)+`]}}}`)
		} else { // page 2
			io.WriteString(w, `{"data":{"search":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[`+node("o/r", 2)+`]}}}`)
		}
	}))
	defer srv.Close()

	prs, err := testClient(srv).TrackedPRs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Two searches × two pages = 4 requests; #1 and #2 collected, deduped once.
	if calls != 4 {
		t.Errorf("made %d requests, want 4 (2 searches × 2 pages)", calls)
	}
	got := map[int]bool{}
	for _, pr := range prs {
		got[pr.Number] = true
	}
	if !got[1] || !got[2] {
		t.Errorf("paginated PRs = %+v, want both #1 and #2", prs)
	}
}

func TestGraphQLErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"errors":[{"message":"bad query"}]}`)
	}))
	defer srv.Close()

	_, err := testClient(srv).TrackedPRs(context.Background())
	if err == nil || !strings.Contains(err.Error(), "bad query") {
		t.Errorf("err = %v, want it to surface the GraphQL error", err)
	}
}

// itoa is a tiny local helper so the test body stays free of strconv noise.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
