// Package version resolves this library's own module version for use as the
// OpenTelemetry instrumentation version.
package version

import (
	"runtime/debug"
	"sync"
)

// ModulePath is the import path reported as the instrumentation scope name.
const ModulePath = "github.com/stakater/operator-utils/telemetry-web"

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
