package nethttp

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/semconv/v1.41.0/httpconv"
)

// activeCount sums http.server.active_requests for one method.
func activeCount(t *testing.T, method string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			sum, ok := m.Data.(metricdata.Sum[int64])
			if m.Name != "http.server.active_requests" || !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				v, ok := dp.Attributes.Value(attribute.Key("http.request.method"))
				if ok && v.AsString() == method {
					total += dp.Value
				}
			}
		}
	}
	return total
}

// serveIgnoringPanic serves one GET and swallows a panic that escapes h, as
// net/http's server does.
func serveIgnoringPanic(h http.Handler, path string) {
	defer func() { _ = recover() }()
	get(h, path)
}

// The count goes up for the length of a request and back down afterwards,
// also when the handler panics, and never for a skipped path.
func TestActiveRequests(t *testing.T) {
	tests := []struct {
		name       string
		opts       []Option
		panics     bool
		wantInside int64
	}{
		{name: "counted while served", wantInside: 1},
		{name: "panic recovered", panics: true, wantInside: 1},
		{name: "panic escaping", opts: []Option{WithoutRecovery()}, panics: true, wantInside: 1},
		{name: "skipped path", opts: []Option{WithSkipPaths("/active")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := activeCount(t, http.MethodGet)
			var inside int64
			h := Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				inside = activeCount(t, http.MethodGet) - before
				if tt.panics {
					panic("kaboom")
				}
			}), tt.opts...)
			serveIgnoringPanic(h, "/active")
			if inside != tt.wantInside {
				t.Errorf("during the request: delta = %d, want %d", inside, tt.wantInside)
			}
			if after := activeCount(t, http.MethodGet) - before; after != 0 {
				t.Errorf("after the request: delta = %d, want 0", after)
			}
		})
	}
}

// Attributes are normalized as otelhttp does on its duration metric, so an
// attacker-chosen method cannot create a series per request.
func TestActiveAttrs(t *testing.T) {
	tests := []struct {
		method     string
		tls        bool
		wantMethod httpconv.RequestMethodAttr
		wantScheme string
	}{
		{http.MethodGet, false, httpconv.RequestMethodGet, "http"},
		{"get", false, httpconv.RequestMethodGet, "http"},
		{"BREW", false, httpconv.RequestMethodOther, "http"},
		{http.MethodPost, true, httpconv.RequestMethodPost, "https"},
	}
	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			r := httptest.NewRequest(tt.method, "/", nil)
			if tt.tls {
				r.TLS = &tls.ConnectionState{}
			}
			method, scheme := activeAttrs(r)
			if method != tt.wantMethod || scheme != tt.wantScheme {
				t.Errorf("activeAttrs = %q, %q; want %q, %q",
					method, scheme, tt.wantMethod, tt.wantScheme)
			}
		})
	}
}
