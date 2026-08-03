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

// Recovered decides what a recovery middleware should do with a recovered value,
// and records the telemetry when there is any to record. It returns false for
// http.ErrAbortHandler, which the caller must re-panic: that sentinel is
// net/http's "drop this connection without a response" signal, not an error.
//
// Every framework's recovery reduces to the same three lines, so the rule lives
// here once instead of in each adapter:
//
//	defer func() {
//	    if rec := recover(); rec != nil {
//	        if !endpoint.Recovered(ctx, rec) {
//	            panic(rec)
//	        }
//	        // ... respond 500 the framework's way ...
//	    }
//	}()
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
// trace_id and a stack, and increments http.server.panics. Call it from a
// recovery middleware (any framework), passing the request context and recovered
// value. It does NOT re-raise or write a response — the caller decides how to
// respond, and does not filter http.ErrAbortHandler either; prefer Recovered,
// which does both.
//
// attrs land on the counter. Pass the matched route template so a spike can be
// traced to an endpoint. Anything unbounded (a raw path, a raw method) would
// blow up the time series count, so nothing is derived from the request here.
func RecordPanic(ctx context.Context, recovered any, attrs ...attribute.KeyValue) {
	// Captured once and shared. The exception event is built by hand rather than
	// via span.RecordError for two reasons: RecordError takes exception.type from
	// reflect.TypeOf, so wrapping the value in fmt.Errorf would report
	// errors.errorString for every panic in the fleet and make "which class of
	// panic is spiking" unanswerable; and its own stack capture uses a fixed 2 KB
	// buffer with no growth loop, which cuts a deep middleware stack mid-line.
	stack := string(debug.Stack())
	message := fmt.Sprint(recovered)

	span := trace.SpanFromContext(ctx)
	span.AddEvent(semconv.ExceptionEventName, trace.WithAttributes(
		semconv.ExceptionType(fmt.Sprintf("%T", recovered)),
		semconv.ExceptionMessage(message),
		semconv.ExceptionStacktrace(stack),
	))
	span.SetStatus(codes.Error, "panic")

	// The stack goes on the log as well as the span. On the span alone it is lost
	// whenever tracing is off, the sampler dropped the trace, or the collector is
	// unreachable, and a panic is the one event where the stack is the whole
	// point.
	logging.Logger().ErrorContext(ctx, "recovered panic",
		"panic", message,
		"stack", stack)

	if c := get().panics; c != nil {
		c.Add(ctx, 1, metric.WithAttributes(attrs...))
	}
}
