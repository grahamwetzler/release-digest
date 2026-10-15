// Package scheduler runs the tick (poll, summarize, render) every
// schedule.interval plus jitter, one tick at a time (PLAN.md §3).
package scheduler

import (
	"context"
	"math/rand/v2"
	"time"
)

// Scheduler runs Tick, never two at once, since the store has a single
// writer. At startup a tick is due at once if the last one is older than the
// interval; otherwise it waits out the rest, so restarts don't burst requests.
type Scheduler struct {
	Interval time.Duration
	Jitter   time.Duration // each wait gains a random 0..Jitter

	LastTick func(ctx context.Context) (time.Time, error)
	Tick     func(ctx context.Context)

	// Now, Sleep and Rand are replaceable in tests. Sleep returns false if
	// ctx ended first; Rand returns a value in [0, n).
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) bool
	Rand  func(n int64) int64
}

// Loop runs ticks as they come due until ctx ends.
func (s *Scheduler) Loop(ctx context.Context) error {
	now, sleep, rnd := s.Now, s.Sleep, s.Rand
	if now == nil {
		now = time.Now
	}
	if sleep == nil {
		sleep = Sleep
	}
	if rnd == nil {
		rnd = rand.Int64N
	}
	jitter := func() time.Duration {
		if s.Jitter <= 0 {
			return 0
		}
		return time.Duration(rnd(int64(s.Jitter) + 1))
	}
	last, err := s.LastTick(ctx)
	if ctx.Err() != nil {
		return nil // stopped, not failed, even if the read failed for it
	}
	if err != nil {
		return err
	}
	var next time.Time // zero (never ticked) is due at once
	if !last.IsZero() {
		next = last.Add(s.Interval)
	}
	for {
		if d := next.Sub(now()); d > 0 && !sleep(ctx, d) {
			return nil
		}
		if ctx.Err() != nil {
			return nil
		}
		start := now()
		s.Tick(ctx)
		next = start.Add(s.Interval + jitter())
	}
}

// Sleep waits for d, returning false if ctx ends first.
func Sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
