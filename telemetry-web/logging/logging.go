package logging

import (
	"context"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel/trace"

	"github.com/stakater/operator-utils/telemetry-web/internal/scope"
)

type logHandler struct{ base slog.Handler }

// NewLogHandler wraps base so every record carries trace_id, span_id, and
// service.name pulled from the context / scope (when present).
func NewLogHandler(base slog.Handler) slog.Handler { return &logHandler{base: base} }

func (h *logHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.base.Enabled(ctx, l)
}

func (h *logHandler) Handle(ctx context.Context, rec slog.Record) error {
	sc := trace.SpanContextFromContext(ctx)
	name := scope.ServiceName()
	if sc.IsValid() || name != "" {
		rec = rec.Clone()
	}
	if sc.IsValid() {
		rec.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	if name != "" {
		rec.AddAttrs(slog.String("service.name", name))
	}
	return h.base.Handle(ctx, rec)
}

func (h *logHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &logHandler{base: h.base.WithAttrs(attrs)}
}

func (h *logHandler) WithGroup(name string) slog.Handler {
	return &logHandler{base: h.base.WithGroup(name)}
}

// Logger returns a *slog.Logger writing trace-correlated JSON to stdout. Use the
// *Context methods so ctx reaches the handler.
func Logger() *slog.Logger {
	return slog.New(NewLogHandler(slog.NewJSONHandler(os.Stdout, nil)))
}
