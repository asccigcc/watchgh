package store

import (
	"context"
	"database/sql"
	"time"
)

// PRState is the last-seen CI/merge status for a tracked PR; the diff engine
// compares against it to emit events only on transitions.
type PRState struct {
	Key        string
	HeadSHA    string
	CIState    string
	MergeState string
}

// GetPRState returns the stored state for a PR and whether a row existed.
func (s *Store) GetPRState(ctx context.Context, key string) (PRState, bool, error) {
	st := PRState{Key: key}
	err := s.db.QueryRowContext(ctx,
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
func (s *Store) SetPRState(ctx context.Context, st PRState) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO pr_state (pr_key, head_sha, ci_state, merge_state, updated_at)
VALUES (?,?,?,?,?)
ON CONFLICT(pr_key) DO UPDATE SET
  head_sha=excluded.head_sha, ci_state=excluded.ci_state,
  merge_state=excluded.merge_state, updated_at=excluded.updated_at`,
		st.Key, st.HeadSHA, st.CIState, st.MergeState, time.Now().Unix())
	return err
}
