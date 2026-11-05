package backoff

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRetryGrowsAndCaps(t *testing.T) {
	tests := []struct {
		attempt int
		base    time.Duration
	}{
		{attempt: -3, base: 100 * time.Millisecond},
		{attempt: 0, base: 100 * time.Millisecond},
		{attempt: 1, base: 100 * time.Millisecond},
		{attempt: 2, base: 200 * time.Millisecond},
		{attempt: 3, base: 400 * time.Millisecond},
		{attempt: 6, base: 3200 * time.Millisecond},
		{attempt: 7, base: retryCap},
		{attempt: 64, base: retryCap},
		{attempt: math.MaxInt32, base: retryCap},
	}
	for _, tc := range tests {
		for range 20 {
			got := Retry(tc.attempt)
			assert.GreaterOrEqual(t, got, tc.base, "attempt %d", tc.attempt)
			assert.LessOrEqual(t, got, tc.base+tc.base/2, "attempt %d", tc.attempt)
		}
	}
}

func TestReconnect(t *testing.T) {
	assert.Equal(t, time.Duration(0), Reconnect(0, 0, 1), "no delays configured")

	for range 20 {
		got := Reconnect(time.Second, 30*time.Second, 1)
		assert.GreaterOrEqual(t, got, time.Second)
		assert.LessOrEqual(t, got, 1500*time.Millisecond)

		got = Reconnect(time.Second, 30*time.Second, 3)
		assert.GreaterOrEqual(t, got, 4*time.Second)
		assert.LessOrEqual(t, got, 6*time.Second)

		got = Reconnect(time.Second, 30*time.Second, 100)
		assert.GreaterOrEqual(t, got, 30*time.Second)
		assert.LessOrEqual(t, got, 45*time.Second)

		got = Reconnect(2*time.Second, 0, 5)
		assert.GreaterOrEqual(t, got, 2*time.Second, "max falls back to min")
		assert.LessOrEqual(t, got, 3*time.Second)
	}
}

func TestJitter(t *testing.T) {
	assert.Equal(t, time.Duration(0), Jitter(0))
	assert.Equal(t, time.Duration(0), Jitter(-time.Second))
	for range 100 {
		got := Jitter(10 * time.Millisecond)
		assert.GreaterOrEqual(t, got, time.Duration(0))
		assert.LessOrEqual(t, got, 10*time.Millisecond)
	}
	assert.GreaterOrEqual(t, Jitter(math.MaxInt64), time.Duration(0))
}

func TestSleep(t *testing.T) {
	require.NoError(t, Sleep(t.Context(), time.Millisecond))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	start := time.Now()
	require.ErrorIs(t, Sleep(ctx, time.Hour), context.Canceled)
	assert.Less(t, time.Since(start), time.Second)
}
