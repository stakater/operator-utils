package logging

import (
	"context"
	"log/slog"
	"os"
	"sync"

	"go.opentelemetry.io/otel/trace"

	"github.com/stakater/operator-utils/telemetry-web/internal/scope"
)

// groupOrAttrs records one WithGroup or WithAttrs call so Handle can replay
// them AFTER stamping — keeping trace_id/span_id/service.name top-level even
// when the logger has open groups.
type groupOrAttrs struct {
	group string
	attrs []slog.Attr
}

type logHandler struct {
	base slog.Handler
	goas []groupOrAttrs
}

// NewLogHandler wraps base so every record carries trace_id, span_id, and
// service.name pulled from the context / scope (when present). The stamps stay
// top-level regardless of WithGroup nesting.
func NewLogHandler(base slog.Handler) slog.Handler { return &logHandler{base: base} }

func (h *logHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.base.Enabled(ctx, l)
}

func (h *logHandler) Handle(ctx context.Context, rec slog.Record) error {
	var stamps []slog.Attr
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		stamps = append(stamps,
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	if name := scope.ServiceName(); name != "" {
		stamps = append(stamps, slog.String("service.name", name))
	}

	base := h.base
	if len(stamps) > 0 {
		base = base.WithAttrs(stamps)
	}
	for _, ga := range h.goas {
		if ga.group != "" {
			base = base.WithGroup(ga.group)
		} else {
			base = base.WithAttrs(ga.attrs)
		}
	}
	return base.Handle(ctx, rec)
}

func (h *logHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	return h.with(groupOrAttrs{attrs: attrs})
}

func (h *logHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return h.with(groupOrAttrs{group: name})
}

func (h *logHandler) with(ga groupOrAttrs) slog.Handler {
	goas := make([]groupOrAttrs, len(h.goas), len(h.goas)+1)
	copy(goas, h.goas)
	return &logHandler{base: h.base, goas: append(goas, ga)}
}

var defaultLogger = sync.OnceValue(func() *slog.Logger {
	return slog.New(NewLogHandler(slog.NewJSONHandler(os.Stdout, nil)))
})

// Logger returns the shared *slog.Logger writing trace-correlated JSON to
// stdout. Use the *Context methods so ctx reaches the handler.
func Logger() *slog.Logger { return defaultLogger() }
