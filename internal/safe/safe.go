// Package safe starts goroutines that recover from panics, so that a failure
// in one chain or background worker never takes down the whole process.
package safe

import (
	"log/slog"
	"runtime/debug"
	"sync"
)

// Runner starts panic-safe goroutines.
//
// The zero value is ready to use: it logs recovered panics to slog.Default and
// reports them nowhere else.
type Runner struct {
	// Logger receives one error record per recovered panic, with its stack.
	// A nil Logger uses slog.Default.
	Logger *slog.Logger

	// OnPanic, when set, is called with the goroutine name after a panic is
	// recovered (for example, to increment a metric).
	OnPanic func(name string)
}

// Go runs fn on a new goroutine tracked by wg.
//
// A panic inside fn is recovered, logged with its stack trace, and reported to
// OnPanic; wg is released either way.
func (r Runner) Go(wg *sync.WaitGroup, name string, fn func()) {
	wg.Go(func() {
		defer r.handlePanic(name)
		fn()
	})
}

// handlePanic recovers a panic raised by the goroutine called name, if any.
// It must be deferred directly so that recover stops the panic.
func (r Runner) handlePanic(name string) {
	recovered := recover()
	if recovered == nil {
		return
	}
	logger := r.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Error("recovered panic in goroutine",
		"goroutine", name,
		"panic", recovered,
		"stack", string(debug.Stack()),
	)
	if r.OnPanic != nil {
		r.OnPanic(name)
	}
}
