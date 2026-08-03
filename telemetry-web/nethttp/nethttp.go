// Package nethttp provides net/http integration for the telemetry library:
// an inbound Handler (spans + server metrics + panic recovery) and an outbound
// Transport that propagates trace context.
package nethttp

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"sync"

	"github.com/felixge/httpsnoop"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/stakater/operator-utils/telemetry-web/endpoint"
	"github.com/stakater/operator-utils/telemetry-web/logging"
)

// Option configures Handler.
type Option func(*config)

type config struct {
	skipPaths       []string
	noRecover       bool
	endpointMetrics bool
}

// Settings is the resolved option set. Adapters call Resolve to learn which
// middleware their Instrument should install.
type Settings struct {
	// Recovery reports whether the built-in recovery middleware is installed.
	Recovery bool
	// EndpointMetrics reports whether WithEndpointMetrics was requested.
	EndpointMetrics bool
}

// Resolve applies opts and reports the result, so an adapter and the Handler
// it builds agree on one configuration.
func Resolve(opts ...Option) Settings {
	cfg := config{}
	for _, opt := range opts {
		opt(&cfg)
	}
	return Settings{
		Recovery:        !cfg.noRecover,
		EndpointMetrics: cfg.endpointMetrics,
	}
}

// WithEndpointMetrics opts in to the endpoint.requests counter in an adapter's
// Instrument. Off by default: once StampRoute runs, http.server.request.duration
// already carries route, method, and status, making it a strict superset. Turn
// this on when you want the simpler {endpoint,outcome} label pair.
func WithEndpointMetrics() Option {
	return func(c *config) { c.endpointMetrics = true }
}

// DefaultSkipPaths are the usual noise endpoints — k8s health probes and the
// Prometheus scrape path. Handler does NOT skip them on its own; opt in with
//
//	nethttp.Handler(mux, nethttp.WithSkipPaths(nethttp.DefaultSkipPaths...))
//
// or append your own: WithSkipPaths(append(nethttp.DefaultSkipPaths, "/ping")...).
var DefaultSkipPaths = []string{"/healthz", "/readyz", "/livez", "/metrics"}

// WithSkipPaths excludes the given exact request paths from instrumentation: no
// span, no http.server.* metrics, and no endpoint.requests. The request is still
// served and recovery still applies. Repeated calls accumulate; by default
// nothing is skipped.
//
// The filter runs before trace context is extracted, so a skipped path also
// drops any inbound traceparent and will not continue a caller's trace. Fine for
// probes, wrong for paths that participate in traces.
func WithSkipPaths(paths ...string) Option {
	return func(c *config) { c.skipPaths = append(c.skipPaths, paths...) }
}

// WithoutRecovery installs no recovery at all — not Handler's, and not the
// framework middleware an adapter's Instrument would add, since that consults
// Resolve too. Only pass it when an outer layer recovers and calls
// endpoint.RecordPanic itself.
//
// A panic then escapes otelhttp, which costs more than the panic counter: its only
// deferred work is span.End(), so the status, the response attributes, the span
// rename and the metric record are all skipped. The span ends Unset with no
// response attributes and the request contributes NO http.server.* metrics at all,
// which makes a service panicking on every request look like it is serving zero
// traffic. The same applies on the ErrAbortHandler re-raise path.
func WithoutRecovery() Option {
	return func(c *config) { c.noRecover = true }
}

// skipKey marks a request that WithSkipPaths excluded, so middleware running
// inside the router (which otelhttp's own filter cannot reach) can bail out too.
type skipKey struct{}

// skipped reports whether this request's path was excluded via WithSkipPaths.
// Unexported: RecordRoute and StampRoute consult it for their callers, so nothing
// outside this package ever needs to ask.
func skipped(ctx context.Context) bool {
	v, _ := ctx.Value(skipKey{}).(bool)
	return v
}

// Handler is the composed inbound chain: otelhttp (spans + metrics) -> recovery
// -> next. Frameworks that don't expose http.Handler chaining should instead
// call endpoint.RecordPanic and endpoint.Record from their own middleware.
// Every path is instrumented unless excluded via WithSkipPaths.
func Handler(next http.Handler, opts ...Option) http.Handler {
	// The config directly, not via Resolve: Handler is the one caller that needs
	// the skip paths themselves, and Settings deliberately does not carry them.
	cfg := config{}
	for _, opt := range opts {
		opt(&cfg)
	}

	inner := next
	if !cfg.noRecover {
		inner = Recovery(next)
	}

	// otelhttp reads the globals in its own defaults, so passing them here
	// would only restate them.
	instrumented := otelhttp.NewHandler(inner, "server",
		otelhttp.WithFilter(func(r *http.Request) bool { return !skipped(r.Context()) }),
	)

	if len(cfg.skipPaths) == 0 {
		return instrumented
	}
	skip := make(map[string]bool, len(cfg.skipPaths))
	for _, p := range cfg.skipPaths {
		skip[p] = true
	}
	// Marking outside otelhttp is what lets both its filter and the adapters'
	// middleware see the same decision.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if skip[r.URL.Path] {
			r = r.WithContext(context.WithValue(r.Context(), skipKey{}, true))
		}
		instrumented.ServeHTTP(w, r)
	})
}

// trackWrites returns w wrapped so responded reports whether anything has been
// committed to the client yet.
//
// httpsnoop is used rather than an embedded http.ResponseWriter because it
// re-exposes exactly the optional interfaces w already implements. Embedding
// would promote only Header/Write/WriteHeader, and gin's and echo's Flush
// type-assert for Flusher — streaming handlers would panic outright.
func trackWrites(w http.ResponseWriter) (wrapped http.ResponseWriter, responded func() bool) {
	var wrote bool
	wrapped = httpsnoop.Wrap(w, httpsnoop.Hooks{
		WriteHeader: func(next httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
			return func(code int) { wrote = true; next(code) }
		},
		Write: func(next httpsnoop.WriteFunc) httpsnoop.WriteFunc {
			return func(b []byte) (int, error) { wrote = true; return next(b) }
		},
		// A hijacked connection is no longer ours to write a status to.
		Hijack: func(next httpsnoop.HijackFunc) httpsnoop.HijackFunc {
			return func() (net.Conn, *bufio.ReadWriter, error) { wrote = true; return next() }
		},
		// Flush and ReadFrom also commit an implicit 200 in net/http, and
		// httpsnoop.Wrap passes unhooked methods straight through — so without
		// these, io.Copy(w, src) or an SSE handler that flushes its headers leaves
		// wrote false and a later panic makes Recovery call WriteHeader(500)
		// pointlessly. otelhttp tracks the real status either way, so the only
		// symptom is a "superfluous response.WriteHeader" line from net/http, but
		// there is no reason to emit it.
		Flush: func(next httpsnoop.FlushFunc) httpsnoop.FlushFunc {
			return func() { wrote = true; next() }
		},
		ReadFrom: func(next httpsnoop.ReadFromFunc) httpsnoop.ReadFromFunc {
			return func(src io.Reader) (int64, error) { wrote = true; return next(src) }
		},
	})
	return wrapped, func() bool { return wrote }
}

// Recovery recovers panics, records telemetry via endpoint.RecordPanic, and
// writes 500 if nothing has been written yet. http.ErrAbortHandler is
// re-raised. Must sit inside the otelhttp handler so the span exists in ctx and
// the response is measured. The writer handed to next keeps whatever optional
// interfaces the original had, so SSE and hijacking keep working.
func Recovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tracked, responded := trackWrites(w)
		defer func() {
			if p := recover(); p != nil {
				ctx := r.Context()
				if !endpoint.Recovered(ctx, p, routeAttrs(ctx)...) {
					panic(p)
				}
				// A handler that panicked mid-stream already sent its status;
				// a second write would only be log noise.
				if !responded() {
					tracked.WriteHeader(http.StatusInternalServerError)
				}
			}
		}()
		next.ServeHTTP(tracked, r)
	})
}

// Transport wraps a RoundTripper so outbound requests inject trace context.
func Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return otelhttp.NewTransport(base)
}

// HTTPClient returns a client whose Transport already propagates trace context.
func HTTPClient() *http.Client { return &http.Client{Transport: Transport(nil)} }

// WrapClient adds propagation to an existing client, in place, and returns it so
// it can be used inline. Calling it twice on the same client would nest one
// otelhttp transport inside another and inject the headers twice, so a client
// that is already wrapped is left alone.
func WrapClient(c *http.Client) *http.Client {
	if _, ok := c.Transport.(*otelhttp.Transport); !ok {
		c.Transport = Transport(c.Transport)
	}
	return c
}

// routeAttrs recovers http.route from the otelhttp labeler, where StampRoute put
// it. Recovery runs outside the router, so at recover time the labeler is the
// only place the matched template can still be read from.
func routeAttrs(ctx context.Context) []attribute.KeyValue {
	labeler, ok := otelhttp.LabelerFromContext(ctx)
	if !ok {
		return nil
	}
	for _, kv := range labeler.Get() {
		if kv.Key == semconv.HTTPRouteKey {
			return []attribute.KeyValue{kv}
		}
	}
	return nil
}

// RecordRoute emits one endpoint.requests data point for a matched route. It is
// the single definition of "failure" every adapter shares: status >= 500. A 4xx
// is the caller's fault, so it counts as a success.
//
// Unmatched routes (empty template) are skipped to bound 404-scan cardinality,
// and WithSkipPaths exclusions are honored here too, not just in otelhttp.
//
// Adapters call this from their Metrics middleware; call it directly when
// wiring a framework by hand.
func RecordRoute(ctx context.Context, route string, status int) {
	if route == "" || skipped(ctx) {
		return
	}
	endpoint.Record(ctx, route, status >= 500)
}

// warnNoLabeler fires at most once per process: the condition is a wiring
// mistake, not a per-request event. Deliberately NOT reset when Init installs new
// providers, unlike endpoint's instrument warning — whether a labeler reaches
// StampRoute depends on how the chain was built, which no re-Init changes, so
// re-arming it could only repeat the same message.
var warnNoLabeler sync.Once

// StampRoute puts http.route on the active server span and on the otelhttp
// metric attributes (via the request Labeler), and renames the span to the
// semconv "{method} {route}" form. An empty method leaves the name alone. The
// adapters call it from RouteTag; call it yourself when wiring a framework by
// hand.
//
// The request must be inside Handler. Without the otelhttp labeler in ctx the
// span attribute still lands but the metric one is dropped, and a warning is
// logged once.
//
// A path excluded via WithSkipPaths returns immediately. otelhttp's filter
// short-circuits before it injects the labeler, so a skipped request has neither
// a labeler nor a recording span and there is nothing here to stamp — but the
// adapters' RouteTag still runs, because a skip path is a registered route with a
// real template. Without this the recommended
// WithSkipPaths(DefaultSkipPaths...) setup would warn about a wiring mistake on
// the first health probe.
func StampRoute(ctx context.Context, method, route string) {
	if skipped(ctx) {
		return
	}
	attr := semconv.HTTPRoute(route)
	labeler, ok := otelhttp.LabelerFromContext(ctx)
	if !ok {
		warnNoLabeler.Do(func() {
			logging.Logger().Warn("nethttp.StampRoute: no otelhttp labeler in context; " +
				"http.route will not reach http.server.* metrics. Serve through nethttp.Handler.")
		})
	}
	labeler.Add(attr)
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(attr)
	if method != "" {
		span.SetName(method + " " + route)
	}
}
