// Package throttle provides polite rate limiting helpers for making
// requests to remote services without hammering them.
package throttle

import (
	"context"
	"math/rand"
	"time"

	"golang.org/x/time/rate"
)

// maxJitter is the upper bound applied to the random jitter added to each
// wait, so that waits never grow unboundedly for large intervals.
const maxJitter = 500 * time.Millisecond

// Limiter wraps a token-bucket rate limiter and adds small random jitter so
// that requests do not land on a rigid grid.
type Limiter struct {
	interval time.Duration
	limiter  *rate.Limiter
}

// New returns a Limiter that allows one request per interval. An interval of
// zero disables throttling entirely: Wait returns immediately.
func New(interval time.Duration) *Limiter {
	if interval <= 0 {
		return &Limiter{}
	}
	return &Limiter{
		interval: interval,
		limiter:  rate.NewLimiter(rate.Every(interval), 1),
	}
}

// Wait blocks until a token is available, then adds a small random jitter so
// requests do not land on a rigid grid. If the limiter was created with a zero
// interval, Wait returns immediately without sleeping. It returns ctx.Err() if
// the context is cancelled while waiting.
func (l *Limiter) Wait(ctx context.Context) error {
	if l.limiter == nil {
		return nil
	}
	if err := l.limiter.Wait(ctx); err != nil {
		return err
	}
	jitter := jitterFor(l.interval)
	if jitter > 0 {
		select {
		case <-time.After(jitter):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// jitterFor returns a random delay of up to 20% of interval, capped at
// maxJitter. It returns zero for a non-positive interval.
func jitterFor(interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	bound := interval / 5
	if bound > maxJitter {
		bound = maxJitter
	}
	if bound <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(bound)))
}

// Backoff returns an exponential backoff delay with jitter for the given
// attempt number (0-based). The base is 1s << attempt, plus random jitter of up
// to half the base. Callers use this to space out retries after 429/5xx
// responses.
func Backoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	base := time.Second << uint(attempt)
	jitter := time.Duration(rand.Int63n(int64(base / 2)))
	return base + jitter
}
