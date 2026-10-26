package telemetry

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

func TestValidEndpoint(t *testing.T) {
	tests := []struct {
		endpoint string
		valid    bool
	}{
		{"", true},
		{"localhost:4317", true},
		{"otel-collector.ns.svc:4317", true},
		{"[::1]:4317", true},
		{"http://collector:4318", true},
		{"https://collector.example:443/v1", true},
		{"http://a b c", false},
		{"http://bad host:4318", false},
		{"collector", false}, // bare host needs a port
	}
	for _, tt := range tests {
		t.Run(tt.endpoint, func(t *testing.T) {
			if err := validEndpoint(tt.endpoint); (err == nil) != tt.valid {
				t.Errorf("validEndpoint(%q) = %v, want valid=%v", tt.endpoint, err, tt.valid)
			}
		})
	}
}

// The check runs before Init changes anything global, with and without an
// injected MeterProvider.
func TestInitRejectsAMalformedEndpointBeforeChangingGlobals(t *testing.T) {
	owner, _ := newMeterProvider(t)
	for _, mp := range []metric.MeterProvider{nil, owner} {
		tp := otel.GetTracerProvider()
		shutdown, err := Init(context.Background(), Config{
			ServiceName:   "svc",
			OTLPEndpoint:  "http://a b c",
			MeterProvider: mp,
		})
		if err == nil || !strings.Contains(err.Error(), "invalid OTLP endpoint") {
			if err == nil {
				_ = shutdown(context.Background())
			}
			t.Fatalf("mp=%T: Init error = %v, want an invalid OTLP endpoint error", mp, err)
		}
		if got := otel.GetTracerProvider(); got != tp {
			t.Errorf("mp=%T: global TracerProvider changed to %T", mp, got)
		}
	}

	// A rejected Init must not leave the guard latched.
	shutdown := initOnce(t)
	_ = shutdown(bounded(t))
}
