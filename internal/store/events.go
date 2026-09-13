package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"watchgh/internal/timeline"
)

// upsertEventSQL inserts a new event or refreshes the mutable fields of an
// existing one (matched by id), preserving seq and first_seen. A local read is
// preserved across refreshes that carry no new activity; genuinely new activity
// (a later ts than the stored row) clears read_at so the item re-surfaces as
// unread — the notification event id is now the stable thread id, so this is how
// a re-read thread comes back, in place, without minting a duplicate row.
const upsertEventSQL = `
INSERT INTO events
  (id, thread_id, source, ts, kind, repo, number, author, detail, url,
   github_unread, actionable, is_mine, first_seen, last_seen)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET
  read_at=CASE WHEN excluded.ts > events.ts THEN NULL ELSE events.read_at END,
  kind=excluded.kind, ts=excluded.ts, detail=excluded.detail,
  github_unread=excluded.github_unread, actionable=excluded.actionable,
  last_seen=excluded.last_seen`

// execer is the write surface shared by *sql.DB and *sql.Tx, so one upsert body
// serves both the single and batched paths.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// upsertEvent runs upsertEventSQL against ex (a DB or an open transaction).
func upsertEvent(ctx context.Context, ex execer, e timeline.Event) error {
	now := time.Now().Unix()
	_, err := ex.ExecContext(ctx, upsertEventSQL,
		e.ID, e.ThreadID, e.Source, e.TS.Unix(), int(e.Kind), e.Repo, e.Number,
		e.Author, e.Detail, e.URL, boolToInt(e.Unread), boolToInt(e.Actionable),
		boolToInt(e.IsMine), now, now)
	return err
}

// Upsert inserts or refreshes a single event.
func (s *Store) Upsert(ctx context.Context, e timeline.Event) error {
	return upsertEvent(ctx, s.db, e)
}

// UpsertAll upserts a batch in one transaction.
func (s *Store) UpsertAll(ctx context.Context, events []timeline.Event) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, e := range events {
		if err := upsertEvent(ctx, tx, e); err != nil {
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
func (s *Store) List(ctx context.Context) ([]timeline.Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+eventColumns+` FROM events ORDER BY ts ASC`)
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
func (s *Store) Get(ctx context.Context, seq int64) (timeline.Event, error) {
	return scanEvent(s.db.QueryRowContext(ctx, `SELECT `+eventColumns+` FROM events WHERE seq=?`, seq))
}

// GetByID returns a single event by its dedupe id (with its assigned seq).
func (s *Store) GetByID(ctx context.Context, id string) (timeline.Event, error) {
	return scanEvent(s.db.QueryRowContext(ctx, `SELECT `+eventColumns+` FROM events WHERE id=?`, id))
}

// ExistingIDs returns the subset of ids already present in the store.
func (s *Store) ExistingIDs(ctx context.Context, ids []string) (map[string]bool, error) {
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
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM events WHERE id IN (`+string(placeholders)+`)`, args...)
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
func (s *Store) MarkRead(ctx context.Context, seq int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE events SET read_at=? WHERE seq=? AND read_at IS NULL`,
		time.Now().Unix(), seq)
	return err
}

// ReconcileNotifications self-heals GitHub-side reads: any notification-sourced
// event whose thread is no longer in the current unread set is marked read on
// GitHub's side. It runs as a single set-based UPDATE — with no active threads,
// every such event is stale, so the NOT IN filter drops away entirely.
func (s *Store) ReconcileNotifications(ctx context.Context, activeThreadIDs map[string]bool) error {
	const base = `UPDATE events SET github_unread=0 WHERE source='notification' AND github_unread=1`
	if len(activeThreadIDs) == 0 {
		_, err := s.db.ExecContext(ctx, base)
		return err
	}
	ph, args := keyArgs(activeThreadIDs)
	_, err := s.db.ExecContext(ctx, base+` AND thread_id NOT IN (`+ph+`)`, args...)
	return err
}

// PRRef identifies a pull request the store holds rows for, so its lifecycle
// state can be looked up and its rows reaped once it merges or closes.
type PRRef struct {
	Repo   string // owner/name
	Number int
}

// DistinctPRs returns every (repo, number) pair the events table references, so
// a poll can look up their states and reap the ones that have merged or closed.
// Rows without a PR number (number 0) carry no PR to check and are skipped.
func (s *Store) DistinctPRs(ctx context.Context) ([]PRRef, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT repo, number FROM events WHERE number > 0 AND repo <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PRRef
	for rows.Next() {
		var r PRRef
		if err := rows.Scan(&r.Repo, &r.Number); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReapPRs deletes every event for the given PRs — the store's way of dropping a
// merged or closed PR from all tabs at once, since a dead PR is no longer
// actionable anywhere. Idempotent: PRs with no remaining rows delete nothing.
func (s *Store) ReapPRs(ctx context.Context, refs []PRRef) error {
	if len(refs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, r := range refs {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM events WHERE repo=? AND number=?`, r.Repo, r.Number); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Prune removes read/resolved events older than maxAge, never touching items
// that are still unread.
func (s *Store) Prune(ctx context.Context, maxAge time.Duration) error {
	cutoff := time.Now().Add(-maxAge).Unix()
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM events WHERE last_seen < ? AND (read_at IS NOT NULL OR github_unread=0)`,
		cutoff)
	return err
}

// BackfillDetails rewrites stored rows whose detail is one of the given raw
// tokens (keys) to its mapped phrase (value) in a single UPDATE. It's a one-off
// cleanup for events saved before the classifier stopped echoing GitHub's raw
// notification reason; idempotent, so re-running it touches nothing.
func (s *Store) BackfillDetails(ctx context.Context, m map[string]string) error {
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
	_, err := s.db.ExecContext(ctx, q.String(), append(whenArgs, inArgs...)...)
	return err
}

// CollapseNotificationThreads is a one-off migration that removes the duplicate
// rows left by the old notification id scheme (thread id + "@" + updated_at),
// which minted a fresh row on every thread update so a single PR piled up as
// many unread rows across the tabs. It keeps, per thread, only the latest
// version (max ts, then max seq) and rewrites its id to the bare thread id so
// the stable-id upsert path merges future polls onto that one row. Idempotent:
// once each survivor's id equals its thread id there is nothing left to collapse.
func (s *Store) CollapseNotificationThreads(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Drop any notification row a same-thread sibling supersedes by (ts, seq);
	// the survivor is the one with no strictly-greater sibling.
	if _, err := tx.ExecContext(ctx, `
DELETE FROM events
WHERE source='notification'
  AND EXISTS (
    SELECT 1 FROM events sib
    WHERE sib.source='notification' AND sib.thread_id=events.thread_id
      AND (sib.ts > events.ts OR (sib.ts = events.ts AND sib.seq > events.seq))
  )`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE events SET id=thread_id WHERE source='notification' AND id<>thread_id`); err != nil {
		return err
	}
	return tx.Commit()
}

// graphqlLane maps a tracked-PR event kind to the channel its stable id encodes.
// It mirrors the channel argument tracker.base emits, so the migration below can
// reconstruct each legacy row's target id from its kind.
func graphqlLane(k timeline.Kind) string {
	switch k {
	case timeline.KindCIPassed, timeline.KindCIFailed:
		return "ci"
	case timeline.KindBlocked, timeline.KindUnblocked:
		return "merge"
	default:
		return "other"
	}
}

// CollapseGraphqlLanes is the tracked-PR analogue of CollapseNotificationThreads.
// The old tracker id embedded the detection nanosecond, so every CI/merge
// transition minted a fresh row — and since those rows are unread, Prune never
// reclaimed them, so a PR's status history piled up unbounded. This folds each
// PR's graphql rows to one per lane (its latest ci row and latest merge row) and
// rewrites the survivor's id to the stable "repo#num:lane" the tracker now emits,
// so future transitions upsert onto it. Idempotent.
func (s *Store) CollapseGraphqlLanes(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, repo, number, kind, ts FROM events WHERE source='graphql'`)
	if err != nil {
		return err
	}
	type winner struct{ seq, ts int64 }
	best := map[string]winner{} // stable id -> latest row so far
	for rows.Next() {
		var seq, ts int64
		var repo string
		var number, kind int
		if err := rows.Scan(&seq, &repo, &number, &kind, &ts); err != nil {
			rows.Close()
			return err
		}
		id := fmt.Sprintf("%s#%d:%s", repo, number, graphqlLane(timeline.Kind(kind)))
		if w, ok := best[id]; !ok || ts > w.ts || (ts == w.ts && seq > w.seq) {
			best[id] = winner{seq: seq, ts: ts}
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(best) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Drop every graphql row that isn't a lane winner, then rename the winners to
	// the stable id. Deleting first guarantees each target id ends up unique.
	keep := make(map[int64]string, len(best))
	ph := make([]string, 0, len(best))
	args := make([]any, 0, len(best))
	for id, w := range best {
		keep[w.seq] = id
		ph = append(ph, "?")
		args = append(args, w.seq)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM events WHERE source='graphql' AND seq NOT IN (`+strings.Join(ph, ",")+`)`,
		args...); err != nil {
		return err
	}
	for seq, id := range keep {
		if _, err := tx.ExecContext(ctx, `UPDATE events SET id=? WHERE seq=?`, id, seq); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ReviewState is a per-PR verdict for reconciling review-request items: whether
// the viewer's latest review covers the PR's current head commit.
type ReviewState struct {
	Repo   string
	Number int
	AtHead bool
	State  string // viewer's latest review verdict: APPROVED, CHANGES_REQUESTED, COMMENTED, …
}

// reviewVerdict maps the viewer's latest review state to the row's kind and
// detail, so a handled review request shows what you actually did rather than
// staying frozen at "review requested". Anything GitHub reports as a review but
// isn't an explicit approve/changes-request (COMMENTED, DISMISSED) reads as a
// plain comment.
func reviewVerdict(state string) (timeline.Kind, string) {
	switch state {
	case "APPROVED":
		return timeline.KindApproved, "approved"
	case "CHANGES_REQUESTED":
		return timeline.KindChangesRequested, "changes requested"
	default:
		return timeline.KindCommented, "commented"
	}
}

// reviewKinds is the SQL IN-list of notification kinds ReconcileReviewRequests
// owns: an open review request and the approve/changes verdicts it relabels a
// handled request into. Scoping the match to these leaves genuine comment,
// assignment, and mention notifications on the same PR untouched, while still
// letting a later poll re-find a row it already relabeled so it can re-surface
// it when new commits push past the review. A "commented" verdict is terminal —
// it is not re-found — which is fine: a genuine re-request arrives as a fresh
// notification that re-opens the row through the normal ingest path.
var reviewKinds = fmt.Sprintf("%d,%d,%d",
	int(timeline.KindReviewRequested), int(timeline.KindChangesRequested), int(timeline.KindApproved))

// ReconcileReviewRequests reflects the viewer's actual verdict onto each review
// request. When the viewer's latest review covers the PR's current head, the row
// is relabeled to what they did — approved / changes requested / commented — and
// treated as handled (marked read, so it drops from the Inbox to the Read tab).
// When new commits have landed on top of that review, the row reverts to an open
// "review requested" and re-surfaces as unread. PRs the viewer hasn't reviewed
// produce no verdict and are left as they are.
func (s *Store) ReconcileReviewRequests(ctx context.Context, states []ReviewState) error {
	now := time.Now().Unix()
	for _, rs := range states {
		var err error
		if rs.AtHead {
			kind, detail := reviewVerdict(rs.State)
			_, err = s.db.ExecContext(ctx,
				`UPDATE events SET kind=?, detail=?, read_at=COALESCE(read_at, ?)
				 WHERE source='notification' AND repo=? AND number=? AND kind IN (`+reviewKinds+`)`,
				int(kind), detail, now, rs.Repo, rs.Number)
		} else {
			_, err = s.db.ExecContext(ctx,
				`UPDATE events SET kind=?, detail='review requested', read_at=NULL, github_unread=1
				 WHERE source='notification' AND repo=? AND number=? AND kind IN (`+reviewKinds+`)`,
				int(timeline.KindReviewRequested), rs.Repo, rs.Number)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
