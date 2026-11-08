package store

import (
	"context"
	"testing"
	"time"
)

func TestEscalateCrosscheck(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	for _, state := range []string{AlertQueued, AlertSending, AlertSent, AlertCancelled, AlertExpired} {
		for _, reason := range []string{"llm", "advisory:GHSA-2345-6789-cfgh"} {
			t.Run(state+"/"+reason, func(t *testing.T) {
				s, _ := open(t)
				if _, err := s.InsertRelease(ctx, &Release{GUID: "g", Repo: "o/r", Tag: "v1", Kind: "release",
					State: StateFallback, FirstSeen: now, Origin: OriginLive}); err != nil {
					t.Fatal(err)
				}
				if ok, err := s.Enqueue(ctx, &Notification{GUID: "g", Repo: "o/r", Tag: "v1", Channel: "ntfy",
					Reason: "crosscheck", Payload: "unconfirmed", Created: now.Add(-time.Hour), Expires: now}); err != nil || !ok {
					t.Fatalf("enqueue: %v, %v", ok, err)
				}
				if _, err := s.q.ExecContext(ctx, `UPDATE notifications SET state = ?, attempts = 3, cancel_requested = 1,
					next_attempt_at = ?, sent_at = ?, acked_at = ?, last_error = 'old' WHERE guid = 'g'`,
					state, toMS(now.Add(time.Hour)), toMS(now), toMS(now)); err != nil {
					t.Fatal(err)
				}
				expires := now.Add(time.Hour)
				if ok, err := s.EscalateCrosscheck(ctx, "g", reason, "urgent", expires, now); err != nil || !ok {
					t.Fatalf("escalate: %v, %v", ok, err)
				}
				n, err := s.Notification(ctx, "g")
				wantState := AlertQueued
				if state == AlertSending {
					wantState = AlertSending
				}
				if err != nil || n == nil {
					t.Fatalf("notification: %+v, %v", n, err)
				}
				if n.State != wantState || n.Reason != reason || n.Payload != "urgent" || !n.Expires.Equal(expires) ||
					!n.NextAttempt.Equal(now) || n.Attempts != 0 || n.CancelRequested || !n.Sent.IsZero() || !n.Acked.IsZero() || n.LastError != "" {
					t.Fatalf("escalated row: %+v", n)
				}
				if ok, err := s.EscalateCrosscheck(ctx, "g", reason, "duplicate", expires, now); err != nil || ok {
					t.Fatalf("duplicate escalation: %v, %v", ok, err)
				}
				if state == AlertSending {
					if err := s.Tx(ctx, func(tx *Tx) error {
						got, _, err := tx.Finish(ctx, "g", "crosscheck", true, now.Add(time.Minute), "", now)
						if err == nil && got != AlertQueued {
							t.Errorf("old POST finished as %s, want queued", got)
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
					n, err = s.Notification(ctx, "g")
					if err != nil || n == nil || n.State != AlertQueued || n.Payload != "urgent" || !n.Sent.IsZero() || !n.NextAttempt.Equal(now) {
						t.Fatalf("after old POST: %+v, %v", n, err)
					}
				}
			})
		}
	}
}
