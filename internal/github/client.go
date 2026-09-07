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
