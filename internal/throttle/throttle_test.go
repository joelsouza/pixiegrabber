package throttle

import (
	"context"
	"testing"
	"time"
)

// waitElapsed measures how long a Wait call takes.
func waitElapsed(t *testing.T, l *Limiter, ctx context.Context) time.Duration {
	t.Helper()
	start := time.Now()
	if err := l.Wait(ctx); err != nil {
		t.Fatalf("Wait returned error: %v", err)
	}
	return time.Since(start)
}

func TestWaitZeroIntervalReturnsImmediately(t *testing.T) {
	l := New(0)
	start := time.Now()
	if err := l.Wait(context.Background()); err != nil {
		t.Fatalf("Wait returned error: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= 10*time.Millisecond {
		t.Fatalf("Wait with zero interval took %v, want < 10ms", elapsed)
	}
}

func TestWaitSpacing(t *testing.T) {
	const interval = 50 * time.Millisecond
	const n = 4
	l := New(interval)

	// First call should be near-immediate (token available).
	waitElapsed(t, l, context.Background())

	start := time.Now()
	for i := 0; i < n; i++ {
		waitElapsed(t, l, context.Background())
	}

	// n sequential waits after the first must take at least (n-1)*interval.
	min := time.Duration(n-1) * interval
	elapsed := time.Since(start)
	if elapsed < min {
		t.Fatalf("n=%d sequential waits took %v, want >= %v", n, elapsed, min)
	}
}

func TestWaitJitterBounds(t *testing.T) {
	const interval = 50 * time.Millisecond
	l := New(interval)

	// Drain the initial token.
	waitElapsed(t, l, context.Background())

	// Each subsequent wait must be >= interval (minus a small clock-granularity
	// tolerance) and < interval + jitter cap + slack.
	const slack = 25 * time.Millisecond
	// rate.Limiter has inherent clock imprecision (~±20%), so allow a
	// generous lower tolerance.
	const lowerTol = interval / 4
	lower := interval - lowerTol
	upper := interval + maxJitter + slack
	for i := 0; i < 5; i++ {
		elapsed := waitElapsed(t, l, context.Background())
		if elapsed < lower {
			t.Fatalf("wait took %v, want >= ~interval %v", elapsed, lower)
		}
		if elapsed >= upper {
			t.Fatalf("wait took %v, want < %v", elapsed, upper)
		}
	}
}

func TestWaitContextCancellation(t *testing.T) {
	const interval = 50 * time.Millisecond
	l := New(interval)

	// Drain the initial token so the next Wait must block.
	waitElapsed(t, l, context.Background())

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before waiting

	if err := l.Wait(ctx); err != context.Canceled {
		t.Fatalf("Wait returned %v, want context.Canceled", err)
	}
}

func TestBackoffMonotonic(t *testing.T) {
	prev := time.Duration(0)
	for attempt := 0; attempt < 5; attempt++ {
		d := Backoff(attempt)
		if d <= prev {
			t.Fatalf("Backoff(%d)=%v not greater than previous %v", attempt, d, prev)
		}
		prev = d
	}
}

func TestBackoffJitterBounds(t *testing.T) {
	for attempt := 0; attempt < 5; attempt++ {
		base := time.Second << uint(attempt)
		for i := 0; i < 20; i++ {
			d := Backoff(attempt)
			if d < base {
				t.Fatalf("Backoff(%d)=%v, want >= base %v", attempt, d, base)
			}
			if d >= base+base/2 {
				t.Fatalf("Backoff(%d)=%v, want < base+jitter %v", attempt, d, base+base/2)
			}
		}
	}
}
