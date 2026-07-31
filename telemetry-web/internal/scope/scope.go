// Package scope holds process-wide telemetry identity (the service name) shared
// between Init and the logging handler, kept off the public API.
package scope

import "sync/atomic"

// Written once by Init, read on every log record — an atomic keeps the lock
// off the logging hot path.
var name atomic.Pointer[string]

// Set records the service name. Called once by telemetry.Init.
func Set(n string) { name.Store(&n) }

// ServiceName returns the service name set by Init, or "" if unset.
func ServiceName() string {
	if n := name.Load(); n != nil {
		return *n
	}
	return ""
}
