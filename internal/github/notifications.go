package github

import (
	"context"
	"fmt"
	"time"
)

// Notification is a single thread from GET /notifications.
type Notification struct {
	ID        string    `json:"id"`
	Unread    bool      `json:"unread"`
	Reason    string    `json:"reason"`
	UpdatedAt time.Time `json:"updated_at"`
	Subject   struct {
		Title            string `json:"title"`
		URL              string `json:"url"`
		LatestCommentURL string `json:"latest_comment_url"`
		Type             string `json:"type"`
	} `json:"subject"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// Subject holds the enriched fields we pull from a notification's subject URL
// (the notifications payload itself lacks author and web URL).
type Subject struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	Draft   bool   `json:"draft"`
	User    struct {
		Login string `json:"login"`
	} `json:"user"`
}

// User is the authenticated viewer.
type User struct {
	Login string `json:"login"`
}

// Viewer returns the authenticated user (used to decide "is this my PR?").
func (c *Client) Viewer(ctx context.Context) (User, error) {
	var u User
	err := c.get(ctx, "/user", &u)
	return u, err
}

// Notifications lists the current unread notification threads across all pages.
//
// Page 1 is a conditional request: if nothing changed since the last poll GitHub
// answers 304 (which costs no rate-limit quota) and we return the cached list.
// Any new or updated thread bumps to the top, so an unchanged page 1 means an
// unchanged feed. Only single-page results are cached — a rare multi-page feed's
// page-1 ETag can't vouch for later pages, so it takes the full path each time.
func (c *Client) Notifications(ctx context.Context) ([]Notification, error) {
	var first []Notification
	notModified, etag, err := c.getCond(ctx, "/notifications?per_page=100&page=1", c.notifETag, &first)
	if err != nil {
		return nil, err
	}
	if notModified {
		return c.notifCache, nil
	}

	all := first
	if len(first) == 100 { // a full first page means there may be more
		for page := 2; ; page++ {
			var ns []Notification
			url := fmt.Sprintf("/notifications?per_page=100&page=%d", page)
			if _, _, err := c.getCond(ctx, url, "", &ns); err != nil {
				return nil, err
			}
			all = append(all, ns...)
			if len(ns) < 100 {
				break
			}
		}
	}

	if len(first) < 100 { // single page: safe to cache for the 304 fast path
		c.notifETag, c.notifCache = etag, all
	} else {
		c.notifETag, c.notifCache = "", nil
	}
	return all, nil
}

// MarkThreadRead marks a notification thread read on GitHub's side.
func (c *Client) MarkThreadRead(ctx context.Context, threadID string) error {
	return c.patch(ctx, "/notifications/threads/"+threadID)
}

// EnrichSubject fetches the notification subject (PR/issue) to recover the
// author, web URL, and number that the notifications payload omits.
//
// Results are cached by subject URL: the fields we read (number, html_url,
// author) are immutable for a PR/issue, so a hit skips the per-notification GET
// that otherwise turns each poll into an N+1 against the API.
func (c *Client) EnrichSubject(ctx context.Context, subjectURL string) (Subject, error) {
	if subjectURL == "" {
		return Subject{}, nil
	}
	c.mu.Lock()
	s, ok := c.subjects[subjectURL]
	c.mu.Unlock()
	if ok {
		return s, nil
	}
	if err := c.get(ctx, subjectURL, &s); err != nil {
		return s, err
	}
	c.mu.Lock()
	if c.subjects == nil {
		c.subjects = make(map[string]Subject)
	}
	c.subjects[subjectURL] = s
	c.mu.Unlock()
	return s, nil
}
