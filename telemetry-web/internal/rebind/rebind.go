// Package rebind lets packages that cache OpenTelemetry instruments rebuild them
// when Init installs new global providers.
//
// otel's global meter delegates to the FIRST real MeterProvider and never
// re-delegates, so an instrument created at or before that moment stays bound to
// it for the life of the process. Swapping the global later — on shutdown, or a
// second Init — is invisible to it, and it goes on writing into a retired
// pipeline. Rebuilding explicitly is the only fix.
//
// Internal on purpose: consumers call telemetry.Init, which triggers this.
package rebind

import (
	"sync"

	"github.com/stakater/operator-utils/telemetry-web/logging"
)

var (
	mu  sync.Mutex
	fns []func()
)

// On registers f to run on every Notify. Call it from a package init function.
func On(f func()) {
	mu.Lock()
	fns = append(fns, f)
	mu.Unlock()
}

// Notify runs every registered rebuild. telemetry.Init calls it once the global
// providers are in place, and the shutdown closure again once they are retired.
//
// Hooks run outside the lock so one may register another without deadlocking,
// and each is isolated: a panicking hook is logged and skipped rather than
// unwinding out of Init. Telemetry wiring must not crash startup.
func Notify() {
	mu.Lock()
	snapshot := make([]func(), len(fns))
	copy(snapshot, fns)
	mu.Unlock()

	for _, f := range snapshot {
		runHook(f)
	}
}

func runHook(f func()) {
	defer func() {
		if p := recover(); p != nil {
			logging.Logger().Error("telemetry: instrument rebind hook panicked; its metrics may be stale", "panic", p)
		}
	}()
	f()
}
