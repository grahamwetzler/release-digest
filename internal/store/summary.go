package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Summarize backoff (§3.3): a failed attempt waits min(BackoffBase ×
// 2^attempts, BackoffMax) before the next.
const (
	BackoffBase = 15 * time.Minute
	BackoffMax  = 24 * time.Hour
)

// Summary is a stored LLM summary, keyed by its canonical request (§3.3).
type Summary struct {
	Key       string
	Model     string
	JSON      string
	Created   time.Time
	TokensIn  int
	TokensOut int
}

// Summary returns the summary stored under key, or nil.
func (s ops) Summary(ctx context.Context, key string) (*Summary, error) {
	if key == "" {
		return nil, nil
	}
	sum := Summary{Key: key}
	var created int64
	var in, out sql.NullInt64
	err := s.q.QueryRowContext(ctx, `SELECT model, json, created_at, tokens_in, tokens_out FROM summaries WHERE key = ?`, key).
		Scan(&sum.Model, &sum.JSON, &created, &in, &out)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sum.Created, sum.TokensIn, sum.TokensOut = fromMS(created), int(in.Int64), int(out.Int64)
	return &sum, nil
}

// PutSummary stores a summary. A summary already stored under its key is
// kept: the key fixes the request, so both are answers to the same one.
func (s ops) PutSummary(ctx context.Context, sum *Summary) error {
	_, err := s.q.ExecContext(ctx, `INSERT INTO summaries (key, model, json, created_at, tokens_in, tokens_out)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (key) DO NOTHING`,
		sum.Key, sum.Model, sum.JSON, toMS(sum.Created), sum.TokensIn, sum.TokensOut)
	return err
}

// DueSummaries returns up to n rows eligible for the summarize pass (§3.3),
// in repos confirmed in tick, oldest first_seen_at first: pending or
// fallback, with resolved notes, due by next_attempt_at, dated, and not
// deleted. Rows whose notes are unresolved wait for resolution (§3.2), so
// provisional notes are never summarized.
func (s ops) DueSummaries(ctx context.Context, tick int64, now time.Time, n int) ([]*Release, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT `+prefixed("r.")+` FROM releases r
		JOIN repo_state rs ON rs.repo = r.repo AND rs.visibility_confirmed_tick = ?1 AND rs.held = 0
		WHERE r.state IN (?2, ?3) AND r.notes_resolved = 1 AND r.notes_source != '' AND r.notes_hash != ''
		  AND r.next_attempt_at <= ?4 AND r.published_at IS NOT NULL AND r.missing_since IS NULL
		ORDER BY r.first_seen_at, r.repo, r.tag LIMIT ?5`,
		tick, StatePending, StateFallback, toMS(now), n)
	if err != nil {
		return nil, err
	}
	return scanReleases(rows)
}

// Summarized records a summary on a pending or fallback row whose notes are
// still notesHash, publishing it if it was pending. It reports whether the
// row changed; it doesn't if the notes changed meanwhile.
func (s ops) Summarized(ctx context.Context, repo, tag, notesHash, key string, at time.Time) (bool, error) {
	res, err := s.q.ExecContext(ctx, `UPDATE releases SET state = ?, summary_key = ?, attempts = 0, last_error = '',
		content_updated_at = ? WHERE repo = ? AND tag = ? AND notes_hash = ? AND state IN (?, ?)`,
		StateSummarized, key, toMS(at), repo, tag, notesHash, StatePending, StateFallback)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// SummaryFailed records a failed summarize attempt and backs the row off:
// the next attempt is due min(BackoffBase × 2^attempts, BackoffMax) after
// at, counting the attempts before this one, so the first failure waits
// BackoffBase.
func (s ops) SummaryFailed(ctx context.Context, repo, tag, msg string, at time.Time) error {
	_, err := s.q.ExecContext(ctx, `UPDATE releases SET attempts = attempts + 1, last_error = ?,
		next_attempt_at = ? + MIN(? << MIN(attempts, 16), ?) WHERE repo = ? AND tag = ?`,
		msg, toMS(at), BackoffBase.Milliseconds(), BackoffMax.Milliseconds(), repo, tag)
	return err
}
