package publisher

import (
	"context"
	"testing"
	"time"
)

func TestNew_EmptyOperatorNameFails(t *testing.T) {
	_, err := New(context.Background(), Config{})
	if err == nil {
		t.Fatal("expected error for empty OperatorName")
	}
}

func TestNew_NoReadersSucceedsWithWarn(t *testing.T) {
	// No OTLP, no Stdout, no env vars.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "")

	p, err := New(context.Background(), Config{OperatorName: "op"})
	if err != nil {
		t.Fatalf("expected success with no readers, got %v", err)
	}
	defer p.Shutdown(context.Background())

	if p.Custom() == nil {
		t.Fatal("Custom() returned nil")
	}
}

// Construction must not block on the network even when the OTLP endpoint
// is unreachable. Real-export-time graceful degradation happens inside
// the PeriodicReader's background goroutine and is not exercised here.
func TestNew_UnreachableOTLPDoesNotBlockConstruction(t *testing.T) {
	cfg := Config{
		OperatorName: "op",
		OTLP: &OTLPConfig{
			Endpoint: "localhost:1", // guaranteed-unused
			Insecure: true,
			Timeout:  100 * time.Millisecond,
			Interval: time.Second,
		},
	}
	p, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("expected graceful degradation, got error: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestNew_EnvOnlyOTLPEnablement(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:1")
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "grpc")

	p, err := New(context.Background(), Config{OperatorName: "op"})
	if err != nil {
		t.Fatalf("expected env-only OTLP to construct, got error: %v", err)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	defer p.Shutdown(shutdownCtx)
}

func TestNew_CustomMetricSurvivesShutdown(t *testing.T) {
	p, err := New(context.Background(), Config{
		OperatorName: "op",
		Stdout:       true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctr, err := p.Custom().Counter("things_total", "counts things")
	if err != nil {
		t.Fatalf("Counter: %v", err)
	}
	ctr.Inc(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}
