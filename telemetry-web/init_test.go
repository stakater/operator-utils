package telemetry

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

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

// A non-gRPC protocol must NOT fail Init. The OTel Operator injects this
// variable into pods, so hard-failing would let a pod-spec change crash-loop a
// previously healthy service. Telemetry degrades, the service still boots.
func TestInitToleratesNonGRPCProtocol(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")

	shutdown, err := Init(context.Background(), Config{ServiceName: "svc", Insecure: true})
	if err != nil {
		t.Fatalf("Init must not fail on an unsupported protocol, got %v", err)
	}
	_ = shutdown(bounded(t))
}

// warnProtocol must warn only for a non-gRPC value, and must honor the OTel
// spec's per-signal-wins precedence.
func TestWarnProtocol(t *testing.T) {
	tests := []struct {
		name, generic, signal string
		wantWarn              bool
	}{
		{name: "unset is quiet"},
		{name: "generic grpc", generic: "grpc"},
		{name: "generic http warns", generic: "http/protobuf", wantWarn: true},
		{name: "signal grpc", signal: "grpc"},
		{name: "signal http warns", signal: "http/protobuf", wantWarn: true},
		// Per the OTel spec the per-signal variable wins over the generic one.
		{name: "signal grpc overrides generic http", generic: "http/protobuf", signal: "grpc"},
		{name: "signal http overrides generic grpc", generic: "grpc", signal: "http/protobuf", wantWarn: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", tt.generic)
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", tt.signal)

			var buf bytes.Buffer
			logging.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
			t.Cleanup(func() { logging.SetDefault(nil) })

			warnProtocol("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL")

			warned := strings.Contains(buf.String(), "OTLP/gRPC only")
			if warned != tt.wantWarn {
				t.Errorf("warned = %v, want %v (log: %s)", warned, tt.wantWarn, buf.String())
			}
		})
	}
}

func f(v float64) *float64 { return &v }

func TestResolveRatio(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		env  string
		want float64
	}{
		{name: "nil pointer defaults to 1.0", cfg: Config{}, want: 1.0},
		{name: "zero pointer honored", cfg: Config{SampleRatio: f(0.0)}, want: 0.0},
		{name: "non-zero pointer honored", cfg: Config{SampleRatio: f(0.25)}, want: 0.25},
		{name: "env var used when unset", cfg: Config{}, env: "0.5", want: 0.5},
		{name: "config takes precedence over env", cfg: Config{SampleRatio: f(0.75)}, env: "0.5", want: 0.75},
		{name: "unparseable env falls back to 1.0", cfg: Config{}, env: "abc", want: 1.0},
		{name: "negative env passed through", cfg: Config{}, env: "-1", want: -1},
		{name: "ratio above one passed through", cfg: Config{}, env: "2", want: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Unconditional: a dev box or CI runner that already exports
			// OTEL_TRACES_SAMPLER_ARG must not change the result.
			t.Setenv("OTEL_TRACES_SAMPLER_ARG", tt.env)
			if got := resolveRatio(tt.cfg); got != tt.want {
				t.Errorf("resolveRatio() = %v, want %v", got, tt.want)
			}
		})
	}
}
