package store

import (
	"context"
	"time"
)

// NotifyCounts returns the last per-category backlog counts a notification pass
// recorded, keyed by category id. A category never yet recorded is simply absent
// (and reads as zero), so the first pass compares against an empty baseline.
func (s *Store) NotifyCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT category, count FROM notify_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]int)
	for rows.Next() {
		var cat string
		var n int
		if err := rows.Scan(&cat, &n); err != nil {
			return nil, err
		}
		out[cat] = n
	}
	return out, rows.Err()
}

// SetNotifyCounts records the current per-category backlog as the new baseline in
// one transaction, so the next pass only alerts on categories whose count grew.
// Counts are stored verbatim (zeros included) so a category that has since
// cleared lowers its baseline and can re-alert if it climbs again.
func (s *Store) SetNotifyCounts(ctx context.Context, counts map[string]int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	for cat, n := range counts {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO notify_state (category, count, updated_at) VALUES (?,?,?)
ON CONFLICT(category) DO UPDATE SET count=excluded.count, updated_at=excluded.updated_at`,
			cat, n, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}
