// Package github is a thin GitHub REST/GraphQL client for watchgh.
//
// It piggybacks on the user's existing credentials rather than implementing
// OAuth: GITHUB_TOKEN if set, otherwise `gh auth token`.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const apiBase = "https://api.github.com"

// Client talks to the GitHub API with a resolved token.
type Client struct {
	http    *http.Client
	token   string
	baseURL string // API root; overridable in tests, defaults to apiBase

	mu       sync.Mutex         // guards the subject cache (EnrichSubject runs concurrently)
	subjects map[string]Subject // enriched notification subjects, keyed by subject URL

	// Notifications' conditional-request cache. Never touched concurrently:
	// callers single-flight their syncs, so no lock is needed here.
	notifETag  string
	notifCache []Notification
}

// New resolves a token and returns a Client, or an error explaining how to
// authenticate if none is found.
func New() (*Client, error) {
	token, err := resolveToken()
	if err != nil {
		return nil, err
	}
	return &Client{
		http:    &http.Client{Timeout: 20 * time.Second},
		token:   token,
		baseURL: apiBase,
	}, nil
}

// resolveToken prefers GITHUB_TOKEN, then falls back to `gh auth token`. The
// fallback is bounded so a wedged `gh` can't hang startup indefinitely.
func resolveToken() (string, error) {
	if t := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); t != "" {
		return t, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	if err != nil {
		return "", fmt.Errorf("no token found: set GITHUB_TOKEN or run `gh auth login`")
	}
	t := strings.TrimSpace(string(out))
	if t == "" {
		return "", fmt.Errorf("no token found: set GITHUB_TOKEN or run `gh auth login`")
	}
	return t, nil
}

// restRequest builds an authenticated REST request with the standard headers.
// url may be a path ("/notifications") or an absolute API URL (as returned in
// payloads).
func (c *Client) restRequest(ctx context.Context, method, url string) (*http.Request, error) {
	if strings.HasPrefix(url, "/") {
		url = c.baseURL + url
	}
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	return req, nil
}

// httpError turns a non-2xx response into an error, surfacing rate-limit
// rejections as a typed *RateLimitError. The caller must not have read the body.
func httpError(resp *http.Response) error {
	if rl := rateLimitError(resp); rl != nil {
		return rl
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("%s %s: %s: %s",
		resp.Request.Method, resp.Request.URL, resp.Status, strings.TrimSpace(string(body)))
}

// get performs an authenticated GET and decodes JSON into v.
func (c *Client) get(ctx context.Context, url string, v any) error {
	req, err := c.restRequest(ctx, http.MethodGet, url)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return httpError(resp)
	}
	if v == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// getCond performs a conditional GET: it sends If-None-Match with etag and, on a
// 304, reports notModified=true and leaves v untouched (a 304 costs no rate-limit
// quota). Otherwise it decodes into v and returns the response's ETag to cache.
func (c *Client) getCond(ctx context.Context, url, etag string, v any) (notModified bool, newETag string, err error) {
	req, err := c.restRequest(ctx, http.MethodGet, url)
	if err != nil {
		return false, "", err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return true, etag, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, "", httpError(resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return false, "", err
	}
	return false, resp.Header.Get("ETag"), nil
}

// patch performs an authenticated PATCH with no body and discards the response.
func (c *Client) patch(ctx context.Context, url string) error {
	req, err := c.restRequest(ctx, http.MethodPatch, url)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return httpError(resp)
	}
	return nil
}

// RateLimitError reports that GitHub applied a primary or secondary rate limit.
// RetryAfter is how long the server advised waiting (0 when it gave no hint).
type RateLimitError struct {
	Status     string
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("github rate limited (%s); retry after %s", e.Status, e.RetryAfter.Round(time.Second))
	}
	return fmt.Sprintf("github rate limited (%s)", e.Status)
}

// rateLimitError returns a *RateLimitError when resp is a rate-limit rejection
// (HTTP 429, or 403 with the remaining quota exhausted), else nil.
func rateLimitError(resp *http.Response) *RateLimitError {
	limited := resp.StatusCode == http.StatusTooManyRequests ||
		(resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0")
	if !limited {
		return nil
	}
	return &RateLimitError{Status: resp.Status, RetryAfter: retryAfter(resp.Header)}
}

// retryAfter derives the advised wait from Retry-After (delay in seconds) or,
// failing that, X-RateLimit-Reset (a Unix timestamp).
func retryAfter(h http.Header) time.Duration {
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	if v := strings.TrimSpace(h.Get("X-RateLimit-Reset")); v != "" {
		if ts, err := strconv.ParseInt(v, 10, 64); err == nil {
			if d := time.Until(time.Unix(ts, 0)); d > 0 {
				return d
			}
		}
	}
	return 0
}
