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

// SetBody records an edited release body (bodyHash, "" for an empty body)
// on a row whose notes may still change: pending, fallback or no_notes.
// The row's notes go back to unresolved; a pending row, which isn't
// visible, also takes src and hash as provisional notes. Visible rows keep
// their content until resolution (§3.3). It reports whether the body changed.
func (s ops) SetBody(ctx context.Context, repo, tag, bodyHash, src, hash string) (bool, error) {
	res, err := s.q.ExecContext(ctx, `UPDATE releases SET body_hash = ?1, notes_resolved = 0, resolve_attempted_at = NULL,
			notes_source = CASE WHEN state = ?4 THEN ?2 ELSE notes_source END,
			notes_hash = CASE WHEN state = ?4 THEN ?3 ELSE notes_hash END
		WHERE repo = ?5 AND tag = ?6 AND body_hash != ?1 AND state IN (?4, ?7, ?8)`,
		bodyHash, src, hash, StatePending, repo, tag, StateFallback, StateNoNotes)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ResolveNotes records resolved notes (§3.2, §3.3). With notes (src set), a
// no_notes row returns to fallback; without (src ""), a fallback row becomes
// no_notes. A visible row whose content changes gets content_updated_at, and
// a row with new notes is due for summarizing at once.
func (s ops) ResolveNotes(ctx context.Context, repo, tag, src, hash string, at time.Time) error {
	var q string
	if src != "" {
		q = `UPDATE releases SET
			content_updated_at = CASE WHEN state = ?3 OR (state = ?2 AND notes_hash != ?5) THEN ?6 ELSE content_updated_at END,
			next_attempt_at = CASE WHEN state = ?3 OR notes_hash != ?5 THEN ?6 ELSE next_attempt_at END,
			state = CASE WHEN state = ?3 THEN ?2 ELSE state END,
			notes_source = ?4, notes_hash = ?5, notes_resolved = 1, resolve_attempted_at = ?6
			WHERE repo = ?7 AND tag = ?8 AND state IN (?1, ?2, ?3)`
	} else {
		q = `UPDATE releases SET
			content_updated_at = CASE WHEN state = ?2 THEN ?6 ELSE content_updated_at END,
			state = CASE WHEN state = ?2 THEN ?3 ELSE state END,
			notes_source = ?4, notes_hash = ?5, notes_resolved = 1, resolve_attempted_at = ?6
			WHERE repo = ?7 AND tag = ?8 AND state IN (?1, ?2, ?3)`
	}
	_, err := s.q.ExecContext(ctx, q, StatePending, StateFallback, StateNoNotes, src, hash, toMS(at), repo, tag)
	return err
}

// ResolveAttempted records a resolution attempt that failed.
func (s ops) ResolveAttempted(ctx context.Context, repo, tag string, at time.Time) error {
	_, err := s.q.ExecContext(ctx, `UPDATE releases SET resolve_attempted_at = ? WHERE repo = ? AND tag = ?`, toMS(at), repo, tag)
	return err
}

// UnresolvedNotes returns up to n rows due for notes resolution in repos
// confirmed in tick (§3.2): dated pending, fallback and no_notes rows whose
// notes aren't resolved (a body edit unresolves them), whatever their age;
// then resolved no_notes rows published since recent and not tried since
// retry, which are rechecked in case notes appeared. Within each group,
// rows never attempted come first, then the least recently attempted.
func (s ops) UnresolvedNotes(ctx context.Context, tick int64, n int, recent, retry time.Time) ([]*Release, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT `+prefixed("r.")+` FROM releases r
		JOIN repo_state rs ON rs.repo = r.repo AND rs.visibility_confirmed_tick = ?1 AND rs.held = 0
		WHERE r.published_at IS NOT NULL AND (
			(r.notes_resolved = 0 AND r.state IN (?2, ?3, ?4))
			OR (r.state = ?4 AND r.published_at > ?5 AND (r.resolve_attempted_at IS NULL OR r.resolve_attempted_at <= ?6)))
		ORDER BY r.notes_resolved, r.resolve_attempted_at IS NOT NULL, r.resolve_attempted_at, r.first_seen_at, r.repo, r.tag
		LIMIT ?7`, tick, StatePending, StateFallback, StateNoNotes, toMS(recent), toMS(retry), n)
	if err != nil {
		return nil, err
	}
	return scanReleases(rows)
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

// DuePromotions returns pending rows with a known date that are due to be
// published (§3.3): first seen at or before cutoff, or resolved to no
// notes, which has nothing to wait for.
func (s ops) DuePromotions(ctx context.Context, cutoff time.Time) ([]*Release, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT `+releaseColumns+` FROM releases
		WHERE state = ? AND published_at IS NOT NULL AND (first_seen_at <= ? OR (notes_resolved = 1 AND notes_source = ''))
		ORDER BY repo, first_seen_at, tag`,
		StatePending, toMS(cutoff))
	if err != nil {
		return nil, err
	}
	return scanReleases(rows)
}

// Promote publishes a pending row, making it visible: as no_notes if its
// notes resolved to none, else as a fallback.
func (s ops) Promote(ctx context.Context, repo, tag string, at time.Time) error {
	_, err := s.q.ExecContext(ctx, `UPDATE releases SET
		state = CASE WHEN notes_resolved = 1 AND notes_source = '' THEN ? ELSE ? END, content_updated_at = ?
		WHERE repo = ? AND tag = ? AND state = ?`,
		StateNoNotes, StateFallback, toMS(at), repo, tag, StatePending)
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
func prefixed(alias string) string { return prefixedWith(alias, releaseColumns) }

// prefixedWith qualifies a column list with a table alias.
func prefixedWith(alias, columns string) string {
	cols := strings.Split(columns, ",")
	for i, c := range cols {
		cols[i] = alias + strings.TrimSpace(c)
	}
	return strings.Join(cols, ", ")
}

func nullInt(n int64) sql.NullInt64 { return sql.NullInt64{Int64: n, Valid: n != 0} }

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }
