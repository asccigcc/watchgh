// Package store persists timeline events in SQLite so they dedupe across runs,
// remember read/unread state, and can be pruned by age.
package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"time"

	"watchgh/internal/timeline"

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

// DefaultPath is the per-user database location (~/.config/watchgh/watchgh.db).
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "watchgh", "watchgh.db"), nil
}

// Open opens (creating parent dirs and schema as needed) the store at path.
func Open(path string) (*Store, error) {
	migrateLegacyDir(filepath.Dir(path))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// WAL keeps reads snappy alongside the background poll writes; busy_timeout
	// rides out brief lock contention.
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

// migrateLegacyDir carries a pre-rename data directory (watchgit) over to the
// current one (watchgh) the first time we run under the new name, so the store —
// read state, seq numbers, roster — and config.toml survive the rename instead
// of starting fresh. Best-effort: any failure just means a clean start. Safe to
// drop once no watchgit installs remain in the wild.
func migrateLegacyDir(newDir string) {
	if _, err := os.Stat(newDir); err == nil {
		return // already on the new path
	}
	legacy := filepath.Join(filepath.Dir(newDir), "watchgit")
	if _, err := os.Stat(legacy); err != nil {
		return // nothing to migrate
	}
	if err := os.Rename(legacy, newDir); err != nil {
		return
	}
	// config.toml keeps its name; only the DB files carry the old base name.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Rename(
			filepath.Join(newDir, "watchgit.db"+suffix),
			filepath.Join(newDir, "watchgh.db"+suffix))
	}
}

// upsertEventSQL inserts a new event or refreshes the mutable fields of an
// existing one (matched by id), preserving seq, first_seen, and any local
// read_at.
const upsertEventSQL = `
INSERT INTO events
  (id, thread_id, source, ts, kind, repo, number, author, detail, url,
   github_unread, actionable, is_mine, first_seen, last_seen)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET
  ts=excluded.ts, detail=excluded.detail, github_unread=excluded.github_unread,
  actionable=excluded.actionable, last_seen=excluded.last_seen`

// execer is the write surface shared by *sql.DB and *sql.Tx, so one upsert body
// serves both the single and batched paths.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// upsertEvent runs upsertEventSQL against ex (a DB or an open transaction).
func upsertEvent(ex execer, e timeline.Event) error {
	now := time.Now().Unix()
	_, err := ex.Exec(upsertEventSQL,
		e.ID, e.ThreadID, e.Source, e.TS.Unix(), int(e.Kind), e.Repo, e.Number,
		e.Author, e.Detail, e.URL, boolToInt(e.Unread), boolToInt(e.Actionable),
		boolToInt(e.IsMine), now, now)
	return err
}

// Upsert inserts or refreshes a single event.
func (s *Store) Upsert(e timeline.Event) error { return upsertEvent(s.db, e) }

// UpsertAll upserts a batch in one transaction.
func (s *Store) UpsertAll(events []timeline.Event) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, e := range events {
		if err := upsertEvent(tx, e); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// eventColumns is the SELECT list every event read shares. The unread flag is
// computed: effective unread = not locally read AND still unread on GitHub.
const eventColumns = `seq, id, thread_id, source, ts, kind, repo, number, author, detail, url,
       (read_at IS NULL AND github_unread=1), actionable, is_mine`

// rowScanner is the Scan surface shared by *sql.Row and *sql.Rows, so one
// scanEvent body serves both single-row and iterated reads.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanEvent reads one events row (selected via eventColumns) into an Event.
func scanEvent(sc rowScanner) (timeline.Event, error) {
	var e timeline.Event
	var ts int64
	var kind, unread, actionable, isMine int
	if err := sc.Scan(&e.Seq, &e.ID, &e.ThreadID, &e.Source, &ts, &kind,
		&e.Repo, &e.Number, &e.Author, &e.Detail, &e.URL,
		&unread, &actionable, &isMine); err != nil {
		return e, err
	}
	e.TS = time.Unix(ts, 0)
	e.Kind = timeline.Kind(kind)
	e.Unread = unread == 1
	e.Actionable = actionable == 1
	e.IsMine = isMine == 1
	return e, nil
}

// List returns all stored events oldest-first.
func (s *Store) List() ([]timeline.Event, error) {
	rows, err := s.db.Query(`SELECT ` + eventColumns + ` FROM events ORDER BY ts ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []timeline.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Get returns a single event by its local seq.
func (s *Store) Get(seq int64) (timeline.Event, error) {
	return scanEvent(s.db.QueryRow(`SELECT `+eventColumns+` FROM events WHERE seq=?`, seq))
}

// GetByID returns a single event by its dedupe id (with its assigned seq).
func (s *Store) GetByID(id string) (timeline.Event, error) {
	return scanEvent(s.db.QueryRow(`SELECT `+eventColumns+` FROM events WHERE id=?`, id))
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
// event whose thread is no longer in the current unread set is marked read on
// GitHub's side. It runs as a single set-based UPDATE — with no active threads,
// every such event is stale, so the NOT IN filter drops away entirely.
func (s *Store) ReconcileNotifications(activeThreadIDs map[string]bool) error {
	const base = `UPDATE events SET github_unread=0 WHERE source='notification' AND github_unread=1`
	if len(activeThreadIDs) == 0 {
		_, err := s.db.Exec(base)
		return err
	}
	ph, args := keyArgs(activeThreadIDs)
	_, err := s.db.Exec(base+` AND thread_id NOT IN (`+ph+`)`, args...)
	return err
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
// PRs that have merged or closed since we last saw them. One set-based DELETE;
// an empty active set means the viewer has no open PRs, so the roster is cleared.
func (s *Store) ReconcileOpenPRs(activeKeys map[string]bool) error {
	if len(activeKeys) == 0 {
		_, err := s.db.Exec(`DELETE FROM open_prs`)
		return err
	}
	ph, args := keyArgs(activeKeys)
	_, err := s.db.Exec(`DELETE FROM open_prs WHERE pr_key NOT IN (`+ph+`)`, args...)
	return err
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

// keyArgs renders a "?,?,…" placeholder list and the matching args for a set of
// string keys, for building a parameterized IN/NOT IN clause. Caller guarantees
// the set is non-empty (an empty IN () is invalid SQL).
func keyArgs(set map[string]bool) (placeholders string, args []any) {
	args = make([]any, 0, len(set))
	var b strings.Builder
	for k := range set {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('?')
		args = append(args, k)
	}
	return b.String(), args
}
