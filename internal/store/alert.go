package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"
)

// Urgencies (§3.4).
const (
	UrgencyNone     = "none"
	UrgencyAdvisory = "advisory"
	UrgencyUrgent   = "urgent"
)

// Advisory is a cached advisory lookup (§3.4). JSON holds the advisories
// found under the ID as an array: one for a GHSA ID, possibly several for a
// CVE ID.
type Advisory struct {
	ID          string
	Found       bool
	Severity    string // the highest of the advisories found
	WithdrawnAt time.Time
	JSON        string
	FetchedAt   time.Time // zero: never looked up
	NextFetchAt time.Time
	Errors      int // consecutive failed lookups
}

// Link is an advisory mentioned in a release's notes, with the outcome of
// the last urgency decision for it.
type Link struct {
	AdvisoryID string
	Matched    bool // the advisory belongs to the repo
	Fixes      bool // this release fixes it
	Advisory   *Advisory
}

// AdvisoryScansDue returns up to n releases in repos confirmed in tick
// whose resolved notes haven't been scanned for advisory IDs since they
// last changed.
func (s ops) AdvisoryScansDue(ctx context.Context, tick int64, n int) ([]*Release, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT `+prefixed("r.")+` FROM releases r
		JOIN repo_state rs ON rs.repo = r.repo AND rs.visibility_confirmed_tick = ? AND rs.held = 0
		WHERE r.state != ? AND r.notes_resolved = 1 AND r.advisories_hash != r.notes_hash
		ORDER BY r.first_seen_at, r.repo, r.tag LIMIT ?`, tick, StateSkipped, n)
	if err != nil {
		return nil, err
	}
	return scanReleases(rows)
}

// SetAdvisoryLinks records the advisory IDs found in a release's notes
// (notesHash): links to other IDs are removed, new IDs are queued for a
// lookup, and the release's urgency is marked for recomputing.
func (s ops) SetAdvisoryLinks(ctx context.Context, repo, tag, notesHash string, ids []string) error {
	old, err := s.linkIDs(ctx, repo, tag)
	if err != nil {
		return err
	}
	for _, id := range old {
		if !slices.Contains(ids, id) {
			if _, err := s.q.ExecContext(ctx, `DELETE FROM release_advisories WHERE repo = ? AND tag = ? AND advisory_id = ?`,
				repo, tag, id); err != nil {
				return err
			}
		}
	}
	for _, id := range ids {
		if _, err := s.q.ExecContext(ctx, `INSERT INTO release_advisories (repo, tag, advisory_id) VALUES (?, ?, ?)
			ON CONFLICT DO NOTHING`, repo, tag, id); err != nil {
			return err
		}
		if _, err := s.q.ExecContext(ctx, `INSERT INTO advisories (id, found, fetched_at, next_fetch_at) VALUES (?, 0, 0, 0)
			ON CONFLICT DO NOTHING`, id); err != nil {
			return err
		}
	}
	_, err = s.q.ExecContext(ctx, `UPDATE releases SET advisories_hash = ?, urgency_dirty = 1 WHERE repo = ? AND tag = ?`,
		notesHash, repo, tag)
	return err
}

func (s ops) linkIDs(ctx context.Context, repo, tag string) ([]string, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT advisory_id FROM release_advisories WHERE repo = ? AND tag = ? ORDER BY advisory_id`, repo, tag)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// AdvisoriesDue returns up to n advisory IDs due for a lookup: next_fetch_at
// reached, and linked to a release that is pending or visible and not
// deleted for longer than MissingGrace (§3.4). Never-fetched IDs come first.
func (s ops) AdvisoriesDue(ctx context.Context, now time.Time, n int) ([]string, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT a.id FROM advisories a WHERE a.next_fetch_at <= ?1
		AND EXISTS (SELECT 1 FROM release_advisories ra JOIN releases r ON r.repo = ra.repo AND r.tag = ra.tag
			WHERE ra.advisory_id = a.id AND r.state IN (?2, ?3, ?4, ?5) AND (r.missing_since IS NULL OR r.missing_since > ?6))
		ORDER BY a.fetched_at != 0, a.next_fetch_at, a.id LIMIT ?7`,
		toMS(now), StatePending, StateFallback, StateSummarized, StateNoNotes, toMS(now.Add(-MissingGrace)), n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// AdvisoryRecent reports whether a release linked to the advisory was
// published after since: such advisories are refreshed daily, others weekly.
func (s ops) AdvisoryRecent(ctx context.Context, id string, since time.Time) (bool, error) {
	var n int
	err := s.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM release_advisories ra JOIN releases r ON r.repo = ra.repo AND r.tag = ra.tag
		WHERE ra.advisory_id = ? AND r.published_at > ?`, id, toMS(since)).Scan(&n)
	return n > 0, err
}

// Advisory returns the cached advisory, or nil.
func (s ops) Advisory(ctx context.Context, id string) (*Advisory, error) {
	a := Advisory{ID: id}
	var sev, js sql.NullString
	var withdrawn sql.NullInt64
	var fetched, next int64
	err := s.q.QueryRowContext(ctx, `SELECT found, severity, withdrawn_at, json, fetched_at, next_fetch_at, errors
		FROM advisories WHERE id = ?`, id).Scan(&a.Found, &sev, &withdrawn, &js, &fetched, &next, &a.Errors)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a.Severity, a.JSON, a.WithdrawnAt = sev.String, js.String, fromNull(withdrawn)
	if fetched != 0 {
		a.FetchedAt = fromMS(fetched)
	}
	a.NextFetchAt = fromMS(next)
	return &a, nil
}

// PutAdvisory stores a lookup's outcome and marks every release linked to
// the advisory for an urgency recompute.
func (s ops) PutAdvisory(ctx context.Context, a *Advisory) error {
	_, err := s.q.ExecContext(ctx, `INSERT INTO advisories (id, found, severity, withdrawn_at, json, fetched_at, next_fetch_at, errors)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET found = excluded.found, severity = excluded.severity, withdrawn_at = excluded.withdrawn_at,
			json = excluded.json, fetched_at = excluded.fetched_at, next_fetch_at = excluded.next_fetch_at, errors = excluded.errors`,
		a.ID, a.Found, nullString(a.Severity), toNull(a.WithdrawnAt), nullString(a.JSON), toMS0(a.FetchedAt), toMS(a.NextFetchAt), a.Errors)
	if err != nil {
		return err
	}
	_, err = s.q.ExecContext(ctx, `UPDATE releases SET urgency_dirty = 1
		WHERE (repo, tag) IN (SELECT repo, tag FROM release_advisories WHERE advisory_id = ?)`, a.ID)
	return err
}

// Links returns the release's advisory links with their cached advisories.
func (s ops) Links(ctx context.Context, repo, tag string) ([]Link, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT advisory_id, matched, fixes FROM release_advisories
		WHERE repo = ? AND tag = ? ORDER BY advisory_id`, repo, tag)
	if err != nil {
		return nil, err
	}
	var out []Link
	for rows.Next() {
		var l Link
		if err := rows.Scan(&l.AdvisoryID, &l.Matched, &l.Fixes); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Advisory, err = s.Advisory(ctx, out[i].AdvisoryID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// SetLink records the outcome of an urgency decision for one link.
func (s ops) SetLink(ctx context.Context, repo, tag, id string, matched, fixes bool) error {
	_, err := s.q.ExecContext(ctx, `UPDATE release_advisories SET matched = ?, fixes = ? WHERE repo = ? AND tag = ? AND advisory_id = ?`,
		matched, fixes, repo, tag, id)
	return err
}

// UrgencyDirty returns the releases in repos confirmed in tick whose
// urgency needs recomputing.
func (s ops) UrgencyDirty(ctx context.Context, tick int64) ([]*Release, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT `+prefixed("r.")+` FROM releases r
		JOIN repo_state rs ON rs.repo = r.repo AND rs.visibility_confirmed_tick = ? AND rs.held = 0
		WHERE r.urgency_dirty = 1 ORDER BY r.first_seen_at, r.repo, r.tag`, tick)
	if err != nil {
		return nil, err
	}
	return scanReleases(rows)
}

// MarkAllUrgencyDirty marks every release that isn't skipped for an urgency
// recompute, after a config change to its inputs (§3.4).
func (s ops) MarkAllUrgencyDirty(ctx context.Context) (int64, error) {
	res, err := s.q.ExecContext(ctx, `UPDATE releases SET urgency_dirty = 1 WHERE state != ?`, StateSkipped)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetUrgency records a release's recomputed urgency and clears its dirty
// mark. A change on a visible release is a content change (§1), so it
// advances content_updated_at. It reports whether the urgency changed.
func (s ops) SetUrgency(ctx context.Context, repo, tag, urgency, source string, at time.Time) (bool, error) {
	var old string
	if err := s.q.QueryRowContext(ctx, `SELECT urgency FROM releases WHERE repo = ? AND tag = ?`, repo, tag).Scan(&old); err != nil {
		return false, err
	}
	_, err := s.q.ExecContext(ctx, `UPDATE releases SET urgency = ?1, urgency_source = ?2, urgency_dirty = 0,
		content_updated_at = CASE WHEN urgency != ?1 AND state IN (?3, ?4, ?5) THEN ?6 ELSE content_updated_at END
		WHERE repo = ?7 AND tag = ?8`,
		urgency, source, StateFallback, StateSummarized, StateNoNotes, toMS(at), repo, tag)
	return old != urgency, err
}

// toMS0 is toMS with the zero time as 0.
func toMS0(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return toMS(t)
}
