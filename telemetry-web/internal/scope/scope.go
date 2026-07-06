// Package scope holds process-wide telemetry identity (the service name) shared
// between Init and the logging handler, kept off the public API.
package scope

import "sync"

var (
	mu   sync.RWMutex
	name string
)

// Set records the service name. Called once by telemetry.Init.
func Set(n string) {
	mu.Lock()
	name = n
	mu.Unlock()
}

// ServiceName returns the service name set by Init, or "" if unset.
func ServiceName() string {
	mu.RLock()
	defer mu.RUnlock()
	return name
}
