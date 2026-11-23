package endpoint

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/stakater/operator-utils/telemetry-web/internal/ident"
)

// Span opens a child span for one step of an operation and returns a finisher
// that ends it. Like Instrument's, the finisher takes the ADDRESS of the step's
// error, so a deferred call sees the final value; when it is set, the error is
// recorded and the span marked failed:
//
//	func fetch(ctx context.Context) (err error) {
//	    ctx, end := endpoint.Span(ctx, "openapi.fetch")
//	    defer end(&err)
//	    // ...
//	}
//
// The tracer is looked up on each call, like the instruments, so a span started
// after the global TracerProvider is replaced lands on the new one.
func Span(
	ctx context.Context, name string, attrs ...attribute.KeyValue,
) (context.Context, func(err *error)) {
	tracer := otel.Tracer(ident.ModulePath,
		trace.WithInstrumentationVersion(ident.Version()))
	ctx, span := tracer.Start(ctx, name, trace.WithAttributes(attrs...))
	return ctx, func(err *error) {
		if err != nil && *err != nil {
			span.RecordError(*err)
			span.SetStatus(codes.Error, (*err).Error())
		}
		span.End()
	}
}

// InstrumentSpan is Span plus Instrument under the same name: the step is both
// traced and timed into endpoint.duration.
func InstrumentSpan(
	ctx context.Context, name string, attrs ...attribute.KeyValue,
) (context.Context, func(err *error)) {
	done := Instrument(ctx, name)
	ctx, end := Span(ctx, name, attrs...)
	return ctx, func(err *error) {
		end(err)
		done(err)
	}
}
