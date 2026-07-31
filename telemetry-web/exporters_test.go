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

// A bare host:port containing "://" later in the string is not URL-form;
// url.Parse distinguishes them, strings.Contains would not.
func TestHasSchemeRejectsEmbeddedSeparator(t *testing.T) {
	cases := map[string]bool{
		"host:4317/x://y":       false,
		"grpc://collector:4317": true,
		"//collector:4317":      false, // scheme-relative, no scheme
		// Both have a scheme followed by "://", so both are URL-form. That hands
		// them to WithEndpointURL, which reports a real error, instead of
		// WithEndpoint silently accepting them as a host:port.
		"http://":                 true,
		"unix:///var/run/otel.sk": true,
	}
	for ep, want := range cases {
		if got := hasScheme(ep); got != want {
			t.Errorf("hasScheme(%q) = %v, want %v", ep, got, want)
		}
	}
}
