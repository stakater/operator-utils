package telemetry

import "testing"

// Config.OTLPEndpoint accepts both spec-style URLs and bare host:port; the
// exporter option differs (WithEndpointURL vs WithEndpoint), so the split must
// classify correctly.
func TestHasScheme(t *testing.T) {
	cases := map[string]bool{
		"http://collector:4317":     true,
		"https://collector.example": true,
		"localhost:4317":            false,
		"collector.ns.svc:4317":     false,
		"":                          false,
	}
	for ep, want := range cases {
		if got := hasScheme(ep); got != want {
			t.Errorf("hasScheme(%q) = %v, want %v", ep, got, want)
		}
	}
}
