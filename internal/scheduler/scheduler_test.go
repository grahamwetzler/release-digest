package scheduler

import (
	"context"
	"testing"
	"time"
)

// fakeClock advances only when the scheduler sleeps.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func run(t *testing.T, last time.Time, jitter time.Duration, ticks int) (start time.Time, at []time.Time) {
	t.Helper()
	start = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{start}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Scheduler{
		Interval: time.Hour,
		Jitter:   jitter,
		LastTick: func(context.Context) (time.Time, error) { return last, nil },
		Tick: func(context.Context) {
			at = append(at, clock.t)
			clock.t = clock.t.Add(time.Minute) // a tick takes a minute
			if len(at) == ticks {
				cancel()
			}
		},
		Now:   clock.now,
		Sleep: func(ctx context.Context, d time.Duration) bool { clock.t = clock.t.Add(d); return ctx.Err() == nil },
		Rand:  func(n int64) int64 { return n - 1 }, // maximum jitter
	}
	if err := s.Loop(ctx); err != nil {
		t.Fatal(err)
	}
	return start, at
}

func TestFirstTickIsImmediateWhenNeverRun(t *testing.T) {
	start, at := run(t, time.Time{}, 0, 3)
	want := []time.Time{start, start.Add(time.Hour), start.Add(2 * time.Hour)}
	for i := range want {
		if !at[i].Equal(want[i]) {
			t.Errorf("tick %d at %v, want %v", i, at[i], want[i])
		}
	}
}

func TestRestartWaitsOutTheInterval(t *testing.T) {
	start := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	_, at := run(t, start.Add(-20*time.Minute), 0, 1)
	if want := start.Add(40 * time.Minute); !at[0].Equal(want) {
		t.Errorf("first tick at %v, want %v", at[0], want)
	}
	_, at = run(t, start.Add(-3*time.Hour), 0, 1)
	if !at[0].Equal(start) {
		t.Errorf("overdue tick at %v, want immediately", at[0])
	}
}

func TestJitterIsAddedAndBounded(t *testing.T) {
	start, at := run(t, time.Time{}, 5*time.Minute, 2)
	if want := start.Add(time.Hour + 5*time.Minute); !at[1].Equal(want) {
		t.Errorf("second tick at %v, want %v", at[1], want)
	}
}
