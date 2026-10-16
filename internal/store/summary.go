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

// ReplaceSummary stores a summary, replacing one stored under its key: a
// resummarize asked for a fresh answer to the same request (§3.3).
func (s ops) ReplaceSummary(ctx context.Context, sum *Summary) error {
	_, err := s.q.ExecContext(ctx, `INSERT INTO summaries (key, model, json, created_at, tokens_in, tokens_out)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (key) DO UPDATE SET model = excluded.model, json = excluded.json,
		created_at = excluded.created_at, tokens_in = excluded.tokens_in, tokens_out = excluded.tokens_out`,
		sum.Key, sum.Model, sum.JSON, toMS(sum.Created), sum.TokensIn, sum.TokensOut)
	return err
}

// RequestResummarize flags the releases that resummarize names (§3.3):
// summarized, fallback or pending rows published at or after since, in
// repo if it isn't "", and not deleted. Each records the request time now
// and is due at once with its backoff reset. Only a summary created after
// now satisfies the request, so the cache is bypassed until one exists.
// A summarized row keeps its summary until the new one is published. It
// returns the flagged rows.
func (s *Store) RequestResummarize(ctx context.Context, repo string, since, now time.Time) ([]*Release, error) {
	var out []*Release
	err := s.Tx(ctx, func(tx *Tx) error {
		const where = `state IN (?1, ?2, ?3) AND published_at IS NOT NULL AND published_at >= ?4
			AND missing_since IS NULL AND (?5 = '' OR repo = ?5)`
		args := []any{StateSummarized, StateFallback, StatePending, toMS(since), repo}
		rows, err := tx.q.QueryContext(ctx, `SELECT `+releaseColumns+` FROM releases WHERE `+where+
			` ORDER BY repo, published_at DESC, tag`, args...)
		if err != nil {
			return err
		}
		if out, err = scanReleases(rows); err != nil {
			return err
		}
		_, err = tx.q.ExecContext(ctx, `UPDATE releases SET resummarize = ?6, attempts = 0, next_attempt_at = ?6 WHERE `+where,
			append(args, toMS(now))...)
		return err
	})
	return out, err
}

// DueSummaries returns up to n rows eligible for the summarize pass (§3.3),
// in repos confirmed in tick: pending or fallback, or summarized and
// flagged by resummarize, with resolved notes, due by next_attempt_at,
// dated, and not deleted. Rows whose notes are unresolved wait for
// resolution (§3.2), so provisional notes are never summarized. Unflagged
// rows come first, so a large resummarize doesn't delay new releases; then
// oldest first_seen_at first.
func (s ops) DueSummaries(ctx context.Context, tick int64, now time.Time, n int) ([]*Release, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT `+prefixed("r.")+` FROM releases r
		JOIN repo_state rs ON rs.repo = r.repo AND rs.visibility_confirmed_tick = ?1 AND rs.held = 0
		WHERE (r.state IN (?2, ?3) OR (r.state = ?6 AND r.resummarize > 0)) AND r.notes_resolved = 1 AND r.notes_source != '' AND r.notes_hash != ''
		  AND r.next_attempt_at <= ?4 AND r.published_at IS NOT NULL AND r.missing_since IS NULL
		ORDER BY r.resummarize > 0, r.first_seen_at, r.repo, r.tag LIMIT ?5`,
		tick, StatePending, StateFallback, toMS(now), n, StateSummarized)
	if err != nil {
		return nil, err
	}
	return scanReleases(rows)
}

// Summarized records a summary on a pending or fallback row, or a
// summarized row flagged by resummarize, whose notes are still notesHash,
// publishing it if it was pending. created is when the summary was made:
// it clears a resummarize request made at or before then, but not a later
// one, which the next pass serves. The update is atomic
// with the gate: it applies only while the repo is confirmed for tick and
// not held (§3.1). The new summary is an urgency input, so the row's
// urgency is marked for recomputing (§3.4). It reports whether the row
// changed; it doesn't if the notes changed or the confirmation was lost
// meanwhile.
func (s ops) Summarized(ctx context.Context, repo, tag, notesHash, key string, created time.Time, tick int64, at time.Time) (bool, error) {
	res, err := s.q.ExecContext(ctx, `UPDATE releases SET state = ?1, summary_key = ?2, attempts = 0, last_error = '',
		content_updated_at = ?3, urgency_dirty = 1, resummarize = CASE WHEN resummarize <= ?4 THEN 0 ELSE resummarize END
		WHERE repo = ?5 AND tag = ?6 AND notes_hash = ?7 AND (state IN (?8, ?9) OR (state = ?1 AND resummarize > 0))
		AND EXISTS (SELECT 1 FROM repo_state rs WHERE rs.repo = releases.repo
			AND rs.visibility_confirmed_tick = ?10 AND rs.held = 0)`,
		StateSummarized, key, toMS(at), toMS(created), repo, tag, notesHash, StatePending, StateFallback, tick)
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
