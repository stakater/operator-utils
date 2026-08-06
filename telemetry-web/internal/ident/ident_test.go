package ident

import (
	"strings"
	"sync"
	"testing"
)

func TestModulePathIsTheImportPath(t *testing.T) {
	const want = "github.com/stakater/operator-utils/telemetry-web"
	if ModulePath != want {
		t.Errorf("ModulePath = %q, want %q", ModulePath, want)
	}
}

func TestSetThenRead(t *testing.T) {
	SetServiceName("svc-a")
	if got := ServiceName(); got != "svc-a" {
		t.Errorf("ServiceName() = %q, want %q", got, "svc-a")
	}
	SetServiceName("svc-b")
	if got := ServiceName(); got != "svc-b" {
		t.Errorf("ServiceName() = %q, want %q", got, "svc-b")
	}
}

// Clearing is a real path, not a curiosity: telemetry.retire sets "" so
// post-shutdown log records stop carrying a stale service.name.
//
// This replaces a test that asserted the never-set case and t.Skipf'd when
// another test had already set a name — under -shuffle that skipped about half
// the time, asserting nothing.
func TestClearedServiceNameReadsEmpty(t *testing.T) {
	SetServiceName("svc")
	SetServiceName("")
	if got := ServiceName(); got != "" {
		t.Errorf("ServiceName() = %q after clearing, want %q", got, "")
	}
}

// SetServiceName runs at Init and on shutdown, but ServiceName is read on every
// log record, so the two must be safe together under -race.
func TestConcurrentSetAndRead(t *testing.T) {
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(2)
		go func() { defer wg.Done(); SetServiceName("concurrent") }()
		go func() { defer wg.Done(); _ = ServiceName() }()
	}
	wg.Wait()
}

// Version reads the consuming binary's build info. Under `go test` this module IS
// the main module, so it is absent from Deps and the Main.Version fallback
// answers — "(devel)" for an untagged build here. A consumer's binary gets the
// resolved module version instead. Either is fine; "" is fine too, since the OTel
// API accepts an empty instrumentation version. What must not happen is a panic
// or a garbage value.
func TestVersionIsEmptyOrASemverPseudoVersion(t *testing.T) {
	got := Version()
	switch {
	case got == "", got == "(devel)":
		return
	case !strings.HasPrefix(got, "v"):
		t.Errorf("Version() = %q, want \"\", \"(devel)\", or a v-prefixed module version", got)
	}
}

// The main-module fallback must actually fire under `go test`, where this module
// is the main module. Without it every span and metric from an in-repo build
// carries an empty scope version.
func TestVersionFallsBackToMainModule(t *testing.T) {
	if Version() == "" {
		t.Error("Version() = \"\", want the Main.Version fallback to resolve when this is the main module")
	}
}

// Cached via sync.OnceValue, so repeated reads on the metric setup path are free
// and consistent.
func TestVersionIsStable(t *testing.T) {
	if first, second := Version(), Version(); first != second {
		t.Errorf("Version() not stable: %q then %q", first, second)
	}
}
