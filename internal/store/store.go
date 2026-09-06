// Package store persists timeline events in SQLite so they dedupe across runs,
// remember read/unread state, and can be pruned by age.
package store

import (
	"database/sql"
	"os"
	"path/filepath"
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
`

// PRState is the last-seen CI/merge status for a tracked PR; the diff engine
// compares against it to emit events only on transitions.
type PRState struct {
	Key        string
	HeadSHA    string
	CIState    string
	MergeState string
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

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
