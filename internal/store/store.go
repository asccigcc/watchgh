// Package store persists timeline events in SQLite so they dedupe across runs,
// remember read/unread state, and can be pruned by age. Events live in events.go;
// the tracked-PR diff state and open-PR roster live in prstate.go and openprs.go.
package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"

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
