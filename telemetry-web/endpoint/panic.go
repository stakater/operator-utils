package endpoint

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/stakater/operator-utils/telemetry-web/logging"
)

// RecordPanic records an exception on the active span, logs at error with
// trace_id, and increments http.server.panics. Call it from a recovery
// middleware (any framework), passing the request context and recovered value.
// It does NOT re-raise or write a response — the caller decides how to respond.
func RecordPanic(ctx context.Context, recovered any) {
	ensure()

	err := fmt.Errorf("panic: %v", recovered)
	span := trace.SpanFromContext(ctx)
	span.RecordError(err, trace.WithStackTrace(true))
	span.SetStatus(codes.Error, "panic")

	logging.Logger().ErrorContext(ctx, "recovered panic", "panic", fmt.Sprint(recovered))

	if panics != nil {
		panics.Add(ctx, 1)
	}
}
