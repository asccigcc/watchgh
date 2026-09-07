package store

import "time"

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
