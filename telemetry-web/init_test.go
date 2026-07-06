package telemetry

import (
	"context"
	"testing"
	"time"
)

func TestInitRequiresServiceName(t *testing.T) {
	if _, err := Init(context.Background(), Config{}); err == nil {
		t.Fatal("expected error when ServiceName is empty, got nil")
	}
}

func TestInitSucceedsAndReturnsShutdown(t *testing.T) {
	shutdown, err := Init(context.Background(), Config{ServiceName: "test-svc", Insecure: true})
	if err != nil {
		t.Fatalf("Init returned error: %v", err)
	}
	if shutdown == nil {
		t.Fatal("Init returned nil shutdown func")
	}

	sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = shutdown(sctx)
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.env != "" {
				t.Setenv("OTEL_TRACES_SAMPLER_ARG", tt.env)
			}
			if got := resolveRatio(tt.cfg); got != tt.want {
				t.Errorf("resolveRatio() = %v, want %v", got, tt.want)
			}
		})
	}
}
