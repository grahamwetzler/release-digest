// Package store persists releases, summaries, HTTP cache validators, alert
// state and the rendered feeds in SQLite (PLAN.md §3.5, §8.1).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure Go: no CGO, easy cross-compiles
)

// Store is safe for concurrent use. There is one writer (the scheduler or
// build); HTTP handlers only read.
type Store struct {
	db *sql.DB
	ops
}

// Tx is a transaction. It has the same row methods as Store.
type Tx struct {
	tx *sql.Tx
	ops
}

// querier is what *sql.DB and *sql.Tx have in common.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ops holds the row methods shared by Store and Tx.
type ops struct{ q querier }

// Tx runs fn in a transaction, committing only if it returns nil.
func (s *Store) Tx(ctx context.Context, fn func(*Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(&Tx{tx: tx, ops: ops{tx}}); err != nil {
		return err
	}
	return tx.Commit()
}

// migrations are applied in order; schema_version records how many have run.
// Times are Unix milliseconds.
var migrations = []string{
	`CREATE TABLE releases (
		repo                TEXT NOT NULL,              -- owner/repo, lowercased
		tag                 TEXT NOT NULL,
		guid                TEXT NOT NULL UNIQUE,       -- UUIDv5 (§5)
		kind                TEXT NOT NULL,              -- release | tag
		name                TEXT NOT NULL DEFAULT '',
		html_url            TEXT NOT NULL,
		stream              TEXT NOT NULL DEFAULT '',   -- tag prefix (§3.1.1)
		version             TEXT NOT NULL DEFAULT '',   -- normalized SemVer, '' if date-ordered
		prev_tag            TEXT NOT NULL DEFAULT '',   -- §3.2.1
		prerelease          INTEGER NOT NULL DEFAULT 0,
		published_at        INTEGER,                    -- frozen once set (tags: after the metadata queue)
		first_seen_at       INTEGER NOT NULL,
		state               TEXT NOT NULL,              -- skipped | pending | fallback | summarized | no_notes (§3.3)
		notes_source        TEXT NOT NULL DEFAULT '',   -- release_body | changelog | compare
		notes_hash          TEXT NOT NULL DEFAULT '',
		summary_key         TEXT,                       -- → summaries.key
		attempts            INTEGER NOT NULL DEFAULT 0,
		next_attempt_at     INTEGER NOT NULL,           -- = first_seen_at on insert (§3.3)
		last_error          TEXT NOT NULL DEFAULT '',
		origin              TEXT NOT NULL,              -- live | backfill | catchup | reconcile (§3.1)
		urgency             TEXT NOT NULL DEFAULT 'none', -- none | advisory | urgent (§3.4)
		urgency_source      TEXT NOT NULL DEFAULT '',   -- advisory:GHSA-… | llm
		content_updated_at  INTEGER,                    -- Atom <updated> (§5)
		last_verified_at    INTEGER,                    -- last successful existence check (§3.1)
		verify_attempted_at INTEGER,                    -- last attempt, any outcome; sweep order
		missing_since       INTEGER,
		PRIMARY KEY (repo, tag)
	);
	CREATE INDEX releases_feed ON releases (state, published_at DESC);
	CREATE INDEX releases_work ON releases (state, next_attempt_at, first_seen_at);
	CREATE INDEX releases_stream ON releases (repo, stream);

	CREATE TABLE notes (hash TEXT PRIMARY KEY, text TEXT NOT NULL);

	CREATE TABLE summaries (
		key        TEXT PRIMARY KEY,                    -- sha256 of the canonical request (§3.3)
		model      TEXT NOT NULL,
		json       TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		tokens_in  INTEGER,
		tokens_out INTEGER
	);

	CREATE TABLE http_cache (url TEXT PRIMARY KEY, etag TEXT, body BLOB, fetched_at INTEGER);

	CREATE TABLE repo_state (
		repo                      TEXT PRIMARY KEY,
		initialized_at            INTEGER,
		last_success_at           INTEGER,
		held                      INTEGER NOT NULL DEFAULT 0,
		visibility_confirmed_at   INTEGER,
		visibility_confirmed_tick INTEGER,              -- tick ID of the last successful public check (§3)
		scan_boundary             TEXT,                 -- newest tag of the last completed scan (§3.1)
		reconcile_next_page       INTEGER,
		last_reconciled_at        INTEGER,
		last_error                TEXT NOT NULL DEFAULT ''
	);

	CREATE TABLE scheduler_state (key TEXT PRIMARY KEY, value TEXT NOT NULL);

	CREATE TABLE rendered (
		path        TEXT PRIMARY KEY,                   -- feed.xml | feed.atom
		bytes       BLOB NOT NULL,
		etag        TEXT NOT NULL,
		modified_at INTEGER NOT NULL
	);

	CREATE TABLE advisories (
		id            TEXT PRIMARY KEY,
		found         INTEGER NOT NULL,
		severity      TEXT,
		withdrawn_at  INTEGER,
		json          TEXT,
		fetched_at    INTEGER NOT NULL,
		next_fetch_at INTEGER NOT NULL
	);

	CREATE TABLE release_advisories (
		repo        TEXT NOT NULL,
		tag         TEXT NOT NULL,
		advisory_id TEXT NOT NULL,
		matched     INTEGER NOT NULL DEFAULT 0,         -- re-evaluated on each recompute (§3.4)
		fixes       INTEGER NOT NULL DEFAULT 0,         -- this release fixes it (§3.4)
		PRIMARY KEY (repo, tag, advisory_id)
	);

	CREATE TABLE notifications (                        -- the alert outbox (§3.4)
		guid             TEXT PRIMARY KEY,              -- releases.guid: enqueued once
		channel          TEXT NOT NULL,                 -- ntfy
		reason           TEXT NOT NULL,                 -- advisory:GHSA-… | llm
		payload          TEXT NOT NULL,
		state            TEXT NOT NULL,                 -- queued | claimed | sending | sent | expired | cancelled
		cancel_requested INTEGER NOT NULL DEFAULT 0,
		attempts         INTEGER NOT NULL DEFAULT 0,
		next_attempt_at  INTEGER,
		created_at       INTEGER NOT NULL,
		sent_at          INTEGER
	);`,
	// 2: a tag's ref target, for the metadata queue that resolves its date,
	// and the queue's last attempt, so failing items rotate to the back (§3.1).
	`ALTER TABLE releases ADD COLUMN ref_sha TEXT NOT NULL DEFAULT '';  -- tags: what refs/tags/<tag> points at
	ALTER TABLE releases ADD COLUMN ref_type TEXT NOT NULL DEFAULT ''; -- commit | tag (annotated)
	ALTER TABLE releases ADD COLUMN meta_attempted_at INTEGER;
	ALTER TABLE repo_state ADD COLUMN scan_mode TEXT NOT NULL DEFAULT '';  -- release | tag: the list last scanned
	CREATE INDEX releases_undated ON releases (meta_attempted_at) WHERE published_at IS NULL;`,
	// 3: notes resolution (§3.2). body_hash keeps the release body apart from
	// the notes chosen for it; unresolved rows queue for resolution, and every
	// existing row is re-resolved once.
	`ALTER TABLE releases ADD COLUMN body_hash TEXT NOT NULL DEFAULT '';        -- → notes.hash: the release body as published
	ALTER TABLE releases ADD COLUMN notes_resolved INTEGER NOT NULL DEFAULT 0; -- 1 once §3.2 chose notes_source (or none)
	ALTER TABLE releases ADD COLUMN resolve_attempted_at INTEGER;
	UPDATE releases SET body_hash = notes_hash WHERE notes_source = 'release_body';
	CREATE INDEX releases_unresolved ON releases (resolve_attempted_at) WHERE notes_resolved = 0;`,
	// 4: security alerts (§3.4). advisories_hash is the notes_hash last
	// scanned for advisory IDs; urgency_dirty marks a release whose urgency
	// inputs changed since it was last decided. Outbox rows carry their
	// release and alert window.
	`ALTER TABLE releases ADD COLUMN advisories_hash TEXT NOT NULL DEFAULT '';
	ALTER TABLE releases ADD COLUMN urgency_dirty INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE advisories ADD COLUMN errors INTEGER NOT NULL DEFAULT 0;     -- consecutive failed lookups
	ALTER TABLE notifications ADD COLUMN repo TEXT NOT NULL DEFAULT '';
	ALTER TABLE notifications ADD COLUMN tag TEXT NOT NULL DEFAULT '';
	ALTER TABLE notifications ADD COLUMN expires_at INTEGER NOT NULL DEFAULT 0; -- published_at + notify.max_age
	ALTER TABLE notifications ADD COLUMN last_error TEXT NOT NULL DEFAULT '';
	ALTER TABLE notifications ADD COLUMN acked_at INTEGER;                       -- an expired alert, acknowledged
	CREATE INDEX release_advisories_by_id ON release_advisories (advisory_id);
	CREATE INDEX releases_urgency_dirty ON releases (repo) WHERE urgency_dirty = 1;
	CREATE INDEX notifications_due ON notifications (state, next_attempt_at);`,
	// 5: resummarize (§3.3) records when a fresh summary was asked for; a
	// summary created after that satisfies it, and clears it.
	`ALTER TABLE releases ADD COLUMN resummarize INTEGER NOT NULL DEFAULT 0; -- request time, 0 for none`,
}

// Open opens (creating if needed) the database at path and migrates it.
func Open(ctx context.Context, path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	// Transactions take the write lock when they begin (_txlock=immediate):
	// the scheduler and the alert outbox both write, and a transaction that
	// read first and then tried to write could fail at once with
	// SQLITE_BUSY instead of waiting out busy_timeout.
	dsn := "file:" + uriPath.Replace(path) + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, ops: ops{db}}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating %s: %w", path, err)
	}
	return s, nil
}

// uriPath escapes the characters that would otherwise end or change the path
// part of a SQLite file: URI.
var uriPath = strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23")

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return err
	}
	var v int
	err := s.db.QueryRowContext(ctx, `SELECT version FROM schema_version`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (0)`); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if v > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this build supports (%d)", v, len(migrations))
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE schema_version SET version = ?`, i+1); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Release states (§3.3).
const (
	StateSkipped    = "skipped"
	StatePending    = "pending"
	StateFallback   = "fallback"
	StateSummarized = "summarized"
	StateNoNotes    = "no_notes"
)

// MissingGrace is how long a confirmed-deleted release stays in the feed (§5).
const MissingGrace = 7 * 24 * time.Hour

// Release is one stored release or tag. Fields are added as milestones need them.
type Release struct {
	Repo           string
	Tag            string
	GUID           string
	Kind           string // release | tag
	Name           string
	HTMLURL        string
	Stream         string
	Version        string
	PrevTag        string
	Prerelease     bool
	Published      time.Time // zero until known
	FirstSeen      time.Time
	State          string
	NotesSource    string
	Origin         string
	Urgency        string
	ContentUpdated time.Time
	MissingSince   time.Time
	NextAttempt    time.Time
	RefSHA         string // tags: the object refs/tags/<tag> points at
	RefType        string // tags: commit, or tag for an annotated tag
	NotesHash      string // → notes.hash
	LastVerified   time.Time
	VerifyAttempt  time.Time
	BodyHash       string // → notes.hash: the release body; "" for none
	NotesResolved  bool   // notes_source is final: "" then means no notes (§3.2)
	ResolveAttempt time.Time
	SummaryKey     string // → summaries.key; "" until summarized
	Attempts       int    // failed summarize attempts (§3.3)
	LastError      string
	Resummarize    time.Time // when resummarize asked for a fresh summary; zero for none (§3.3)
}

const releaseColumns = `repo, tag, guid, kind, name, html_url, stream, version, prev_tag, prerelease,
	published_at, first_seen_at, state, notes_source, origin, urgency, content_updated_at, missing_since, next_attempt_at,
	ref_sha, ref_type, notes_hash, last_verified_at, verify_attempted_at, body_hash, notes_resolved, resolve_attempted_at,
	summary_key, attempts, last_error, resummarize`

func scanRelease(row interface{ Scan(...any) error }) (*Release, error) {
	var r Release
	var published, contentUpdated, missing, verified, attempted, resolveAttempt sql.NullInt64
	var firstSeen, nextAttempt, resummarize int64
	var summaryKey sql.NullString
	err := row.Scan(&r.Repo, &r.Tag, &r.GUID, &r.Kind, &r.Name, &r.HTMLURL, &r.Stream, &r.Version, &r.PrevTag,
		&r.Prerelease, &published, &firstSeen, &r.State, &r.NotesSource, &r.Origin, &r.Urgency, &contentUpdated,
		&missing, &nextAttempt, &r.RefSHA, &r.RefType, &r.NotesHash, &verified, &attempted,
		&r.BodyHash, &r.NotesResolved, &resolveAttempt, &summaryKey, &r.Attempts, &r.LastError, &resummarize)
	if err != nil {
		return nil, err
	}
	r.Published, r.ContentUpdated, r.MissingSince = fromNull(published), fromNull(contentUpdated), fromNull(missing)
	r.LastVerified, r.VerifyAttempt, r.ResolveAttempt = fromNull(verified), fromNull(attempted), fromNull(resolveAttempt)
	r.FirstSeen, r.NextAttempt = fromMS(firstSeen), fromMS(nextAttempt)
	r.SummaryKey = summaryKey.String
	if resummarize > 0 {
		r.Resummarize = fromMS(resummarize)
	}
	return &r, nil
}

// InsertRelease adds a newly discovered release. A release already stored
// under (repo, tag) is left unchanged, so published_at and the GUID stay
// frozen (§5); it reports whether a row was inserted.
func (s ops) InsertRelease(ctx context.Context, r *Release) (bool, error) {
	if r.NextAttempt.IsZero() {
		r.NextAttempt = r.FirstSeen // §3.3: due at once
	}
	if r.Urgency == "" {
		r.Urgency = "none"
	}
	res, err := s.q.ExecContext(ctx, `INSERT INTO releases (`+releaseColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (repo, tag) DO NOTHING`,
		r.Repo, r.Tag, r.GUID, r.Kind, r.Name, r.HTMLURL, r.Stream, r.Version, r.PrevTag, r.Prerelease,
		toNull(r.Published), toMS(r.FirstSeen), r.State, r.NotesSource, r.Origin, r.Urgency,
		toNull(r.ContentUpdated), toNull(r.MissingSince), toMS(r.NextAttempt),
		r.RefSHA, r.RefType, r.NotesHash, toNull(r.LastVerified), toNull(r.VerifyAttempt),
		r.BodyHash, r.NotesResolved, toNull(r.ResolveAttempt), nullString(r.SummaryKey), r.Attempts, r.LastError, msOrZero(r.Resummarize))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Release returns the stored release, or nil.
func (s ops) Release(ctx context.Context, repo, tag string) (*Release, error) {
	r, err := scanRelease(s.q.QueryRowContext(ctx, `SELECT `+releaseColumns+` FROM releases WHERE repo = ? AND tag = ?`, repo, tag))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// FeedReleases returns up to n visible releases, newest first: published
// (fallback, summarized or no_notes) with a known date, and not deleted for
// longer than MissingGrace. Ties are broken by first-seen time, then GUID,
// so the order is deterministic.
func (s *Store) FeedReleases(ctx context.Context, n int, now time.Time) ([]*Release, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+releaseColumns+` FROM releases
		WHERE state IN (?, ?, ?) AND published_at IS NOT NULL
		  AND (missing_since IS NULL OR missing_since > ?)
		ORDER BY published_at DESC, first_seen_at DESC, guid LIMIT ?`,
		StateFallback, StateSummarized, StateNoNotes, toMS(now.Add(-MissingGrace)), n)
	if err != nil {
		return nil, err
	}
	return scanReleases(rows)
}

func scanReleases(rows *sql.Rows) ([]*Release, error) {
	defer rows.Close()
	var out []*Release
	for rows.Next() {
		r, err := scanRelease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Rendered is a feed's stored bytes, ETag and the time its bytes last changed.
type Rendered struct {
	Bytes    []byte
	ETag     string
	Modified time.Time
}

// Rendered returns the feed stored at path, or nil.
func (s *Store) Rendered(ctx context.Context, path string) (*Rendered, error) {
	var r Rendered
	var ms int64
	err := s.db.QueryRowContext(ctx, `SELECT bytes, etag, modified_at FROM rendered WHERE path = ?`, path).Scan(&r.Bytes, &r.ETag, &ms)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.Modified = fromMS(ms)
	return &r, nil
}

// PutRendered replaces the feed stored at path.
func (s *Store) PutRendered(ctx context.Context, path string, r *Rendered) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO rendered (path, bytes, etag, modified_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (path) DO UPDATE SET bytes = excluded.bytes, etag = excluded.etag, modified_at = excluded.modified_at`,
		path, r.Bytes, r.ETag, toMS(r.Modified))
	return err
}

// PutRenderedAll replaces every given feed in one transaction, so a failed
// write never leaves one format new and another old.
func (s *Store) PutRenderedAll(ctx context.Context, feeds map[string]*Rendered) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for path, r := range feeds {
		if _, err := tx.ExecContext(ctx, `INSERT INTO rendered (path, bytes, etag, modified_at) VALUES (?, ?, ?, ?)
			ON CONFLICT (path) DO UPDATE SET bytes = excluded.bytes, etag = excluded.etag, modified_at = excluded.modified_at`,
			path, r.Bytes, r.ETag, toMS(r.Modified)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// State returns a scheduler_state value, or "" if unset.
func (s ops) State(ctx context.Context, key string) (string, error) {
	var v string
	err := s.q.QueryRowContext(ctx, `SELECT value FROM scheduler_state WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetState replaces a scheduler_state value.
func (s ops) SetState(ctx context.Context, key, value string) error {
	_, err := s.q.ExecContext(ctx, `INSERT INTO scheduler_state (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// StateTime and SetStateTime store a time in scheduler_state.
func (s *Store) StateTime(ctx context.Context, key string) (time.Time, error) {
	v, err := s.State(ctx, key)
	if err != nil || v == "" {
		return time.Time{}, err
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("scheduler_state %s: %w", key, err)
	}
	return t, nil
}

func (s *Store) SetStateTime(ctx context.Context, key string, t time.Time) error {
	return s.SetState(ctx, key, t.UTC().Format(time.RFC3339Nano))
}

func toMS(t time.Time) int64 { return t.UnixMilli() }

// msOrZero is toMS, with 0 for the zero time.
func msOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return toMS(t)
}

func fromMS(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

func toNull(t time.Time) sql.NullInt64 {
	if t.IsZero() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.UnixMilli(), Valid: true}
}

func fromNull(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return fromMS(v.Int64)
}
