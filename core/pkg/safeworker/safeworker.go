// Package safeworker contains panics in background work: it logs one failing
// tick of a long-running worker instead of letting it crash the whole process.
package safeworker

import (
	"fmt"
	"runtime/debug"

	"github.com/alpkeskin/rota/core/pkg/logger"
)

// Call runs fn and recovers a panic from it, logging the worker name, the
// panic value and the stack at error level. It does not re-raise the panic, so
// the calling loop carries on with its next tick.
//
// fn must release its own locks with defer: Call cannot unlock a mutex that fn
// held when it panicked.
func Call(log *logger.Logger, worker string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("background worker panicked",
				"worker", worker,
				"panic", fmt.Sprint(r),
				"stack", string(debug.Stack()),
			)
		}
	}()
	fn()
}

// Go runs fn in a new goroutine under Call.
func Go(log *logger.Logger, worker string, fn func()) {
	go Call(log, worker, fn)
}
