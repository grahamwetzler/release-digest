package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Outbox states (§3.4).
const (
	AlertQueued    = "queued"
	AlertSending   = "sending"
	AlertSent      = "sent"
	AlertExpired   = "expired"
	AlertCancelled = "cancelled"
)

// Notification is one alert in the outbox, at most one per release.
type Notification struct {
	GUID            string
	Repo, Tag       string
	Channel         string // ntfy
	Reason          string // advisory:GHSA-… | llm
	Payload         string
	State           string
	CancelRequested bool
	Attempts        int
	NextAttempt     time.Time
	Created         time.Time
	Sent            time.Time
	Expires         time.Time // the end of the alert window
	LastError       string
	Acked           time.Time
}

const notificationColumns = `guid, repo, tag, channel, reason, payload, state, cancel_requested, attempts,
	next_attempt_at, created_at, sent_at, expires_at, last_error, acked_at`

func scanNotification(row interface{ Scan(...any) error }) (*Notification, error) {
	var n Notification
	var next, sent, acked sql.NullInt64
	var created, expires int64
	err := row.Scan(&n.GUID, &n.Repo, &n.Tag, &n.Channel, &n.Reason, &n.Payload, &n.State, &n.CancelRequested,
		&n.Attempts, &next, &created, &sent, &expires, &n.LastError, &acked)
	if err != nil {
		return nil, err
	}
	n.NextAttempt, n.Sent, n.Acked = fromNull(next), fromNull(sent), fromNull(acked)
	n.Created, n.Expires = fromMS(created), fromMS(expires)
	return &n, nil
}

// Notification returns the outbox row for a release GUID, or nil.
func (s ops) Notification(ctx context.Context, guid string) (*Notification, error) {
	n, err := scanNotification(s.q.QueryRowContext(ctx, `SELECT `+notificationColumns+` FROM notifications WHERE guid = ?`, guid))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return n, err
}

// Enqueue adds a queued alert, due at once. A release is enqueued once:
// an existing row is left alone and Enqueue reports false.
func (s ops) Enqueue(ctx context.Context, n *Notification) (bool, error) {
	res, err := s.q.ExecContext(ctx, `INSERT INTO notifications (guid, repo, tag, channel, reason, payload, state,
		next_attempt_at, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (guid) DO NOTHING`,
		n.GUID, n.Repo, n.Tag, n.Channel, n.Reason, n.Payload, AlertQueued, toMS(n.Created), toMS(n.Created), toMS(n.Expires))
	if err != nil {
		return false, err
	}
	c, err := res.RowsAffected()
	return c > 0, err
}

// Rebuild replaces an unsent alert's reason, payload and window after the
// release escalated again (§3.4): a cancelled row returns to queued, due at
// once; a sending row has its cancel request cleared, so a failed POST
// re-queues it. Sent and expired rows are never touched. It reports
// whether the row is now queued or sending.
func (s ops) Rebuild(ctx context.Context, guid, reason, payload string, expires, now time.Time) (bool, error) {
	res, err := s.q.ExecContext(ctx, `UPDATE notifications SET reason = ?, payload = ?, expires_at = ?, cancel_requested = 0,
		next_attempt_at = CASE WHEN state = ? THEN ? ELSE next_attempt_at END,
		state = CASE WHEN state = ? THEN ? ELSE state END
		WHERE guid = ? AND state IN (?, ?, ?)`,
		reason, payload, toMS(expires), AlertCancelled, toMS(now), AlertCancelled, AlertQueued,
		guid, AlertQueued, AlertSending, AlertCancelled)
	if err != nil {
		return false, err
	}
	c, err := res.RowsAffected()
	return c > 0, err
}

// Cancel withdraws an unsent alert after de-escalation (§3.4): a queued row
// is cancelled; a sending row gets cancel_requested, and the claim's
// outcome decides it.
func (s ops) Cancel(ctx context.Context, guid string) error {
	_, err := s.q.ExecContext(ctx, `UPDATE notifications SET
		cancel_requested = CASE WHEN state = ? THEN 1 ELSE cancel_requested END,
		state = CASE WHEN state = ? THEN ? ELSE state END
		WHERE guid = ? AND state IN (?, ?)`,
		AlertSending, AlertQueued, AlertCancelled, guid, AlertQueued, AlertSending)
	return err
}

// ResetSending returns rows left sending by a crash to queued (§3.4,
// crash safety). It runs before the outbox worker starts.
func (s ops) ResetSending(ctx context.Context) (int64, error) {
	res, err := s.q.ExecContext(ctx, `UPDATE notifications SET state = CASE WHEN cancel_requested = 1 THEN ? ELSE ? END,
		cancel_requested = 0 WHERE state = ?`, AlertCancelled, AlertQueued, AlertSending)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Expire marks queued alerts whose window has ended as expired and
// returns them.
func (s *Store) Expire(ctx context.Context, now time.Time) ([]*Notification, error) {
	var out []*Notification
	err := s.Tx(ctx, func(tx *Tx) error {
		rows, err := tx.q.QueryContext(ctx, `SELECT `+notificationColumns+` FROM notifications
			WHERE state = ? AND expires_at <= ? ORDER BY expires_at, guid`, AlertQueued, toMS(now))
		if err != nil {
			return err
		}
		for rows.Next() {
			n, err := scanNotification(rows)
			if err != nil {
				rows.Close()
				return err
			}
			out = append(out, n)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, n := range out {
			if _, err := tx.q.ExecContext(ctx, `UPDATE notifications SET state = ? WHERE guid = ? AND state = ?`,
				AlertExpired, n.GUID, AlertQueued); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// Claim atomically moves the next due queued alert to sending and returns
// it, or nil if none is due (§3.4). An alert waits while its release's
// urgency awaits a recompute (marked dirty, or notes not yet rescanned for
// advisory IDs), while the release is confirmed deleted, and while its
// repo is held.
func (s *Store) Claim(ctx context.Context, now time.Time) (*Notification, error) {
	var n *Notification
	err := s.Tx(ctx, func(tx *Tx) error {
		var err error
		n, err = scanNotification(tx.q.QueryRowContext(ctx, `SELECT `+prefixedWith("n.", notificationColumns)+` FROM notifications n
			JOIN releases r ON r.guid = n.guid
			LEFT JOIN repo_state rs ON rs.repo = n.repo
			WHERE n.state = ? AND n.next_attempt_at <= ? AND n.expires_at > ? AND `+releaseSendable+`
			ORDER BY n.next_attempt_at, n.created_at, n.guid LIMIT 1`, AlertQueued, toMS(now), toMS(now)))
		if errors.Is(err, sql.ErrNoRows) {
			n = nil
			return nil
		}
		if err != nil {
			return err
		}
		_, err = tx.q.ExecContext(ctx, `UPDATE notifications SET state = ? WHERE guid = ? AND state = ?`, AlertSending, n.GUID, AlertQueued)
		n.State = AlertSending
		return err
	})
	return n, err
}

// Sendable re-reads a claimed alert immediately before its POST (§3.4): it
// returns the row if it is still sending with no cancel request, its repo
// isn't held (another check may have found it private since the worker's
// own), and its release still passes Claim's conditions (no pending
// recompute, notes scanned, not deleted); else nil.
func (s ops) Sendable(ctx context.Context, guid string) (*Notification, error) {
	n, err := s.Notification(ctx, guid)
	if err != nil || n == nil || n.State != AlertSending || n.CancelRequested {
		return nil, err
	}
	var ok bool
	err = s.q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM releases r LEFT JOIN repo_state rs ON rs.repo = r.repo
		WHERE r.guid = ? AND `+releaseSendable+`)`, guid).Scan(&ok)
	if err != nil || !ok {
		return nil, err
	}
	return n, nil
}

// releaseSendable is what a release (r) and its repo (rs) must satisfy for
// its alert to be claimed or sent.
const releaseSendable = `COALESCE(rs.held, 0) = 0 AND r.urgency_dirty = 0 AND r.advisories_hash = r.notes_hash AND r.missing_since IS NULL`

// Finish ends a claim in exactly one state (§3.4): sent on a 2xx; else
// cancelled if a cancel was requested; else queued again for next, with
// the attempt counted. It returns the final state ("" if the row wasn't
// sending) and whether a cancel had been requested.
func (s ops) Finish(ctx context.Context, guid string, sent bool, next time.Time, errText string, now time.Time) (string, bool, error) {
	n, err := s.Notification(ctx, guid)
	if err != nil || n == nil || n.State != AlertSending {
		return "", false, err
	}
	var q string
	var args []any
	switch {
	case sent:
		q, args = `UPDATE notifications SET state = ?, sent_at = ?, cancel_requested = 0, last_error = '' WHERE guid = ? AND state = ?`,
			[]any{AlertSent, toMS(now), guid, AlertSending}
	case n.CancelRequested:
		q, args = `UPDATE notifications SET state = ?, cancel_requested = 0, last_error = ? WHERE guid = ? AND state = ?`,
			[]any{AlertCancelled, errText, guid, AlertSending}
	default:
		q, args = `UPDATE notifications SET state = ?, attempts = attempts + 1, next_attempt_at = ?, last_error = ?
			WHERE guid = ? AND state = ?`, []any{AlertQueued, toMS(next), errText, guid, AlertSending}
	}
	if _, err := s.q.ExecContext(ctx, q, args...); err != nil {
		return "", false, err
	}
	state := map[bool]string{true: AlertSent, false: AlertQueued}[sent]
	if !sent && n.CancelRequested {
		state = AlertCancelled
	}
	return state, n.CancelRequested, nil
}

// ExpiredUnacked counts expired alerts not yet acknowledged: /healthz
// reports them as degraded (§3.4).
func (s *Store) ExpiredUnacked(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notifications WHERE state = ? AND acked_at IS NULL`, AlertExpired).Scan(&n)
	return n, err
}

// AckExpired acknowledges every expired alert and returns them.
func (s *Store) AckExpired(ctx context.Context, now time.Time) ([]*Notification, error) {
	var out []*Notification
	err := s.Tx(ctx, func(tx *Tx) error {
		rows, err := tx.q.QueryContext(ctx, `SELECT `+notificationColumns+` FROM notifications
			WHERE state = ? AND acked_at IS NULL ORDER BY expires_at, guid`, AlertExpired)
		if err != nil {
			return err
		}
		for rows.Next() {
			n, err := scanNotification(rows)
			if err != nil {
				rows.Close()
				return err
			}
			out = append(out, n)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		_, err = tx.q.ExecContext(ctx, `UPDATE notifications SET acked_at = ? WHERE state = ? AND acked_at IS NULL`, toMS(now), AlertExpired)
		return err
	})
	return out, err
}
