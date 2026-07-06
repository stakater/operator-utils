package telemetry

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func TestParentBasedHonorsSampledParent(t *testing.T) {
	sampler := sdktrace.ParentBased(sdktrace.TraceIDRatioBased(0)) // never sample roots

	tid, _ := trace.TraceIDFromHex("0123456789abcdef0123456789abcdef")
	sid, _ := trace.SpanIDFromHex("0123456789abcdef")
	parent := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled, Remote: true,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), parent)

	res := sampler.ShouldSample(sdktrace.SamplingParameters{
		ParentContext: ctx, TraceID: tid, Name: "child",
	})
	if res.Decision != sdktrace.RecordAndSample {
		t.Errorf("child of sampled parent: decision = %v, want RecordAndSample", res.Decision)
	}
}
