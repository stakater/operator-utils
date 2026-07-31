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

// Version reads the consuming binary's build info. Under `go test` this module
// IS the main module, so it appears in no Deps entry and "" is the correct
// answer — the OTel API accepts an empty instrumentation version. What must not
// happen is a panic or a garbage value.
func TestVersionIsEmptyOrASemverPseudoVersion(t *testing.T) {
	got := Version()
	if got == "" {
		return
	}
	if !strings.HasPrefix(got, "v") {
		t.Errorf("Version() = %q, want \"\" or a v-prefixed module version", got)
	}
}

// Cached via sync.OnceValue, so repeated reads on the metric setup path are free
// and consistent.
func TestVersionIsStable(t *testing.T) {
	if first, second := Version(), Version(); first != second {
		t.Errorf("Version() not stable: %q then %q", first, second)
	}
}
