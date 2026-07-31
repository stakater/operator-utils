// Package logging provides the trace-correlated slog handler the library logs
// through, and the injection point for replacing it.
package logging

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"

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
	base    slog.Handler   // as handed to NewLogHandler, no goas applied
	applied slog.Handler   // base with every goas already applied
	goas    []groupOrAttrs // replay list, only needed once a group is open
	grouped bool           // true once any WithGroup has been called
}

// NewLogHandler wraps base so every record carries trace_id, span_id, and
// service.name pulled from the context / scope (when present). The stamps stay
// top-level regardless of WithGroup nesting.
func NewLogHandler(base slog.Handler) slog.Handler {
	return &logHandler{base: base, applied: base}
}

func (h *logHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.applied.Enabled(ctx, l)
}

// stamps returns the correlation attrs for ctx, or nil when there is nothing
// to add.
func stamps(ctx context.Context) []slog.Attr {
	var out []slog.Attr
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		out = append(out,
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	if name := scope.ServiceName(); name != "" {
		out = append(out, slog.String("service.name", name))
	}
	return out
}

func (h *logHandler) Handle(ctx context.Context, rec slog.Record) error {
	st := stamps(ctx)
	if len(st) == 0 {
		return h.applied.Handle(ctx, rec)
	}
	// Fast path: with no group open, record-level attrs already land
	// top-level, so the pre-applied handler can be reused as-is. Only an open
	// group would capture the stamps, and that is the rare case.
	if !h.grouped {
		rec = rec.Clone()
		rec.AddAttrs(st...)
		return h.applied.Handle(ctx, rec)
	}
	// Slow path: stamp the base handler first, then reopen the groups on top.
	base := h.base.WithAttrs(st)
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
	goas = append(goas, ga)

	applied := h.applied
	if ga.group != "" {
		applied = applied.WithGroup(ga.group)
	} else {
		applied = applied.WithAttrs(ga.attrs)
	}
	return &logHandler{
		base:    h.base,
		applied: applied,
		goas:    goas,
		grouped: h.grouped || ga.group != "",
	}
}

var fallback = sync.OnceValue(func() *slog.Logger {
	return slog.New(NewLogHandler(slog.NewJSONHandler(os.Stdout, nil)))
})

var current atomic.Pointer[slog.Logger]

// SetDefault replaces the logger this library writes through — endpoint
// metrics warnings, recovered panics, and setup diagnostics. Call it before
// telemetry.Init so a consuming operator gets one log stream in its own
// format instead of a second one on stdout. Passing nil restores the default.
//
// Wrap your handler to keep trace correlation:
//
//	logging.SetDefault(slog.New(logging.NewLogHandler(myHandler)))
func SetDefault(l *slog.Logger) { current.Store(l) }

// Logger returns the logger the library writes through: whatever SetDefault
// installed, else a shared trace-correlated JSON logger on stdout. Use the
// *Context methods so ctx reaches the handler.
func Logger() *slog.Logger {
	if l := current.Load(); l != nil {
		return l
	}
	return fallback()
}
