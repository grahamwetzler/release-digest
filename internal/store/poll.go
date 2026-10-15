package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// Release origins (§3.1).
const (
	OriginLive      = "live"
	OriginBackfill  = "backfill"
	OriginCatchup   = "catchup"
	OriginReconcile = "reconcile"
)

// visibleStates are the states shown in the feed (§3.3).
var visibleStates = []any{StateFallback, StateSummarized, StateNoNotes}

const visibleIn = `state IN (?, ?, ?)`

// RepoState is one repo's poll bookkeeping (§8.1). Zero times and "" mean unset.
type RepoState struct {
	Repo              string
	InitializedAt     time.Time
	LastSuccessAt     time.Time
	Held              bool
	ConfirmedAt       time.Time
	ConfirmedTick     int64 // 0: not confirmed
	ScanBoundary      string
	ScanMode          string // release | tag: which list the last completed scan read
	ReconcileNextPage int    // 0: no reconciliation in progress
	LastReconciledAt  time.Time
	LastError         string
}

// RepoState returns the repo's state; a repo never seen gets a zero state.
func (s ops) RepoState(ctx context.Context, repo string) (*RepoState, error) {
	rs := RepoState{Repo: repo}
	var init, success, confirmedAt, reconciled, tick, page sql.NullInt64
	var boundary sql.NullString
	err := s.q.QueryRowContext(ctx, `SELECT initialized_at, last_success_at, held, visibility_confirmed_at,
		visibility_confirmed_tick, scan_boundary, reconcile_next_page, last_reconciled_at, last_error, scan_mode
		FROM repo_state WHERE repo = ?`, repo).Scan(&init, &success, &rs.Held, &confirmedAt, &tick, &boundary,
		&page, &reconciled, &rs.LastError, &rs.ScanMode)
	if errors.Is(err, sql.ErrNoRows) {
		return &rs, nil
	}
	if err != nil {
		return nil, err
	}
	rs.InitializedAt, rs.LastSuccessAt = fromNull(init), fromNull(success)
	rs.ConfirmedAt, rs.LastReconciledAt = fromNull(confirmedAt), fromNull(reconciled)
	rs.ConfirmedTick, rs.ScanBoundary, rs.ReconcileNextPage = tick.Int64, boundary.String, int(page.Int64)
	return &rs, nil
}

// PutRepoState replaces the repo's state.
func (s ops) PutRepoState(ctx context.Context, rs *RepoState) error {
	_, err := s.q.ExecContext(ctx, `INSERT INTO repo_state (repo, initialized_at, last_success_at, held,
		visibility_confirmed_at, visibility_confirmed_tick, scan_boundary, reconcile_next_page, last_reconciled_at, last_error,
		scan_mode)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (repo) DO UPDATE SET initialized_at = excluded.initialized_at, last_success_at = excluded.last_success_at,
			held = excluded.held, visibility_confirmed_at = excluded.visibility_confirmed_at,
			visibility_confirmed_tick = excluded.visibility_confirmed_tick, scan_boundary = excluded.scan_boundary,
			reconcile_next_page = excluded.reconcile_next_page, last_reconciled_at = excluded.last_reconciled_at,
			last_error = excluded.last_error, scan_mode = excluded.scan_mode`,
		rs.Repo, toNull(rs.InitializedAt), toNull(rs.LastSuccessAt), rs.Held, toNull(rs.ConfirmedAt),
		nullInt(rs.ConfirmedTick), nullString(rs.ScanBoundary), nullInt(int64(rs.ReconcileNextPage)),
		toNull(rs.LastReconciledAt), rs.LastError, rs.ScanMode)
	return err
}

// Visibility records the outcome of a public check (§3.1 step 0). A public
// repo is confirmed for tick and un-held. Any other outcome clears the
// confirmation; private or not found (gone) also holds the repo.
func (s ops) Visibility(ctx context.Context, repo string, tick int64, public, gone bool, at time.Time, errText string) error {
	rs, err := s.RepoState(ctx, repo)
	if err != nil {
		return err
	}
	switch {
	case public:
		rs.Held, rs.ConfirmedTick, rs.ConfirmedAt = false, tick, at
	case gone:
		rs.Held, rs.ConfirmedTick = true, 0
	default:
		rs.ConfirmedTick = 0
	}
	if errText != "" {
		rs.LastError = errText
	}
	return s.PutRepoState(ctx, rs)
}

// Confirmed reports whether the repo passed its public check in tick and is
// not held: the scheduler half of the processing gate (§3.1).
func (s ops) Confirmed(ctx context.Context, repo string, tick int64) (bool, error) {
	rs, err := s.RepoState(ctx, repo)
	if err != nil {
		return false, err
	}
	return !rs.Held && tick != 0 && rs.ConfirmedTick == tick, nil
}

// SetRepoError records a poll failure for the repo.
func (s ops) SetRepoError(ctx context.Context, repo, msg string) error {
	_, err := s.q.ExecContext(ctx, `INSERT INTO repo_state (repo, last_error) VALUES (?, ?)
		ON CONFLICT (repo) DO UPDATE SET last_error = excluded.last_error`, repo, msg)
	return err
}

// RepoTags returns the tags of every stored row of the repo, any kind or state.
func (s ops) RepoTags(ctx context.Context, repo string) (map[string]bool, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT tag FROM releases WHERE repo = ?`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var tag string
		if err := rows.Scan(&tag); err != nil {
			return nil, err
		}
		out[tag] = true
	}
	return out, rows.Err()
}

// StreamReleases returns every stored row of the repo in the stream, any
// state, for predecessor selection (§3.2.1).
func (s ops) StreamReleases(ctx context.Context, repo, stream string) ([]*Release, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT `+releaseColumns+` FROM releases WHERE repo = ? AND stream = ?`, repo, stream)
	if err != nil {
		return nil, err
	}
	return scanReleases(rows)
}

// SetPrevTag records the release's predecessor.
func (s ops) SetPrevTag(ctx context.Context, repo, tag, prev string) error {
	_, err := s.q.ExecContext(ctx, `UPDATE releases SET prev_tag = ? WHERE repo = ? AND tag = ?`, prev, repo, tag)
	return err
}

// Seen records that tags appeared in a list response, which clears any
// missing_since (§3.1, deletion detection).
func (s ops) Seen(ctx context.Context, repo string, tags []string) error {
	for _, tag := range tags {
		if _, err := s.q.ExecContext(ctx, `UPDATE releases SET missing_since = NULL
			WHERE repo = ? AND tag = ? AND missing_since IS NOT NULL`, repo, tag); err != nil {
			return err
		}
	}
	return nil
}

// RepoVisible returns the repo's visible rows with a known date, newest first.
func (s ops) RepoVisible(ctx context.Context, repo string) ([]*Release, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT `+releaseColumns+` FROM releases
		WHERE repo = ? AND `+visibleIn+` AND published_at IS NOT NULL ORDER BY published_at DESC`,
		append([]any{repo}, visibleStates...)...)
	if err != nil {
		return nil, err
	}
	return scanReleases(rows)
}

// PutNotes stores text and returns its hash. Notes are content-addressed, so
// identical notes are stored once.
func (s ops) PutNotes(ctx context.Context, text string) (string, error) {
	sum := sha256.Sum256([]byte(text))
	h := hex.EncodeToString(sum[:])
	_, err := s.q.ExecContext(ctx, `INSERT INTO notes (hash, text) VALUES (?, ?) ON CONFLICT (hash) DO NOTHING`, h, text)
	return h, err
}

// Notes returns the text stored under hash, or "".
func (s ops) Notes(ctx context.Context, hash string) (string, error) {
	if hash == "" {
		return "", nil
	}
	var text string
	err := s.q.QueryRowContext(ctx, `SELECT text FROM notes WHERE hash = ?`, hash).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return text, err
}

// SetPendingNotes replaces the release-body notes of a release that isn't
// visible yet, for a body edited after discovery (an empty source and hash
// clear them). Visible rows are left alone: their content changes only
// through the transitions in §3.3. Notes from another source are kept.
func (s ops) SetPendingNotes(ctx context.Context, repo, tag, source, hash string) error {
	_, err := s.q.ExecContext(ctx, `UPDATE releases SET notes_source = ?, notes_hash = ?
		WHERE repo = ? AND tag = ? AND state = ? AND notes_source IN ('', 'release_body')`,
		source, hash, repo, tag, StatePending)
	return err
}

// UndatedTags returns up to n tag rows whose commit date is unknown, in
// repos confirmed in tick: the metadata queue (§3.1 step 2). Rows never
// attempted come first, then the least recently attempted, so lookups that
// keep failing rotate to the back. Skipped rows with a parsed version are
// never dated: nothing shows them, and SemVer predecessors don't need dates.
func (s ops) UndatedTags(ctx context.Context, tick int64, n int) ([]*Release, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT `+prefixed("r.")+` FROM releases r
		JOIN repo_state rs ON rs.repo = r.repo AND rs.visibility_confirmed_tick = ?3 AND rs.held = 0
		WHERE r.published_at IS NULL AND r.ref_sha != '' AND (r.state != ?1 OR r.version = '')
		ORDER BY r.meta_attempted_at IS NOT NULL, r.meta_attempted_at, r.state = ?1, r.first_seen_at, r.repo, r.tag
		LIMIT ?2`, StateSkipped, n, tick)
	if err != nil {
		return nil, err
	}
	return scanReleases(rows)
}

// MetaAttempted records a metadata lookup attempt, whatever its outcome.
func (s ops) MetaAttempted(ctx context.Context, repo, tag string, at time.Time) error {
	_, err := s.q.ExecContext(ctx, `UPDATE releases SET meta_attempted_at = ? WHERE repo = ? AND tag = ?`, toMS(at), repo, tag)
	return err
}

// SetPublished sets a release's date if it is still unknown; once set it is
// frozen (§5).
func (s ops) SetPublished(ctx context.Context, repo, tag string, at time.Time) error {
	_, err := s.q.ExecContext(ctx, `UPDATE releases SET published_at = ? WHERE repo = ? AND tag = ? AND published_at IS NULL`,
		toMS(at), repo, tag)
	return err
}

// Skip moves a pending row to skipped, for a reconciled tag whose date turned
// out to be outside the publish window (§3.1).
func (s ops) Skip(ctx context.Context, repo, tag string) error {
	_, err := s.q.ExecContext(ctx, `UPDATE releases SET state = ? WHERE repo = ? AND tag = ? AND state = ?`,
		StateSkipped, repo, tag, StatePending)
	return err
}

// VerifyDue returns up to n visible releases in repos confirmed in tick,
// least recently attempted first: the rotating deletion sweep (§3.1).
func (s ops) VerifyDue(ctx context.Context, tick int64, n int) ([]*Release, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT `+prefixed("r.")+` FROM releases r
		JOIN repo_state rs ON rs.repo = r.repo AND rs.visibility_confirmed_tick = ? AND rs.held = 0
		WHERE r.`+visibleIn+` AND r.published_at IS NOT NULL
		ORDER BY r.verify_attempted_at IS NOT NULL, r.verify_attempted_at, r.published_at, r.repo, r.tag LIMIT ?`,
		append(append([]any{tick}, visibleStates...), n)...)
	if err != nil {
		return nil, err
	}
	return scanReleases(rows)
}

// Existence-check outcomes (§3.1).
const (
	VerifyFound = iota
	VerifyNotFound
	VerifyFailed
)

// RecordVerify records an existence check. Found clears missing_since; not
// found sets it (keeping an earlier time); a failure only records the attempt.
func (s ops) RecordVerify(ctx context.Context, repo, tag string, outcome int, at time.Time) error {
	var q string
	switch outcome {
	case VerifyFound:
		q = `UPDATE releases SET verify_attempted_at = ?1, last_verified_at = ?1, missing_since = NULL WHERE repo = ?2 AND tag = ?3`
	case VerifyNotFound:
		q = `UPDATE releases SET verify_attempted_at = ?1, missing_since = COALESCE(missing_since, ?1) WHERE repo = ?2 AND tag = ?3`
	default:
		q = `UPDATE releases SET verify_attempted_at = ?1 WHERE repo = ?2 AND tag = ?3`
	}
	_, err := s.q.ExecContext(ctx, q, toMS(at), repo, tag)
	return err
}

// DuePromotions returns pending rows with a known date first seen at or
// before cutoff: due to be published as fallbacks (§3.3).
func (s ops) DuePromotions(ctx context.Context, cutoff time.Time) ([]*Release, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT `+releaseColumns+` FROM releases
		WHERE state = ? AND published_at IS NOT NULL AND first_seen_at <= ? ORDER BY repo, first_seen_at, tag`,
		StatePending, toMS(cutoff))
	if err != nil {
		return nil, err
	}
	return scanReleases(rows)
}

// Promote publishes a pending row as a fallback, making it visible.
func (s ops) Promote(ctx context.Context, repo, tag string, at time.Time) error {
	_, err := s.q.ExecContext(ctx, `UPDATE releases SET state = ?, content_updated_at = ? WHERE repo = ? AND tag = ? AND state = ?`,
		StateFallback, toMS(at), repo, tag, StatePending)
	return err
}

// CacheEntry is a stored HTTP response for conditional requests.
type CacheEntry struct {
	ETag string
	Body []byte
}

// Cached returns the cache entry for url, or nil.
func (s ops) Cached(ctx context.Context, url string) (*CacheEntry, error) {
	var e CacheEntry
	var etag sql.NullString
	err := s.q.QueryRowContext(ctx, `SELECT etag, body FROM http_cache WHERE url = ?`, url).Scan(&etag, &e.Body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	e.ETag = etag.String
	return &e, nil
}

// PutCached stores a response under url.
func (s ops) PutCached(ctx context.Context, url string, e *CacheEntry, at time.Time) error {
	_, err := s.q.ExecContext(ctx, `INSERT INTO http_cache (url, etag, body, fetched_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (url) DO UPDATE SET etag = excluded.etag, body = excluded.body, fetched_at = excluded.fetched_at`,
		url, e.ETag, e.Body, toMS(at))
	return err
}

// prefixed qualifies releaseColumns with a table alias.
func prefixed(alias string) string {
	cols := strings.Split(releaseColumns, ",")
	for i, c := range cols {
		cols[i] = alias + strings.TrimSpace(c)
	}
	return strings.Join(cols, ", ")
}

func nullInt(n int64) sql.NullInt64 { return sql.NullInt64{Int64: n, Valid: n != 0} }

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }
