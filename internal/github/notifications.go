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
func (c *Client) Notifications(ctx context.Context) ([]Notification, error) {
	var all []Notification
	for page := 1; ; page++ {
		var ns []Notification
		url := fmt.Sprintf("/notifications?per_page=100&page=%d", page)
		if err := c.get(ctx, url, &ns); err != nil {
			return nil, err
		}
		if len(ns) == 0 {
			break
		}
		all = append(all, ns...)
		if len(ns) < 100 {
			break
		}
	}
	return all, nil
}

// CheckNotifications is a cheap conditional GET used by the watch loop as a
// change detector: it reports whether anything changed since ifModifiedSince,
// plus the server's requested poll spacing. A 304 costs no rate limit.
func (c *Client) CheckNotifications(ctx context.Context, ifModifiedSince string) (changed bool, lastModified string, poll time.Duration, err error) {
	var discard []Notification
	meta, err := c.getWithMeta(ctx, "/notifications?per_page=50", ifModifiedSince, &discard)
	if err != nil {
		return false, "", 0, err
	}
	return !meta.NotModified, meta.LastModified, meta.PollInterval, nil
}

// MarkThreadRead marks a notification thread read on GitHub's side.
func (c *Client) MarkThreadRead(ctx context.Context, threadID string) error {
	return c.patch(ctx, "/notifications/threads/"+threadID)
}

// EnrichSubject fetches the notification subject (PR/issue) to recover the
// author, web URL, and number that the notifications payload omits.
func (c *Client) EnrichSubject(ctx context.Context, subjectURL string) (Subject, error) {
	var s Subject
	if subjectURL == "" {
		return s, nil
	}
	err := c.get(ctx, subjectURL, &s)
	return s, err
}
