package telemetry

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/stakater/operator-utils/telemetry-web/endpoint"
	"github.com/stakater/operator-utils/telemetry-web/internal/rebind"
	"github.com/stakater/operator-utils/telemetry-web/internal/scope"
	"github.com/stakater/operator-utils/telemetry-web/logging"
)

func TestInitRequiresServiceName(t *testing.T) {
	if _, err := Init(context.Background(), Config{}); err == nil {
		t.Fatal("expected error when ServiceName is empty, got nil")
	}
}

// initOnce runs Init and returns a shutdown the test must call, keeping the
// package-level initialized guard from leaking into other tests.
func initOnce(t *testing.T) func(context.Context) error {
	t.Helper()
	shutdown, err := Init(context.Background(), Config{ServiceName: "test-svc", Insecure: true})
	if err != nil {
		t.Fatalf("Init returned error: %v", err)
	}
	if shutdown == nil {
		t.Fatal("Init returned nil shutdown func")
	}
	return shutdown
}

func bounded(t *testing.T) context.Context {
	t.Helper()
	// Short: with no collector listening, shutdown blocks on the flush until
	// this deadline. The flush error is expected and ignored.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	t.Cleanup(cancel)
	return ctx
}

func TestInitSucceedsAndReturnsShutdown(t *testing.T) {
	shutdown := initOnce(t)
	_ = shutdown(bounded(t))
}

// A second Init without an intervening shutdown would orphan the first pair of
// providers — their batch processor goroutine and reader ticker become
// unreachable — and would register the runtime callbacks twice.
func TestInitTwiceIsRefused(t *testing.T) {
	shutdown := initOnce(t)
	defer func() { _ = shutdown(bounded(t)) }()

	_, err := Init(context.Background(), Config{ServiceName: "second", Insecure: true})
	if !errors.Is(err, ErrAlreadyInitialized) {
		t.Fatalf("second Init error = %v, want ErrAlreadyInitialized", err)
	}
}

// After shutdown the guard clears, so a process (or a test) can initialize again.
func TestInitAgainAfterShutdown(t *testing.T) {
	shutdown := initOnce(t)
	_ = shutdown(bounded(t)) // flush fails without a collector; the guard still clears
	second := initOnce(t)
	_ = second(bounded(t))
}

// Consumers defer shutdown and sometimes also call it explicitly; the second
// call must be a no-op returning the same result, not a double shutdown.
func TestShutdownIsIdempotent(t *testing.T) {
	shutdown := initOnce(t)
	first := shutdown(bounded(t))
	if second := shutdown(bounded(t)); !errors.Is(second, first) {
		t.Fatalf("second shutdown = %v, want the memoized %v", second, first)
	}
}

// The OTel Operator injects OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf into pods,
// which is the single most common deployment. Init must boot and pick the HTTP
// exporter, silently.
func TestInitHonorsHTTPProtocol(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")

	var buf bytes.Buffer
	logging.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { logging.SetDefault(nil) })

	shutdown, err := Init(context.Background(), Config{ServiceName: "svc", Insecure: true})
	if err != nil {
		t.Fatalf("Init must not fail on http/protobuf, got %v", err)
	}
	_ = shutdown(bounded(t))

	if strings.Contains(buf.String(), "OTLP") {
		t.Errorf("a supported protocol must not warn, got: %s", buf.String())
	}
}

// resolveProtocol picks the transport per signal, honors the spec's
// per-signal-wins precedence, and warns only when it cannot do what was asked.
func TestResolveProtocol(t *testing.T) {
	tests := []struct {
		name, generic, signal string
		want                  string
		wantWarn              bool
	}{
		// Unset resolves to gRPC, not the spec default, so existing pipelines
		// keep working. See resolveProtocol.
		{name: "unset defaults to grpc", want: protoGRPC},
		{name: "generic grpc", generic: "grpc", want: protoGRPC},
		{name: "generic http", generic: "http/protobuf", want: protoHTTP},
		{name: "signal http", signal: "http/protobuf", want: protoHTTP},
		{name: "whitespace tolerated", generic: "  http/protobuf  ", want: protoHTTP},
		// Per the OTel spec the per-signal variable wins over the generic one.
		{name: "signal grpc overrides generic http", generic: "http/protobuf", signal: "grpc", want: protoGRPC},
		{name: "signal http overrides generic grpc", generic: "grpc", signal: "http/protobuf", want: protoHTTP},
		// No JSON encoder in the Go SDK, so this downgrades to protobuf on the
		// same HTTP endpoint rather than to a different port.
		{name: "http/json warns and uses http", generic: "http/json", want: protoHTTP, wantWarn: true},
		{name: "garbage warns and uses grpc", generic: "thrift", want: protoGRPC, wantWarn: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", tt.generic)
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", tt.signal)

			var buf bytes.Buffer
			logging.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
			t.Cleanup(func() { logging.SetDefault(nil) })

			if got := resolveProtocol("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"); got != tt.want {
				t.Errorf("resolveProtocol = %q, want %q", got, tt.want)
			}
			if warned := strings.Contains(buf.String(), "OTLP"); warned != tt.wantWarn {
				t.Errorf("warned = %v, want %v (log: %s)", warned, tt.wantWarn, buf.String())
			}
		})
	}
}

func f(v float64) *float64 { return &v }

func TestResolveRatio(t *testing.T) {
	tests := []struct {
		name     string
		cfg      Config
		env      string
		want     float64
		wantWarn bool
	}{
		{name: "nil pointer defaults to 1.0", cfg: Config{}, want: 1.0},
		{name: "zero pointer honored", cfg: Config{SampleRatio: f(0.0)}, want: 0.0},
		{name: "non-zero pointer honored", cfg: Config{SampleRatio: f(0.25)}, want: 0.25},
		{name: "env var used when unset", cfg: Config{}, env: "0.5", want: 0.5},
		{name: "config takes precedence over env", cfg: Config{SampleRatio: f(0.75)}, env: "0.5", want: 0.75},
		{name: "whitespace trimmed", cfg: Config{}, env: "  0.5  ", want: 0.5},
		// A typo must be loud. Falling back to 1.0 silently turns a sampling
		// config into full-volume export, which costs money rather than nothing.
		{name: "unparseable env warns and falls back to 1.0", cfg: Config{}, env: "0,01", want: 1.0, wantWarn: true},
		{name: "unparseable text warns", cfg: Config{}, env: "abc", want: 1.0, wantWarn: true},
		// TraceIDRatioBased clamps these, so the behaviour is fine and the value
		// passes through; the config is still wrong and worth saying so.
		{name: "negative env warns, passed through", cfg: Config{}, env: "-1", want: -1, wantWarn: true},
		{name: "ratio above one warns, passed through", cfg: Config{}, env: "2", want: 2, wantWarn: true},
		// An explicit out-of-range Config value is the caller's own doing and is
		// not second-guessed here.
		{name: "config value not range checked", cfg: Config{SampleRatio: f(2)}, want: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Unconditional: a dev box or CI runner that already exports
			// OTEL_TRACES_SAMPLER_ARG must not change the result.
			t.Setenv("OTEL_TRACES_SAMPLER_ARG", tt.env)

			var buf bytes.Buffer
			logging.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
			t.Cleanup(func() { logging.SetDefault(nil) })

			if got := resolveRatio(tt.cfg); got != tt.want {
				t.Errorf("resolveRatio() = %v, want %v", got, tt.want)
			}
			if warned := strings.Contains(buf.String(), "sampler argument"); warned != tt.wantWarn {
				t.Errorf("warned = %v, want %v (log: %s)", warned, tt.wantWarn, buf.String())
			}
		})
	}
}

// A failed Init must leave the process on noop providers, not on the ones it
// already installed and then shut down. Callers overwhelmingly log a telemetry
// error and keep serving, so the difference is between a service that records
// nothing and a service wired to a dead pipeline.
func TestInitFailureRestoresNoopProviders(t *testing.T) {
	// The HTTP metric exporter validates its endpoint eagerly and the trace
	// exporter does not, so this fails at newMetricReader — after
	// SetTracerProvider has already installed tp. That is the ordering that used
	// to strand a shut-down provider on the global.
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")
	shutdown, err := Init(context.Background(), Config{
		ServiceName:  "svc",
		OTLPEndpoint: "http://a b c",
	})
	if err == nil {
		_ = shutdown(bounded(t))
		t.Fatal("Init succeeded on a malformed endpoint, want an error")
	}
	if shutdown != nil {
		t.Error("a failed Init must not return a shutdown func")
	}

	// Asserted on the concrete type, not on IsRecording: a shut-down SDK provider
	// also reports non-recording, so only the type distinguishes "restored to
	// noop" from "still holding the corpse".
	if got := otel.GetTracerProvider(); !isNoopTracerProvider(got) {
		t.Errorf("global TracerProvider is %T after a failed Init, want the noop", got)
	}
	if got := otel.GetMeterProvider(); !isNoopMeterProvider(got) {
		t.Errorf("global MeterProvider is %T after a failed Init, want the noop", got)
	}
	if name := scope.ServiceName(); name != "" {
		t.Errorf("log scope = %q after a failed Init, want it cleared", name)
	}

	// The guard must not be left latched, or one failed Init would make every
	// later attempt return ErrAlreadyInitialized.
	second, err := Init(context.Background(), Config{ServiceName: "svc", Insecure: true})
	if err != nil {
		t.Fatalf("Init after a failure returned %v, want it to succeed", err)
	}
	_ = second(bounded(t))
}

func isNoopTracerProvider(tp trace.TracerProvider) bool {
	_, ok := tp.(tracenoop.TracerProvider)
	return ok
}

func isNoopMeterProvider(mp metric.MeterProvider) bool {
	_, ok := mp.(metricnoop.MeterProvider)
	return ok
}

// SDK-internal failures (export refused, queue full, spans dropped) must reach
// the consumer's logger. Without an error handler they go to otel's default,
// which prints to stderr — the second, differently formatted log stream that
// logging.SetDefault exists to prevent.
func TestInitRoutesSDKErrorsToTheLogger(t *testing.T) {
	var buf bytes.Buffer
	logging.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { logging.SetDefault(nil) })

	shutdown := initOnce(t)
	defer func() { _ = shutdown(bounded(t)) }()

	otel.Handle(errors.New("exporter refused the batch"))

	out := buf.String()
	if !strings.Contains(out, "exporter refused the batch") {
		t.Errorf("SDK error did not reach the logger: %s", out)
	}
	if !strings.Contains(out, "sdk error") {
		t.Errorf("SDK error not labelled as such: %s", out)
	}
}

// Init must trigger the instrument rebind. otel's global meter never
// re-delegates, so anything caching instruments (the endpoint package) would
// otherwise stay bound to whichever provider was global before this call —
// stranding endpoint.requests, endpoint.duration, and http.server.panics after
// a re-Init.
func TestInitNotifiesRebind(t *testing.T) {
	var called int
	rebind.On(func() { called++ })

	shutdown := initOnce(t)
	defer func() { _ = shutdown(bounded(t)) }()

	if called != 1 {
		t.Errorf("rebind hook ran %d times during Init, want 1", called)
	}
}

// Shutdown must stop the cached instruments writing into the provider it just
// retired. Swapping the globals to noop is not enough on its own: instruments
// that already exist never re-resolve their provider, so shutdown has to notify
// the rebind hooks the same way Init does.
func TestShutdownStopsWritesToRetiredProvider(t *testing.T) {
	retired := sdkmetric.NewManualReader()

	shutdown := initOnce(t)
	// Stand in for Init's provider with one we can read.
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(retired)))
	rebind.Notify()

	endpoint.Record(context.Background(), "probe", false)
	live := endpointPoints(t, retired)
	if live == 0 {
		t.Fatal("setup: nothing recorded into the provider before shutdown")
	}

	_ = shutdown(bounded(t))

	endpoint.Record(context.Background(), "probe", false)
	if got := endpointPoints(t, retired); got != live {
		t.Errorf("retired provider received %d points after shutdown, want it frozen at %d", got, live)
	}
}

func endpointPoints(t *testing.T, r *sdkmetric.ManualReader) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := r.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "endpoint.requests" {
				continue
			}
			if sum, ok := m.Data.(metricdata.Sum[int64]); ok {
				for _, dp := range sum.DataPoints {
					total += dp.Value
				}
			}
		}
	}
	return total
}
