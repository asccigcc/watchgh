package store

import (
	"database/sql"
	"strings"
	"time"

	"watchgh/internal/timeline"
)

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
