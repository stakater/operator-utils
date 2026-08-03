package endpoint

import (
	"context"
	"fmt"
	"net/http"
	"runtime/debug"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/stakater/operator-utils/telemetry-web/logging"
)

// Recovered records a recovered panic and reports whether the caller may respond
// normally. It returns false for http.ErrAbortHandler, which the caller must
// re-panic: that sentinel is net/http's "drop this connection without a
// response" signal, not an error.
//
// attrs are attached to http.server.panics; pass the matched route template when
// the caller knows it.
func Recovered(ctx context.Context, recovered any, attrs ...attribute.KeyValue) bool {
	// Sentinel arrives by identity, never wrapped.
	if recovered == http.ErrAbortHandler { //nolint:errorlint
		return false
	}
	RecordPanic(ctx, recovered, attrs...)
	return true
}

// RecordPanic records an exception on the active span, logs at error with
// trace_id and a stack, and increments http.server.panics. It does NOT re-raise
// or write a response, and does not filter http.ErrAbortHandler; prefer
// Recovered, which does both.
//
// attrs land on the counter. Pass the matched route template so a spike can be
// traced to an endpoint. Nothing is derived from the request here, since a raw
// path or method would blow up the time series count.
func RecordPanic(ctx context.Context, recovered any, attrs ...attribute.KeyValue) {
	// Built by hand rather than via span.RecordError: that takes exception.type
	// from reflect.TypeOf, so every panic would report errors.errorString, and its
	// stack capture uses a fixed 2 KB buffer that truncates deep stacks.
	stack := string(debug.Stack())
	message := fmt.Sprint(recovered)

	span := trace.SpanFromContext(ctx)
	span.AddEvent(semconv.ExceptionEventName, trace.WithAttributes(
		semconv.ExceptionType(fmt.Sprintf("%T", recovered)),
		semconv.ExceptionMessage(message),
		semconv.ExceptionStacktrace(stack),
	))
	span.SetStatus(codes.Error, "panic")

	// Also on the log, since the span is lost when tracing is off, the trace is
	// unsampled, or the collector is unreachable.
	logging.Logger().ErrorContext(ctx, "recovered panic",
		"panic", message,
		"stack", stack)

	if c := get().panics; c != nil {
		c.Add(ctx, 1, metric.WithAttributes(attrs...))
	}
}
