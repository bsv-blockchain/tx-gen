package safe

import (
	"bytes"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRunnerRunsFunction(t *testing.T) {
	var wg sync.WaitGroup
	var ran atomic.Bool
	Runner{}.Go(&wg, "worker", func() { ran.Store(true) })
	wg.Wait()
	assert.True(t, ran.Load())
}

func TestRunnerRecoversPanic(t *testing.T) {
	var logs bytes.Buffer
	var reported atomic.Value
	runner := Runner{
		Logger:  slog.New(slog.NewTextHandler(&logs, nil)),
		OnPanic: func(name string) { reported.Store(name) },
	}

	var wg sync.WaitGroup
	runner.Go(&wg, "chain", func() { panic("boom") })
	wg.Wait()

	assert.Equal(t, "chain", reported.Load())
	assert.Contains(t, logs.String(), "recovered panic in goroutine")
	assert.Contains(t, logs.String(), "goroutine=chain")
	assert.Contains(t, logs.String(), "boom")
}

func TestZeroRunnerRecoversPanic(t *testing.T) {
	var wg sync.WaitGroup
	var after atomic.Bool
	Runner{}.Go(&wg, "worker", func() { panic("boom") })
	Runner{}.Go(&wg, "worker", func() { after.Store(true) })
	wg.Wait() // reaching here means the panic did not crash the test binary
	assert.True(t, after.Load(), "other goroutines keep running")
}
