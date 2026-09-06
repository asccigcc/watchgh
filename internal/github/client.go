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
	"time"
)

const apiBase = "https://api.github.com"

// Client talks to the GitHub API with a resolved token.
type Client struct {
	http  *http.Client
	token string
}

// New resolves a token and returns a Client, or an error explaining how to
// authenticate if none is found.
func New() (*Client, error) {
	token, err := resolveToken()
	if err != nil {
		return nil, err
	}
	return &Client{
		http:  &http.Client{Timeout: 20 * time.Second},
		token: token,
	}, nil
}

// resolveToken prefers GITHUB_TOKEN, then falls back to `gh auth token`.
func resolveToken() (string, error) {
	if t := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); t != "" {
		return t, nil
	}
	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return "", fmt.Errorf("no token found: set GITHUB_TOKEN or run `gh auth login`")
	}
	t := strings.TrimSpace(string(out))
	if t == "" {
		return "", fmt.Errorf("no token found: set GITHUB_TOKEN or run `gh auth login`")
	}
	return t, nil
}

// get performs an authenticated GET and decodes JSON into v. url may be a path
// ("/notifications") or an absolute API URL (as returned in payloads).
func (c *Client) get(ctx context.Context, url string, v any) error {
	if strings.HasPrefix(url, "/") {
		url = apiBase + url
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("GET %s: %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
	}
	if v == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// PollMeta carries the polling hints from a notifications response.
type PollMeta struct {
	PollInterval time.Duration // server-requested minimum poll spacing (X-Poll-Interval)
	LastModified string        // echo back as If-Modified-Since next poll
	NotModified  bool          // 304: nothing changed since LastModified
}

// getWithMeta is like get but sends If-Modified-Since and reports the poll
// hints. On 304 it returns NotModified without decoding.
func (c *Client) getWithMeta(ctx context.Context, url, ifModifiedSince string, v any) (PollMeta, error) {
	var meta PollMeta
	if strings.HasPrefix(url, "/") {
		url = apiBase + url
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return meta, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if ifModifiedSince != "" {
		req.Header.Set("If-Modified-Since", ifModifiedSince)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return meta, err
	}
	defer resp.Body.Close()

	meta.LastModified = resp.Header.Get("Last-Modified")
	if s := resp.Header.Get("X-Poll-Interval"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			meta.PollInterval = time.Duration(n) * time.Second
		}
	}
	if resp.StatusCode == http.StatusNotModified {
		meta.NotModified = true
		return meta, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return meta, fmt.Errorf("GET %s: %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
	}
	if v != nil {
		if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
			return meta, err
		}
	}
	return meta, nil
}

// patch performs an authenticated PATCH with no body and discards the response.
func (c *Client) patch(ctx context.Context, url string) error {
	if strings.HasPrefix(url, "/") {
		url = apiBase + url
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("PATCH %s: %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}
