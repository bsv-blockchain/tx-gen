// Package backoff provides the retry, reconnect, and jitter delays shared by
// the network clients and the engine, plus a context-aware sleep.
package backoff

import (
	"context"
	"math"
	"math/rand/v2"
	"time"
)

const (
	// retryBase is the delay before the first retry.
	retryBase = 100 * time.Millisecond

	// retryCap is the largest delay Retry returns before jitter is added.
	retryCap = 5 * time.Second
)

// Retry returns the delay to wait before retry number attempt (1-based).
//
// The delay doubles from 100ms per attempt, is capped at 5s, and has up to 50%
// random jitter added so that concurrent callers do not retry in lockstep.
func Retry(attempt int) time.Duration {
	delay := exponential(retryBase, retryCap, attempt)
	return delay + Jitter(delay/2)
}

// Reconnect returns the delay to wait before reconnect number attempt
// (1-based) to a dropped stream.
//
// The delay doubles from minDelay per attempt, is capped at maxDelay, and has
// up to 50% random jitter added. A non-positive maxDelay falls back to minDelay.
func Reconnect(minDelay, maxDelay time.Duration, attempt int) time.Duration {
	if maxDelay <= 0 {
		maxDelay = minDelay
	}
	delay := exponential(minDelay, maxDelay, attempt)
	if delay <= 0 {
		return minDelay
	}
	return delay + Jitter(delay/2)
}

// Jitter returns a uniformly random duration in [0, maxJitter]. It returns 0
// when maxJitter is not positive.
func Jitter(maxJitter time.Duration) time.Duration {
	if maxJitter <= 0 {
		return 0
	}
	if maxJitter == math.MaxInt64 {
		maxJitter--
	}
	// Jitter only spreads load; it does not need a cryptographic source.
	return time.Duration(rand.Int64N(int64(maxJitter) + 1)) //nolint:gosec // non-cryptographic jitter
}

// Sleep waits for d or until ctx is done, whichever comes first. It returns
// ctx.Err() when the context ends the wait early.
func Sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// exponential returns base doubled (attempt-1) times, capped at limit. It
// never overflows, however large attempt is.
func exponential(base, limit time.Duration, attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if base <= 0 || base >= limit {
		return limit
	}
	delay := base
	for i := 1; i < attempt; i++ {
		if delay >= limit/2 {
			return limit
		}
		delay *= 2
	}
	return delay
}
