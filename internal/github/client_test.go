package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNotificationsUsesETagCache(t *testing.T) {
	var bodyServed, notModified int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == "v1" {
			notModified++
			w.WriteHeader(http.StatusNotModified)
			return
		}
		bodyServed++
		w.Header().Set("ETag", "v1")
		io.WriteString(w, `[{"id":"n1","unread":true,"reason":"review_requested"}]`)
	}))
	defer srv.Close()
	c := testClient(srv)

	first, err := c.Notifications(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].ID != "n1" {
		t.Fatalf("first poll = %+v, want one thread n1", first)
	}

	// Second poll sends If-None-Match: the stub answers 304 and we reuse cache.
	again, err := c.Notifications(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 || again[0].ID != "n1" {
		t.Fatalf("second poll = %+v, want the cached thread", again)
	}
	if bodyServed != 1 || notModified != 1 {
		t.Errorf("bodyServed=%d notModified=%d, want 1 and 1 (304 fast path)", bodyServed, notModified)
	}
}

func TestEnrichSubjectCaches(t *testing.T) {
	var gets int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets++
		io.WriteString(w, `{"number":7,"html_url":"h","user":{"login":"alice"}}`)
	}))
	defer srv.Close()
	c := testClient(srv)
	url := srv.URL + "/repos/o/r/pulls/7"

	for i := 0; i < 3; i++ {
		s, err := c.EnrichSubject(context.Background(), url)
		if err != nil {
			t.Fatal(err)
		}
		if s.Number != 7 || s.User.Login != "alice" {
			t.Fatalf("subject = %+v, want number 7 / alice", s)
		}
	}
	if gets != 1 {
		t.Errorf("made %d subject GETs, want 1 (cached thereafter)", gets)
	}
}

func TestRateLimitErrorTyped(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    time.Duration
	}{
		{"429 with Retry-After", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
		}, 30 * time.Second},
		{"403 with quota exhausted", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.WriteHeader(http.StatusForbidden)
		}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			err := testClient(srv).get(context.Background(), "/user", &struct{}{})
			var rl *RateLimitError
			if !errors.As(err, &rl) {
				t.Fatalf("err = %v, want *RateLimitError", err)
			}
			if rl.RetryAfter != tc.want {
				t.Errorf("RetryAfter = %s, want %s", rl.RetryAfter, tc.want)
			}
		})
	}
}

func TestNonRateLimitErrorStaysGeneric(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	err := testClient(srv).get(context.Background(), "/user", &struct{}{})
	var rl *RateLimitError
	if errors.As(err, &rl) {
		t.Fatalf("a 500 should not be a RateLimitError, got %v", err)
	}
	if err == nil {
		t.Fatal("expected an error for a 500")
	}
}
