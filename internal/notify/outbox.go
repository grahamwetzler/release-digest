package notify

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"release-digest/internal/github"
	"release-digest/internal/store"
)

// Outbox delivery (§3.4).
const (
	WakeInterval = 30 * time.Second
	PerWake      = 10
	RetryBase    = time.Minute
	RetryMax     = time.Hour
	FinalAttempt = 2 * time.Minute // before expiry
	CheckTimeout = 10 * time.Second
)

// Outbox is the worker that delivers queued alerts, independently of the
// scheduler's ticks (§3.4). Delivery is at-least-once: a row is marked sent
// only after a 2xx, so a POST whose response is lost is sent again.
type Outbox struct {
	Store *store.Store
	// Check is the just-in-time public check before a POST.
	Check func(ctx context.Context, repo string) (github.Visibility, error)
	Ntfy  *Ntfy
	Log   *slog.Logger
	Now   func() time.Time // replaceable in tests

	kick chan struct{}
}

func (o *Outbox) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// Kick wakes the worker at once. It never blocks, and does nothing before
// Recover.
func (o *Outbox) Kick() {
	if o.kick == nil {
		return
	}
	select {
	case o.kick <- struct{}{}:
	default:
	}
}

// Recover returns rows left sending by a crash to queued (§3.4). Call it
// before Run starts and before anything can Kick.
func (o *Outbox) Recover(ctx context.Context) error {
	o.kick = make(chan struct{}, 1)
	n, err := o.Store.ResetSending(ctx)
	if n > 0 {
		o.Log.Warn("alerts left sending by the last run are queued again; one may be delivered twice", "alerts", n)
	}
	return err
}

// Run delivers until ctx ends: every WakeInterval, or at once when kicked.
// Recover must have run.
func (o *Outbox) Run(ctx context.Context) {
	t := time.NewTicker(WakeInterval)
	defer t.Stop()
	for {
		if err := o.Drain(ctx); err != nil && ctx.Err() == nil {
			o.Log.Error("alert outbox", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-o.kick:
		}
	}
}

// Drain is one wake: it expires alerts past their window, then delivers
// up to PerWake due alerts. Only store failures are returned.
func (o *Outbox) Drain(ctx context.Context) error {
	expired, err := o.Store.Expire(ctx, o.now())
	if err != nil {
		return err
	}
	for _, n := range expired {
		o.Log.Error("alert expired undelivered: every attempt in its window failed; acknowledge with release-digest notify ack",
			"repo", n.Repo, "tag", n.Tag, "attempts", n.Attempts, "last_error", n.LastError)
	}
	for range PerWake {
		if ctx.Err() != nil {
			return nil
		}
		n, err := o.Store.Claim(ctx, o.now())
		if err != nil || n == nil {
			return err
		}
		if err := o.deliver(ctx, n); err != nil {
			return err
		}
	}
	return nil
}

// deliver runs one claimed alert to exactly one final state (§3.4).
func (o *Outbox) deliver(ctx context.Context, n *store.Notification) error {
	// The outcome is recorded even if shutdown cancels the attempt.
	rec := context.WithoutCancel(ctx)
	cctx, cancel := context.WithTimeout(ctx, CheckTimeout)
	vis, err := o.Check(cctx, n.Repo)
	cancel()
	if !vis.Public {
		msg := "visibility check failed"
		switch {
		case vis.Gone:
			msg = "the repo is private or not found"
			if err := o.Store.Visibility(rec, n.Repo, 0, false, true, o.now(), "alert held: "+msg); err != nil {
				return err
			}
		case err != nil:
			msg += ": " + err.Error()
		}
		return o.finish(rec, n, nil, msg)
	}
	cur, err := o.Store.Sendable(rec, n.GUID)
	if err != nil {
		return err
	}
	if cur == nil {
		return o.finish(rec, n, nil, "cancelled before sending")
	}
	p, err := DecodePayload(cur.Payload)
	if err != nil {
		return o.finish(rec, n, nil, "stored payload: "+err.Error())
	}
	serr := o.Ntfy.Send(ctx, p)
	if serr == nil {
		if cur.Payload != n.Payload {
			o.Log.Info("alert rebuilt while it was being sent; the earlier version went out", "repo", n.Repo, "tag", n.Tag)
		}
		return o.finish(rec, n, serr, "")
	}
	var se *SendError
	if errors.As(serr, &se) && se.Ambiguous {
		o.Log.Warn("alert delivery may have succeeded; a retry can deliver it twice", "repo", n.Repo, "tag", n.Tag, "err", serr)
	}
	return o.finish(rec, n, serr, serr.Error())
}

// finish records a claim's outcome: sent if failure is "" and sendErr is
// nil, else cancelled or re-queued with backoff.
func (o *Outbox) finish(ctx context.Context, n *store.Notification, sendErr error, failure string) error {
	now := o.now()
	sent := failure == "" && sendErr == nil
	next := o.retryAt(n, now)
	var state string
	var cancelled bool
	err := o.Store.Tx(ctx, func(tx *store.Tx) (err error) {
		state, cancelled, err = tx.Finish(ctx, n.GUID, sent, next, failure, now)
		return err
	})
	if err != nil {
		return err
	}
	switch state {
	case store.AlertSent:
		o.Log.Info("alert sent", "repo", n.Repo, "tag", n.Tag, "reason", n.Reason)
		if cancelled {
			o.Log.Warn("alert sent although it was cancelled during the request", "repo", n.Repo, "tag", n.Tag)
		}
	case store.AlertCancelled:
		o.Log.Info("alert cancelled", "repo", n.Repo, "tag", n.Tag, "why", failure)
	case store.AlertQueued:
		o.Log.Warn("alert not delivered; retrying", "repo", n.Repo, "tag", n.Tag, "retry_at", next, "why", failure)
	}
	return nil
}

// retryAt is when a failed attempt is retried (§3.4): RetryBase doubling
// to RetryMax, but never past a final attempt FinalAttempt before expiry.
// After the final attempt it is the expiry itself, when the alert expires.
func (o *Outbox) retryAt(n *store.Notification, now time.Time) time.Time {
	next := now.Add(min(RetryBase<<min(n.Attempts, 10), RetryMax))
	final := n.Expires.Add(-FinalAttempt)
	if next.Before(final) {
		return next
	}
	if now.Before(final) {
		return final
	}
	return n.Expires
}
