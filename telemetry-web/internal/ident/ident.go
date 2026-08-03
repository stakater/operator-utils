// Package ident holds the two pieces of identity the library's packages share:
// the service name the consumer configured, and this library's own module
// version. Both are process-wide, both are written once during setup and read on
// hot paths, and neither belongs on the public API.
//
// It is a package rather than fields on some type because telemetry.Init writes
// the service name and the logging handler reads it, and logging cannot import
// telemetry: telemetry already imports logging. This breaks that cycle.
package ident

import (
	"runtime/debug"
	"sync"
	"sync/atomic"
)

// ModulePath is the import path reported as the instrumentation scope name.
const ModulePath = "github.com/stakater/operator-utils/telemetry-web"

// Written by Init and by its rollback, read on every log record — an atomic keeps
// the lock off the logging hot path.
var serviceName atomic.Pointer[string]

// SetServiceName records the service name. Called by telemetry.Init, and again
// with "" when Init fails or its shutdown runs.
func SetServiceName(n string) { serviceName.Store(&n) }

// ServiceName returns the service name set during setup, or "" if unset.
func ServiceName() string {
	if n := serviceName.Load(); n != nil {
		return *n
	}
	return ""
}

// Version returns the module version recorded in the consuming binary's build
// info, or "" when it cannot be resolved. Reading it from build info keeps it
// correct without a hand-bumped constant.
//
// Deps covers the normal case, this library imported by a service. When it is the
// main module instead (its own tests, or a build from this repo) it is absent
// from Deps, so Main.Version is the fallback — usually "(devel)", which is still
// more useful on a span than an empty scope version.
var Version = sync.OnceValue(func() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, dep := range info.Deps {
		if dep.Path == ModulePath {
			return dep.Version
		}
	}
	if info.Main.Path == ModulePath {
		return info.Main.Version
	}
	return ""
})
