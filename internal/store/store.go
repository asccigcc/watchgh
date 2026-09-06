// Package store persists timeline events in SQLite so they dedupe across runs,
// remember read/unread state, and can be pruned by age.
package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"time"

	"watchgit/internal/timeline"

	_ "modernc.org/sqlite"
)

// Store is a SQLite-backed event log.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS events (
  seq           INTEGER PRIMARY KEY AUTOINCREMENT,
  id            TEXT UNIQUE NOT NULL,
  thread_id     TEXT,
  source        TEXT,
  ts            INTEGER,
  kind          INTEGER,
  repo          TEXT,
  number        INTEGER,
  author        TEXT,
  detail        TEXT,
  url           TEXT,
  github_unread INTEGER,
  actionable    INTEGER,
  is_mine       INTEGER,
  first_seen    INTEGER,
  last_seen     INTEGER,
  read_at       INTEGER
);
CREATE INDEX IF NOT EXISTS idx_events_ts ON events(ts);

CREATE TABLE IF NOT EXISTS pr_state (
  pr_key      TEXT PRIMARY KEY,
  head_sha    TEXT,
  ci_state    TEXT,
  merge_state TEXT,
  updated_at  INTEGER
);

CREATE TABLE IF NOT EXISTS open_prs (
  pr_key      TEXT PRIMARY KEY,
  repo        TEXT,
  number      INTEGER,
  title       TEXT,
  url         TEXT,
  author      TEXT,
  is_draft    INTEGER,
  ci_state    TEXT,
  merge_state TEXT,
  updated_at  INTEGER,
  last_seen   INTEGER
);
`

// PRState is the last-seen CI/merge status for a tracked PR; the diff engine
// compares against it to emit events only on transitions.
type PRState struct {
	Key        string
	HeadSHA    string
	CIState    string
	MergeState string
}

// OpenPR is a snapshot of one of the viewer's open PRs, kept as a roster so the
// Mine tab can list every open PR — even one with no timeline activity yet.
type OpenPR struct {
	Key        string
	Repo       string
	Number     int
	Title      string
	URL        string
	Author     string
	IsDraft    bool
	CIState    string
	MergeState string
	UpdatedAt  time.Time
}

// DefaultPath is the per-user database location (~/.config/watchgit/watchgit.db).
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "watchgit", "watchgit.db"), nil
}

// Open opens (creating parent dirs and schema as needed) the store at path.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// WAL lets a reader (CLI) run while the daemon writes; busy_timeout rides
	// out brief lock contention between the two processes.
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Serialize this process's own access; cross-process is handled by WAL.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Upsert inserts a new event or refreshes the mutable fields of an existing one
// (matched by ID), preserving seq, first_seen, and any local read_at.
func (s *Store) Upsert(e timeline.Event) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(`
INSERT INTO events
  (id, thread_id, source, ts, kind, repo, number, author, detail, url,
   github_unread, actionable, is_mine, first_seen, last_seen)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET
  ts=excluded.ts, detail=excluded.detail, github_unread=excluded.github_unread,
  actionable=excluded.actionable, last_seen=excluded.last_seen`,
		e.ID, e.ThreadID, e.Source, e.TS.Unix(), int(e.Kind), e.Repo, e.Number,
		e.Author, e.Detail, e.URL, boolToInt(e.Unread), boolToInt(e.Actionable),
		boolToInt(e.IsMine), now, now)
	return err
}

// UpsertAll upserts a batch in one transaction.
func (s *Store) UpsertAll(events []timeline.Event) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, e := range events {
		if err := s.upsertTx(tx, e); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) upsertTx(tx *sql.Tx, e timeline.Event) error {
	now := time.Now().Unix()
	_, err := tx.Exec(`
INSERT INTO events
  (id, thread_id, source, ts, kind, repo, number, author, detail, url,
   github_unread, actionable, is_mine, first_seen, last_seen)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET
  ts=excluded.ts, detail=excluded.detail, github_unread=excluded.github_unread,
  actionable=excluded.actionable, last_seen=excluded.last_seen`,
		e.ID, e.ThreadID, e.Source, e.TS.Unix(), int(e.Kind), e.Repo, e.Number,
		e.Author, e.Detail, e.URL, boolToInt(e.Unread), boolToInt(e.Actionable),
		boolToInt(e.IsMine), now, now)
	return err
}

// List returns all stored events oldest-first. Effective unread = not locally
// read AND still unread on GitHub's side.
func (s *Store) List() ([]timeline.Event, error) {
	rows, err := s.db.Query(`
SELECT seq, id, thread_id, source, ts, kind, repo, number, author, detail, url,
       (read_at IS NULL AND github_unread=1), actionable, is_mine
FROM events ORDER BY ts ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []timeline.Event
	for rows.Next() {
		var e timeline.Event
		var ts int64
		var kind int
		var unread, actionable, isMine int
		if err := rows.Scan(&e.Seq, &e.ID, &e.ThreadID, &e.Source, &ts, &kind,
			&e.Repo, &e.Number, &e.Author, &e.Detail, &e.URL,
			&unread, &actionable, &isMine); err != nil {
			return nil, err
		}
		e.TS = time.Unix(ts, 0)
		e.Kind = timeline.Kind(kind)
		e.Unread = unread == 1
		e.Actionable = actionable == 1
		e.IsMine = isMine == 1
		out = append(out, e)
	}
	return out, rows.Err()
}

// UnreadCount returns how many events are effectively unread (not locally read
// and still unread on GitHub) — used for the menu-bar badge.
func (s *Store) UnreadCount() (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM events WHERE read_at IS NULL AND github_unread=1`).Scan(&n)
	return n, err
}

// Get returns a single event by its local seq.
func (s *Store) Get(seq int64) (timeline.Event, error) {
	return s.scanOne(`
SELECT seq, id, thread_id, source, ts, kind, repo, number, author, detail, url,
       (read_at IS NULL AND github_unread=1), actionable, is_mine
FROM events WHERE seq=?`, seq)
}

// GetByID returns a single event by its dedupe id (with its assigned seq).
func (s *Store) GetByID(id string) (timeline.Event, error) {
	return s.scanOne(`
SELECT seq, id, thread_id, source, ts, kind, repo, number, author, detail, url,
       (read_at IS NULL AND github_unread=1), actionable, is_mine
FROM events WHERE id=?`, id)
}

func (s *Store) scanOne(query string, arg any) (timeline.Event, error) {
	var e timeline.Event
	var ts int64
	var kind, unread, actionable, isMine int
	err := s.db.QueryRow(query, arg).Scan(&e.Seq, &e.ID, &e.ThreadID, &e.Source, &ts,
		&kind, &e.Repo, &e.Number, &e.Author, &e.Detail, &e.URL,
		&unread, &actionable, &isMine)
	if err != nil {
		return e, err
	}
	e.TS = time.Unix(ts, 0)
	e.Kind = timeline.Kind(kind)
	e.Unread = unread == 1
	e.Actionable = actionable == 1
	e.IsMine = isMine == 1
	return e, nil
}

// ExistingIDs returns the subset of ids already present in the store.
func (s *Store) ExistingIDs(ids []string) (map[string]bool, error) {
	found := make(map[string]bool, len(ids))
	if len(ids) == 0 {
		return found, nil
	}
	placeholders := make([]byte, 0, len(ids)*2)
	args := make([]any, len(ids))
	for i, id := range ids {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '?')
		args[i] = id
	}
	rows, err := s.db.Query(`SELECT id FROM events WHERE id IN (`+string(placeholders)+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		found[id] = true
	}
	return found, rows.Err()
}

// MarkRead records a local read timestamp for an event (idempotent).
func (s *Store) MarkRead(seq int64) error {
	_, err := s.db.Exec(`UPDATE events SET read_at=? WHERE seq=? AND read_at IS NULL`,
		time.Now().Unix(), seq)
	return err
}

// ReconcileNotifications self-heals GitHub-side reads: any notification-sourced
// event no longer in the current unread set is marked read on GitHub's side.
func (s *Store) ReconcileNotifications(activeThreadIDs map[string]bool) error {
	rows, err := s.db.Query(
		`SELECT seq, thread_id FROM events WHERE source='notification' AND github_unread=1`)
	if err != nil {
		return err
	}
	var stale []int64
	for rows.Next() {
		var seq int64
		var tid string
		if err := rows.Scan(&seq, &tid); err != nil {
			rows.Close()
			return err
		}
		if !activeThreadIDs[tid] {
			stale = append(stale, seq)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, seq := range stale {
		if _, err := s.db.Exec(`UPDATE events SET github_unread=0 WHERE seq=?`, seq); err != nil {
			return err
		}
	}
	return nil
}

// Prune removes read/resolved events older than maxAge, never touching items
// that are still unread.
func (s *Store) Prune(maxAge time.Duration) error {
	cutoff := time.Now().Add(-maxAge).Unix()
	_, err := s.db.Exec(
		`DELETE FROM events WHERE last_seen < ? AND (read_at IS NOT NULL OR github_unread=0)`,
		cutoff)
	return err
}

// BackfillDetails rewrites stored rows whose detail is one of the given raw
// tokens (keys) to its mapped phrase (value) in a single UPDATE. It's a one-off
// cleanup for events saved before the classifier stopped echoing GitHub's raw
// notification reason; idempotent, so re-running it touches nothing.
func (s *Store) BackfillDetails(m map[string]string) error {
	if len(m) == 0 {
		return nil
	}
	var q strings.Builder
	q.WriteString("UPDATE events SET detail = CASE detail")
	whenArgs := make([]any, 0, len(m)*2)
	inArgs := make([]any, 0, len(m))
	ph := make([]string, 0, len(m))
	for from, to := range m {
		q.WriteString(" WHEN ? THEN ?")
		whenArgs = append(whenArgs, from, to)
		inArgs = append(inArgs, from)
		ph = append(ph, "?")
	}
	q.WriteString(" END WHERE detail IN (" + strings.Join(ph, ",") + ")")
	_, err := s.db.Exec(q.String(), append(whenArgs, inArgs...)...)
	return err
}

// ReviewState is a per-PR verdict for reconciling review-request items: whether
// the viewer's latest review covers the PR's current head commit.
type ReviewState struct {
	Repo   string
	Number int
	AtHead bool
}

// ReconcileReviewRequests drives the read-state of review-request items from
// whether the viewer has actually reviewed each PR's current head. A request
// whose review is up to date is auto-resolved (marked read, so it drops from the
// Inbox); one whose review has gone stale — new commits pushed on top of it — is
// re-surfaced (marked unread again). Only KindReviewRequested rows are touched;
// PRs the viewer hasn't reviewed produce no verdict and are left as they are.
func (s *Store) ReconcileReviewRequests(states []ReviewState) error {
	for _, rs := range states {
		var err error
		if rs.AtHead {
			_, err = s.db.Exec(
				`UPDATE events SET read_at=? WHERE kind=? AND repo=? AND number=? AND read_at IS NULL`,
				time.Now().Unix(), int(timeline.KindReviewRequested), rs.Repo, rs.Number)
		} else {
			_, err = s.db.Exec(
				`UPDATE events SET read_at=NULL, github_unread=1 WHERE kind=? AND repo=? AND number=?`,
				int(timeline.KindReviewRequested), rs.Repo, rs.Number)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// GetPRState returns the stored state for a PR and whether a row existed.
func (s *Store) GetPRState(key string) (PRState, bool, error) {
	st := PRState{Key: key}
	err := s.db.QueryRow(
		`SELECT head_sha, ci_state, merge_state FROM pr_state WHERE pr_key=?`, key).
		Scan(&st.HeadSHA, &st.CIState, &st.MergeState)
	if err == sql.ErrNoRows {
		return st, false, nil
	}
	if err != nil {
		return st, false, err
	}
	return st, true, nil
}

// SetPRState upserts the last-seen state for a PR.
func (s *Store) SetPRState(st PRState) error {
	_, err := s.db.Exec(`
INSERT INTO pr_state (pr_key, head_sha, ci_state, merge_state, updated_at)
VALUES (?,?,?,?,?)
ON CONFLICT(pr_key) DO UPDATE SET
  head_sha=excluded.head_sha, ci_state=excluded.ci_state,
  merge_state=excluded.merge_state, updated_at=excluded.updated_at`,
		st.Key, st.HeadSHA, st.CIState, st.MergeState, time.Now().Unix())
	return err
}

// SetOpenPR upserts one PR into the roster, stamping last_seen so a later
// ReconcileOpenPRs can drop rows for PRs that have since closed.
func (s *Store) SetOpenPR(pr OpenPR) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(`
INSERT INTO open_prs
  (pr_key, repo, number, title, url, author, is_draft, ci_state, merge_state, updated_at, last_seen)
VALUES (?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(pr_key) DO UPDATE SET
  title=excluded.title, url=excluded.url, author=excluded.author,
  is_draft=excluded.is_draft, ci_state=excluded.ci_state,
  merge_state=excluded.merge_state, updated_at=excluded.updated_at,
  last_seen=excluded.last_seen`,
		pr.Key, pr.Repo, pr.Number, pr.Title, pr.URL, pr.Author, boolToInt(pr.IsDraft),
		pr.CIState, pr.MergeState, pr.UpdatedAt.Unix(), now)
	return err
}

// ReconcileOpenPRs drops roster rows whose key isn't in the current poll — i.e.
// PRs that have merged or closed since we last saw them.
func (s *Store) ReconcileOpenPRs(activeKeys map[string]bool) error {
	rows, err := s.db.Query(`SELECT pr_key FROM open_prs`)
	if err != nil {
		return err
	}
	var stale []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return err
		}
		if !activeKeys[k] {
			stale = append(stale, k)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, k := range stale {
		if _, err := s.db.Exec(`DELETE FROM open_prs WHERE pr_key=?`, k); err != nil {
			return err
		}
	}
	return nil
}

// OpenPRs returns the current roster, most recently updated first.
func (s *Store) OpenPRs() ([]OpenPR, error) {
	rows, err := s.db.Query(`
SELECT pr_key, repo, number, title, url, author, is_draft, ci_state, merge_state, updated_at
FROM open_prs ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []OpenPR
	for rows.Next() {
		var pr OpenPR
		var draft int
		var updated int64
		if err := rows.Scan(&pr.Key, &pr.Repo, &pr.Number, &pr.Title, &pr.URL,
			&pr.Author, &draft, &pr.CIState, &pr.MergeState, &updated); err != nil {
			return nil, err
		}
		pr.IsDraft = draft == 1
		pr.UpdatedAt = time.Unix(updated, 0)
		out = append(out, pr)
	}
	return out, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
