package version

import (
	"strings"
	"testing"
)

func TestModulePathIsTheImportPath(t *testing.T) {
	const want = "github.com/stakater/operator-utils/telemetry-web"
	if ModulePath != want {
		t.Errorf("ModulePath = %q, want %q", ModulePath, want)
	}
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
