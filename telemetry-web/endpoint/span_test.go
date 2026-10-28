package endpoint

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// recordSpans installs a tracer provider that keeps every ended span.
func recordSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	return rec
}

func TestSpan(t *testing.T) {
	boom := errors.New("boom")
	var none error
	tests := []struct {
		name       string
		err        *error
		wantStatus codes.Code
		wantEvents int
	}{
		{"nil pointer is a success", nil, codes.Unset, 0},
		{"nil error is a success", &none, codes.Unset, 0},
		{"error marks the span failed", &boom, codes.Error, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := recordSpans(t)
			parent, parentSpan := otel.Tracer("test").Start(context.Background(), "request")

			ctx, end := Span(parent, "openapi.fetch", attribute.Int("projects", 3))
			end(tt.err)
			parentSpan.End()

			spans := rec.Ended()
			if len(spans) != 2 {
				t.Fatalf("ended spans = %d, want 2", len(spans))
			}
			s := spans[0]
			if s.Name() != "openapi.fetch" {
				t.Errorf("name = %q, want openapi.fetch", s.Name())
			}
			if s.Parent().SpanID() != parentSpan.SpanContext().SpanID() {
				t.Errorf("span is not a child of the span in ctx")
			}
			if s.Status().Code != tt.wantStatus {
				t.Errorf("status = %v, want %v", s.Status().Code, tt.wantStatus)
			}
			if len(s.Events()) != tt.wantEvents {
				t.Errorf("events = %d, want %d", len(s.Events()), tt.wantEvents)
			}
			if !hasAttribute(s.Attributes(), attribute.Int("projects", 3)) {
				t.Errorf("attributes = %v, want projects=3", s.Attributes())
			}
			if trace.SpanContextFromContext(ctx).SpanID() != s.SpanContext().SpanID() {
				t.Errorf("returned ctx does not carry the new span")
			}
		})
	}
}

// One call must both trace the step and time it, with the same outcome.
func TestInstrumentSpan(t *testing.T) {
	rec := recordSpans(t)
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	resetInstruments()

	err := errors.New("boom")
	_, end := InstrumentSpan(context.Background(), "bundle.build")
	end(&err)

	spans := rec.Ended()
	if len(spans) != 1 || spans[0].Name() != "bundle.build" {
		t.Fatalf("spans = %v, want one bundle.build", spans)
	}
	if spans[0].Status().Code != codes.Error {
		t.Errorf("status = %v, want Error", spans[0].Status().Code)
	}
	if failure, _, _ := histogram(t, reader, "bundle.build", "failure"); failure != 1 {
		t.Errorf("failure = %d, want 1", failure)
	}
}

func hasAttribute(attrs []attribute.KeyValue, want attribute.KeyValue) bool {
	for _, kv := range attrs {
		if kv == want {
			return true
		}
	}
	return false
}
